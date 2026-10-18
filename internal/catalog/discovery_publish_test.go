package catalog

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/observability"
	"next-game-crush/internal/steam"
)

type blockingDiscoveryAI struct {
	repeatingContentClient
	stage                  string
	started                chan struct{}
	release                chan struct{}
	startOnce, releaseOnce sync.Once
	mu                     sync.Mutex
	featureJobs            []uuid.UUID
}

func (c *blockingDiscoveryAI) unblock() { c.releaseOnce.Do(func() { close(c.release) }) }
func (c *blockingDiscoveryAI) block(ctx context.Context, stage, content string) error {
	if c.stage != stage || content != "Content A" {
		return nil
	}
	c.startOnce.Do(func() { close(c.started) })
	select {
	case <-c.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *blockingDiscoveryAI) ExtractGameFeatures(ctx context.Context, evidence ai.GameEvidence) (ai.GameFeatures, error) {
	c.mu.Lock()
	c.featureJobs = append(c.featureJobs, observability.CorrelationFromContext(ctx).JobID)
	c.mu.Unlock()
	for _, item := range evidence.Evidence {
		if item.Key == "description" {
			if err := c.block(ctx, "features", item.Value); err != nil {
				return ai.GameFeatures{}, err
			}
		}
	}
	return c.repeatingContentClient.ExtractGameFeatures(ctx, evidence)
}
func (c *blockingDiscoveryAI) Embed(ctx context.Context, input []string) ([][]float32, error) {
	content := ""
	if len(input) > 0 {
		for _, line := range []string{"Content A", "Content B"} {
			if containsLine(input[0], line) {
				content = line
			}
		}
	}
	if err := c.block(ctx, "embedding", content); err != nil {
		return nil, err
	}
	return c.repeatingContentClient.Embed(ctx, input)
}
func containsLine(value, line string) bool {
	for _, part := range strings.Split(value, "\n") {
		if part == line {
			return true
		}
	}
	return false
}

func TestDiscoveryEnrichmentPublicationFencesObsoleteExecutions(t *testing.T) {
	for _, stage := range []string{"features", "embedding", "queued"} {
		t.Run(stage, func(t *testing.T) {
			s, metadata := discoveryFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			client := &blockingDiscoveryAI{stage: stage, started: make(chan struct{}), release: make(chan struct{})}
			enrichment, err := ai.NewService(ai.NewSQLRepository(s.repo.store), client, "text-v1", "embedding-v1")
			if err != nil {
				t.Fatal(err)
			}
			enrichment.SetQueue(s.queue)
			var stalePublication atomic.Bool
			enrichment.SetCatalogChangedHook(func(ctx context.Context, id uuid.UUID) error {
				var stale bool
				err := s.repo.store.Pool.QueryRow(ctx, `SELECT (g.source_snapshot->>'ShortDescription'='Content B') AND COALESCE(f.features->'themes'->>0='content-a',false) FROM games g LEFT JOIN game_features f ON f.game_id=g.id AND f.model='text-v1' WHERE g.id=$1`, id).Scan(&stale)
				if stale {
					stalePublication.Store(true)
				}
				return err
			})
			s.SetEnrichmentPlanner(enrichment)
			now := time.Now().UTC().Truncate(time.Second)
			metadata.details.ShortDescription = "Content A"
			first := leaseDiscoveryID(t, s, enqueueQualification(t, s, DiscoveredApp{AppID: 42, LastModified: now}, 0))
			if err = s.handleQualification(ctx, first); err != nil {
				t.Fatal(err)
			}
			if err = s.queue.Complete(ctx, first); err != nil {
				t.Fatal(err)
			}
			// Reserve B's qualification lease before the AI workers can claim it.
			second := leaseDiscoveryID(t, s, enqueueQualification(t, s, DiscoveredApp{AppID: 42, LastModified: now.Add(time.Second)}, 0))
			qualifyB := func() {
				t.Helper()
				metadata.details.ShortDescription = "Content B"
				if err := s.handleQualification(ctx, second); err != nil {
					t.Fatal(err)
				}
				if err := s.queue.Complete(ctx, second); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "queued" {
				qualifyB()
			}
			worker := jobs.NewWorker(s.queue, "publication-fence", jobs.WithConcurrency(2), jobs.WithPollInterval(time.Millisecond))
			if err = enrichment.RegisterHandlers(worker); err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { finished <- worker.Run(ctx) }()
			defer func() {
				client.unblock()
				cancel()
				if err := <-finished; err != nil {
					t.Error(err)
				}
			}()
			if stage != "queued" {
				select {
				case <-client.started:
				case <-ctx.Done():
					t.Fatal("A did not reach its controlled remote call")
				}
				qualifyB()
			}
			var secondJob uuid.UUID
			if err = s.repo.store.Pool.QueryRow(ctx, `SELECT id FROM sync_jobs WHERE kind='ai.features' AND split_part(idempotency_key,':',5)='2'`).Scan(&secondJob); err != nil {
				t.Fatal(err)
			}
			wait := func(condition func() bool) {
				t.Helper()
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for !condition() {
					select {
					case <-ticker.C:
					case <-ctx.Done():
						t.Fatal("enrichment did not reach the required state")
					}
				}
			}
			wait(func() bool {
				var status string
				if err := s.repo.store.Pool.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE id=$1`, secondJob).Scan(&status); err != nil {
					t.Fatal(err)
				}
				return status == "succeeded"
			})
			client.unblock()
			wait(func() bool {
				var pending int
				if err := s.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind IN ('ai.features','ai.embedding') AND status<>'succeeded'`).Scan(&pending); err != nil {
					t.Fatal(err)
				}
				return pending == 0
			})
			var theme string
			var gameID uuid.UUID
			if err = s.repo.store.Pool.QueryRow(ctx, `SELECT f.game_id,f.features->'themes'->>0 FROM game_features f WHERE model='text-v1'`).Scan(&gameID, &theme); err != nil {
				t.Fatal(err)
			}
			vector, err := s.repo.store.GetGameEmbedding(ctx, gameID, "embedding-v1")
			if err != nil {
				t.Fatal(err)
			}
			if theme != "content-b" || vector.Vector[0] != 0 || vector.Vector[1] != 1 || stalePublication.Load() {
				t.Fatal("an obsolete AI execution overwrote the current feature/vector singleton")
			}
			if stage == "queued" {
				client.mu.Lock()
				ids := append([]uuid.UUID(nil), client.featureJobs...)
				client.mu.Unlock()
				if len(ids) != 1 || ids[0] != secondJob {
					t.Fatal("a queued obsolete generation processed the newer snapshot")
				}
			}
		})
	}
}

func TestDiscoveryLegacyPayloadKeepsItsPlannedContentIdentity(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			s, metadata := discoveryFixture(t)
			ctx := context.Background()
			client := &repeatingContentClient{}
			enrichment, err := ai.NewService(ai.NewSQLRepository(s.repo.store), client, "text-v1", "embedding-v1")
			if err != nil {
				t.Fatal(err)
			}
			s.SetEnrichmentPlanner(enrichment)
			metadata.details.ShortDescription = "Content A"
			app := DiscoveredApp{AppID: 42, LastModified: time.Now().UTC().Truncate(time.Second)}
			lease := leaseDiscoveryID(t, s, enqueueQualification(t, s, app, 0))
			if err = s.handleQualification(ctx, lease); err != nil {
				t.Fatal(err)
			}
			if err = s.queue.Complete(ctx, lease); err != nil {
				t.Fatal(err)
			}
			if _, err = s.repo.store.Pool.Exec(ctx, `UPDATE sync_jobs SET payload=payload-'execution' WHERE kind='ai.features'`); err != nil {
				t.Fatal(err)
			}
			if changed {
				if _, err = s.repo.store.Pool.Exec(ctx, `UPDATE games SET source_snapshot=jsonb_set(source_snapshot,'{ShortDescription}','"Content B"') WHERE steam_app_id=42`); err != nil {
					t.Fatal(err)
				}
			}
			finishDiscoveryEnrichment(t, s, enrichment)
			want := int32(1)
			if changed {
				want = 0
			}
			if client.featureCalls.Load() != want || client.embeddingCalls.Load() != want {
				t.Fatal("legacy catalog execution ignored its planned content or lost compatibility")
			}
		})
	}
}

