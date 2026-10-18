package catalog

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/steam"
)

func TestDiscoveryNewerUnavailableFencesOlderAdmission(t *testing.T) {
	s, metadata := discoveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	qualify := func(app DiscoveredApp) {
		t.Helper()
		lease := leaseDiscoveryID(t, s, enqueueQualification(t, s, app, 0))
		if err := s.handleQualification(ctx, lease); err != nil {
			t.Fatal(err)
		}
		if err := s.queue.Complete(ctx, lease); err != nil {
			t.Fatal(err)
		}
	}
	qualify(DiscoveredApp{AppID: 42, LastModified: now})
	metadata.details.Name = "Known Released"
	metadata.details.ReleaseDate.ComingSoon = false
	qualify(DiscoveredApp{AppID: 42, LastModified: now.Add(time.Second)})
	metadata.err = steam.ErrUnavailable
	newest := now.Add(3 * time.Second)
	qualify(DiscoveredApp{AppID: 42, LastModified: newest})
	var before []byte
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT source_snapshot FROM games WHERE steam_app_id=42`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	metadata.err = nil
	metadata.details.Name = "Slow Older Upcoming"
	metadata.details.ReleaseDate.ComingSoon = true
	qualify(DiscoveredApp{AppID: 42, LastModified: now.Add(2 * time.Second)})
	var name, availability, fetchAvailability, newestStatus string
	var active bool
	var after []byte
	var projections int
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT g.name,g.availability,g.source_snapshot,f.availability,src.active,o.status,(SELECT count(*) FROM catalog_entries WHERE game_id=g.id)
 FROM games g JOIN catalog_fetch_states f ON f.game_id=g.id JOIN catalog_entry_sources src ON src.game_id=g.id AND src.source='steam_discovery'
 JOIN catalog_discovery_observations o ON o.steam_app_id=g.steam_app_id AND o.last_modified=$1 WHERE g.steam_app_id=42`, newest).Scan(&name, &availability, &after, &fetchAvailability, &active, &newestStatus, &projections); err != nil {
		t.Fatal(err)
	}
	if name != "Known Released" || availability != "unavailable" || fetchAvailability != "unavailable" || active || newestStatus != "unavailable" || projections != 0 || string(after) != string(before) {
		t.Fatal("older admission overwrote the newer unavailable state or reactivated its source")
	}
}

type repeatingContentClient struct {
	ai.Client
	featureCalls   atomic.Int32
	embeddingCalls atomic.Int32
}

func (c *repeatingContentClient) ExtractGameFeatures(_ context.Context, evidence ai.GameEvidence) (ai.GameFeatures, error) {
	c.featureCalls.Add(1)
	theme := "content-a"
	for _, item := range evidence.Evidence {
		if item.Key == "description" && strings.Contains(item.Value, "Content B") {
			theme = "content-b"
		}
	}
	return ai.GameFeatures{Themes: []string{theme}, Mechanics: []string{}, Modes: []string{}, Tone: []string{}, EvidenceKeys: []string{"description"}}, nil
}
func (c *repeatingContentClient) Embed(_ context.Context, input []string) ([][]float32, error) {
	c.embeddingCalls.Add(1)
	vector := make([]float32, ai.EmbeddingDimensions)
	if strings.Contains(input[0], "Content B") {
		vector[1] = 1
	} else {
		vector[0] = 1
	}
	return [][]float32{vector}, nil
}

