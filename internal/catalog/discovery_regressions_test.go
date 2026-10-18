package catalog

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/jobs"
)

type discoveryEnrichmentClient struct{ ai.Client }

func (discoveryEnrichmentClient) ExtractGameFeatures(_ context.Context, evidence ai.GameEvidence) (ai.GameFeatures, error) {
	return ai.GameFeatures{Themes: []string{"adventure"}, Mechanics: []string{}, Modes: []string{}, Tone: []string{}, EvidenceKeys: []string{evidence.Evidence[0].Key}}, nil
}
func (discoveryEnrichmentClient) Embed(context.Context, []string) ([][]float32, error) {
	vector := make([]float32, ai.EmbeddingDimensions)
	vector[0] = 1
	return [][]float32{vector}, nil
}

func finishDiscoveryEnrichment(t *testing.T, s *DiscoveryService, enrichment *ai.Service) {
	t.Helper()
	enrichment.SetQueue(s.queue)
	worker := jobs.NewWorker(s.queue, "cached-enrichment", jobs.WithPollInterval(time.Millisecond))
	if err := enrichment.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- worker.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Error(err)
		}
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(5 * time.Second)
	for {
		var pending, dead int
		if err := s.repo.store.Pool.QueryRow(context.Background(), `SELECT count(*) FILTER(WHERE status NOT IN ('succeeded','dead')),count(*) FILTER(WHERE status='dead') FROM sync_jobs WHERE kind IN ('ai.features','ai.embedding')`).Scan(&pending, &dead); err != nil {
			t.Fatal(err)
		}
		if dead != 0 {
			t.Fatal("cached enrichment job failed")
		}
		if pending == 0 {
			return
		}
		select {
		case <-ticker.C:
		case <-timeout:
			t.Fatal("cached enrichment did not finish")
		}
	}
}

func TestDiscoveryUnchangedRevisionPlansMissingAndNewModelEnrichment(t *testing.T) {
	s, metadata := discoveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	app := DiscoveredApp{AppID: 42, LastModified: now}
	initial := qualificationLease(t, s, app, 0)
	if err := s.handleQualification(ctx, initial); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Complete(ctx, initial); err != nil {
		t.Fatal(err)
	}
	metadata.err = fmt.Errorf("unchanged revision must use the trusted snapshot")
	var attemptedAt time.Time
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT last_attempt_at FROM catalog_discovery_observations`).Scan(&attemptedAt); err != nil {
		t.Fatal(err)
	}
	for index, models := range []struct {
		text, embedding string
		jobs            int
	}{{"text-v1", "embedding-v1", 1}, {"text-v1", "embedding-v1", 1}, {"text-v2", "embedding-v1", 2}, {"text-v2", "embedding-v2", 3}, {"text-v2", "embedding-v2", 3}} {
		at := now.Add(time.Duration(index+1) * time.Hour)
		s.now = func() time.Time { return at }
		enrichment, err := ai.NewService(ai.NewSQLRepository(s.repo.store), discoveryEnrichmentClient{}, models.text, models.embedding)
		if err != nil {
			t.Fatal(err)
		}
		s.SetEnrichmentPlanner(enrichment)
		s.source = &discoverySourceStub{pages: []DiscoveryPage{{Apps: []DiscoveredApp{app}, NextAppID: 42}}}
		lease, _ := scanLease(t, s, DiscoveryBackfill, at, 0)
		if err = s.handleScan(ctx, lease); err != nil {
			t.Fatal(err)
		}
		if err = s.queue.Complete(ctx, lease); err != nil {
			t.Fatal(err)
		}
		var featureJobs int
		if err = s.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='ai.features'`).Scan(&featureJobs); err != nil {
			t.Fatal(err)
		}
		if featureJobs != models.jobs {
			t.Fatalf("phase %d: cached admitted revision planned %d feature jobs, want %d", index, featureJobs, models.jobs)
		}
		finishDiscoveryEnrichment(t, s, enrichment)
		var embeddingJobs, qualifications, observations, attempts, used int
		var retained time.Time
		if err = s.repo.store.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM sync_jobs WHERE kind='ai.embedding'),(SELECT count(*) FROM sync_jobs WHERE kind='catalog.qualify'),(SELECT count(*) FROM catalog_discovery_observations),(SELECT attempts FROM catalog_discovery_observations),(SELECT last_attempt_at FROM catalog_discovery_observations),(SELECT COALESCE(sum(used),0)::integer FROM catalog_discovery_budget)`).Scan(&embeddingJobs, &qualifications, &observations, &attempts, &retained, &used); err != nil {
			t.Fatal(err)
		}
		if embeddingJobs != models.jobs || qualifications != 1 || observations != 1 || attempts != 1 || !retained.Equal(attemptedAt) || used != 0 {
			t.Fatal("cached enrichment requalified, spent metadata budget, or duplicated model jobs")
		}
	}
	var textModels, embeddingModels int
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT (SELECT count(DISTINCT model) FROM game_features),(SELECT count(DISTINCT model) FROM game_embeddings)`).Scan(&textModels, &embeddingModels); err != nil {
		t.Fatal(err)
	}
	if textModels != 2 || embeddingModels != 2 {
		t.Fatal("new model versions did not produce persisted features and embeddings")
	}
}

func TestDiscoveryIntervalNudgeKeepsSingleFutureChain(t *testing.T) {
	for _, interval := range []int{2, 3, 24} {
		t.Run(fmt.Sprint(interval), func(t *testing.T) {
			s, _ := discoveryFixture(t)
			ctx := context.Background()
			s.cfg.IntervalHours = interval
			first := time.Date(2026, 10, 18, 12, 5, 0, 0, time.UTC)
			s.now = func() time.Time { return first }
			if err := s.EnsureScheduled(ctx); err != nil {
				t.Fatal(err)
			}
			var current uuid.UUID
			if err := s.repo.store.Pool.QueryRow(ctx, `SELECT id FROM sync_jobs WHERE kind=$1 AND available_at<=$2 ORDER BY available_at LIMIT 1`, TickJobKind, first).Scan(&current); err != nil {
				t.Fatal(err)
			}
			lease := leaseDiscoveryID(t, s, current)
			if err := s.handleTick(ctx, lease); err != nil {
				t.Fatal(err)
			}
			if err := s.queue.Complete(ctx, lease); err != nil {
				t.Fatal(err)
			}
			adjacent := first.Add(time.Hour)
			s.now = func() time.Time { return adjacent }
			restarted, err := NewDiscoveryService(s.repo, s.source, s.metadata, s.queue, s.cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			restarted.now = func() time.Time { return adjacent }
			var wg sync.WaitGroup
			for index := range 20 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					coordinator := s
					if index%2 == 0 {
						coordinator = restarted
					}
					if e := coordinator.Nudge(ctx); e != nil {
						t.Error(e)
					}
				}()
			}
			wg.Wait()
			var future, total int
			if err = s.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE available_at>$2),count(*) FROM sync_jobs WHERE kind=$1 AND status NOT IN ('succeeded','dead')`, TickJobKind, adjacent).Scan(&future, &total); err != nil {
				t.Fatal(err)
			}
			if future != 1 || total != 1 {
				t.Fatalf("interval %d: adjacent-hour concurrent restart nudges left %d future and %d pending ticks", interval, future, total)
			}
		})
	}
}