func TestDiscoverySemanticNoopDuringExtractionRetainsPublication(t *testing.T) {
	s, metadata := discoveryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := &blockingDiscoveryAI{stage: "features", started: make(chan struct{}), release: make(chan struct{})}
	enrichment, err := ai.NewService(ai.NewSQLRepository(s.repo.store), client, "text-v1", "embedding-v1")
	if err != nil {
		t.Fatal(err)
	}
	enrichment.SetQueue(s.queue)
	s.SetEnrichmentPlanner(enrichment)
	metadata.details.ShortDescription = "Content A"
	metadata.details.Name = "Upcoming   Game"
	metadata.details.Genres = []steam.Genre{{ID: "2", Description: "Strategy"}, {ID: "1", Description: "Puzzle"}}
	now := time.Now().UTC().Truncate(time.Second)
	first := leaseDiscoveryID(t, s, enqueueQualification(t, s, DiscoveredApp{AppID: 42, LastModified: now}, 0))
	if err = s.handleQualification(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err = s.queue.Complete(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := leaseDiscoveryID(t, s, enqueueQualification(t, s, DiscoveredApp{AppID: 42, LastModified: now.Add(time.Second)}, 0))
	worker := jobs.NewWorker(s.queue, "semantic-noop", jobs.WithPollInterval(time.Millisecond))
	if err = enrichment.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	defer func() {
		client.unblock()
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-client.started:
	case <-ctx.Done():
		t.Fatal("feature extraction did not start")
	}
	metadata.details.Name = "Upcoming Game"
	metadata.details.Genres = []steam.Genre{{ID: "1", Description: "Puzzle"}, {ID: "2", Description: "Strategy"}}
	if err = s.handleQualification(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err = s.queue.Complete(ctx, second); err != nil {
		t.Fatal(err)
	}
	client.unblock()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pending int
		if err = s.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind IN ('ai.features','ai.embedding') AND status<>'succeeded'`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("semantic no-op did not finish")
		}
	}
	var features, embeddings int
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM game_features),(SELECT count(*) FROM game_embeddings)`).Scan(&features, &embeddings); err != nil {
		t.Fatal(err)
	}
	if features != 1 || embeddings != 1 || client.featureCalls.Load() != 1 || client.embeddingCalls.Load() != 1 {
		t.Fatal("a semantically unchanged metadata revision discarded the current execution")
	}
}