func TestDiscoveryRepeatedContentRefreshesSingletonEnrichment(t *testing.T) {
	s, metadata := discoveryFixture(t)
	ctx := context.Background()
	client := &repeatingContentClient{}
	enrichment, err := ai.NewService(ai.NewSQLRepository(s.repo.store), client, "text-v1", "embedding-v1")
	if err != nil {
		t.Fatal(err)
	}
	s.SetEnrichmentPlanner(enrichment)
	now := time.Now().UTC().Truncate(time.Second)
	var originalJobs []byte
	var featureHashes, embeddingHashes []string
	for index, description := range []string{"Content A", "Content B", "Content A", "Content A"} {
		metadata.details.ShortDescription = description
		metadata.details.HeaderImage = strings.Repeat("x", index) // Presentation-only changes must not add executions.
		app := DiscoveredApp{AppID: 42, LastModified: now.Add(time.Duration(index) * time.Second)}
		lease := leaseDiscoveryID(t, s, enqueueQualification(t, s, app, 0))
		if err = s.handleQualification(ctx, lease); err != nil {
			t.Fatal(err)
		}
		if err = s.queue.Complete(ctx, lease); err != nil {
			t.Fatal(err)
		}
		finishDiscoveryEnrichment(t, s, enrichment)
		var featureHash, embeddingHash string
		var features []byte
		if err = s.repo.store.Pool.QueryRow(ctx, `SELECT f.input_content_hash,e.input_content_hash,f.features FROM game_features f JOIN game_embeddings e ON e.game_id=f.game_id WHERE f.model='text-v1' AND e.model='embedding-v1'`).Scan(&featureHash, &embeddingHash, &features); err != nil {
			t.Fatal(err)
		}
		featureHashes = append(featureHashes, featureHash)
		embeddingHashes = append(embeddingHashes, embeddingHash)
		var decoded ai.GameFeatures
		if json.Unmarshal(features, &decoded) != nil || len(decoded.Themes) != 1 || decoded.Themes[0] != strings.ToLower(strings.ReplaceAll(description, " ", "-")) {
			t.Fatalf("phase %d: singleton features do not describe the current canonical content", index)
		}
		if index == 0 {
			if err = s.repo.store.Pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(j) ORDER BY j.id) FROM sync_jobs j WHERE kind IN ('ai.features','ai.embedding')`).Scan(&originalJobs); err != nil {
				t.Fatal(err)
			}
		}
	}
	if featureHashes[0] == featureHashes[1] || featureHashes[0] != featureHashes[2] || embeddingHashes[0] == embeddingHashes[1] || embeddingHashes[0] != embeddingHashes[2] {
		t.Fatal("returning to content A failed to restore its feature/embedding input hashes")
	}
	var jobs, terminal int
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE status='succeeded') FROM sync_jobs WHERE kind IN ('ai.features','ai.embedding')`).Scan(&jobs, &terminal); err != nil {
		t.Fatal(err)
	}
	if jobs != 6 || terminal != 6 || client.featureCalls.Load() != 3 || client.embeddingCalls.Load() != 3 {
		t.Fatal("execution identity lost a transition, duplicated unchanged work, or rearmed terminal history")
	}
	var oldIDs []struct {
		ID uuid.UUID `json:"id"`
	}
	if err = json.Unmarshal(originalJobs, &oldIDs); err != nil {
		t.Fatal(err)
	}
	var retained []byte
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(j) ORDER BY j.id) FROM sync_jobs j WHERE id IN ($1,$2)`, oldIDs[0].ID, oldIDs[1].ID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if string(retained) != string(originalJobs) {
		t.Fatal("repeated content modified the first terminal execution")
	}
}

func TestDiscoveryEnrichmentExecutionKeepsRetryAndDeadHistory(t *testing.T) {
	s, metadata := discoveryFixture(t)
	ctx := context.Background()
	planner, err := ai.NewService(ai.NewSQLRepository(s.repo.store), &repeatingContentClient{}, "text-v1", "embedding-v1")
	if err != nil {
		t.Fatal(err)
	}
	gameID := uuid.New()
	contentA := metadata.details
	contentB := contentA
	contentB.ShortDescription = "Content B"
	enqueue := func(details steam.GameDetails, priority int) error {
		tx, err := s.repo.store.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if err = enqueueDiscoveryFeatures(ctx, s.repo.store.Queries.WithTx(tx), planner, gameID, details, priority); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err = enqueue(contentA, 50); err != nil {
		t.Fatal(err)
	}
	var firstID uuid.UUID
	var retryAt time.Time
	if err = s.repo.store.Pool.QueryRow(ctx, `UPDATE sync_jobs SET status='retryable',attempts=1,last_error='bounded retry',available_at=now()+interval '1 hour' RETURNING id,available_at`).Scan(&firstID, &retryAt); err != nil {
		t.Fatal(err)
	}
	if err = enqueue(contentA, 80); err != nil {
		t.Fatal(err)
	}
	var count, attempts, priority int
	var status string
	var retainedRetry time.Time
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT count(*) OVER(),status,attempts,priority,available_at FROM sync_jobs`).Scan(&count, &status, &attempts, &priority, &retainedRetry); err != nil {
		t.Fatal(err)
	}
	if count != 1 || status != "retryable" || attempts != 1 || priority != 80 || !retainedRetry.Equal(retryAt) {
		t.Fatal("unchanged planning replaced a pending retry or changed its schedule")
	}
	var terminal []byte
	if err = s.repo.store.Pool.QueryRow(ctx, `UPDATE sync_jobs j SET status='dead',attempts=max_attempts,last_error='bounded terminal failure' WHERE id=$1 RETURNING to_jsonb(j)`, firstID).Scan(&terminal); err != nil {
		t.Fatal(err)
	}
	for _, details := range []steam.GameDetails{contentA, contentB, contentA, contentA} {
		if err = enqueue(details, 100); err != nil {
			t.Fatal(err)
		}
	}
	var retained []byte
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT to_jsonb(j) FROM sync_jobs j WHERE id=$1`, firstID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if string(retained) != string(terminal) {
		t.Fatal("new content occurrence mutated the old dead execution")
	}
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("transition executions=%d error=%v", count, err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := enqueue(contentB, 50); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("concurrent planners duplicated a new execution: count=%d error=%v", count, err)
	}
}

func TestDiscoveryEnrichmentPlanningCannotEditNewlyTerminalExecution(t *testing.T) {
	s, metadata := discoveryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	planner, err := ai.NewService(ai.NewSQLRepository(s.repo.store), &repeatingContentClient{}, "text-v1", "embedding-v1")
	if err != nil {
		t.Fatal(err)
	}
	gameID := uuid.New()
	first, err := s.repo.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx)
	if err = enqueueDiscoveryFeatures(ctx, s.repo.store.Queries.WithTx(first), planner, gameID, metadata.details, 50); err != nil {
		t.Fatal(err)
	}
	if err = first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	completion, err := s.repo.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer completion.Rollback(ctx)
	var jobID uuid.UUID
	if err = completion.QueryRow(ctx, `SELECT id FROM sync_jobs FOR UPDATE`).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	pid := make(chan uint32, 1)
	finished := make(chan error, 1)
	go func() {
		tx, err := s.repo.store.Pool.Begin(ctx)
		if err != nil {
			finished <- err
			return
		}
		defer tx.Rollback(ctx)
		pid <- tx.Conn().PgConn().PID()
		if err = enqueueDiscoveryFeatures(ctx, s.repo.store.Queries.WithTx(tx), planner, gameID, metadata.details, 80); err == nil {
			err = tx.Commit(ctx)
		}
		finished <- err
	}()
	var planningPID uint32
	select {
	case planningPID = <-pid:
	case err := <-finished:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err = s.repo.store.Pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, planningPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("planner did not wait on the locked execution")
		}
	}
	var terminal []byte
	if err = completion.QueryRow(ctx, `UPDATE sync_jobs j SET status='succeeded' WHERE id=$1 RETURNING to_jsonb(j)`, jobID).Scan(&terminal); err != nil {
		t.Fatal(err)
	}
	if err = completion.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
	var retained []byte
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT to_jsonb(j) FROM sync_jobs j WHERE id=$1`, jobID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if string(retained) != string(terminal) {
		t.Fatal("planning raced completion and modified terminal history")
	}
}
