package catalog

import (
	"context"
	"encoding/json"
	"errors"
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
	worker := jobs.NewWorker(s.queue, jobs.LaneInteractive, "cached-enrichment", jobs.WithPollInterval(time.Millisecond))
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

func TestDiscoveryRetiredTickKeysCannotBlockInteractiveSuccessors(t *testing.T) {
	s, _ := discoveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour).Add(time.Minute)
	for _, at := range []time.Time{now.Truncate(time.Hour), now.Truncate(time.Hour).Add(time.Hour)} {
		payload, _ := json.Marshal(tickPayload{1, at})
		if _, err := s.repo.store.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,lane,status,payload,available_at,last_error,result)
			VALUES($1,$2,$3,'catalog','dead',$4,$5,'superseded_catalog_workflow','{"audit":true}')`, uuid.New(), TickJobKind, fmt.Sprintf("catalog.discovery.tick:v1:%d", at.Unix()), payload, at); err != nil {
			t.Fatal(err)
		}
	}
	for generation := range 3 {
		current := now.Add(time.Duration(generation) * time.Hour)
		s.now = func() time.Time { return current }
		for range 5 {
			if err := s.EnsureScheduled(ctx); err != nil {
				t.Fatal(err)
			}
		}
		var currentID uuid.UUID
		if err := s.repo.store.Pool.QueryRow(ctx, `SELECT id FROM sync_jobs WHERE kind=$1 AND lane='interactive' AND status='queued' AND available_at=$2`, TickJobKind, current.Truncate(time.Hour)).Scan(&currentID); err != nil {
			t.Fatal(err)
		}
		lease := leaseDiscoveryID(t, s, currentID)
		if err := s.handleTick(ctx, lease); err != nil {
			t.Fatal(err)
		}
		if err := s.queue.Complete(ctx, lease); err != nil {
			t.Fatal(err)
		}
		var retired, live, successors int
		if err := s.repo.store.Pool.QueryRow(ctx, `SELECT
			count(*) FILTER(WHERE lane='catalog' AND status='dead' AND last_error='superseded_catalog_workflow' AND result='{"audit":true}'),
			count(*) FILTER(WHERE lane='interactive' AND status NOT IN ('succeeded','dead')),
			count(*) FILTER(WHERE lane='interactive' AND status='queued' AND available_at=$2)
			FROM sync_jobs WHERE kind=$1`, TickJobKind, current.Truncate(time.Hour).Add(time.Hour)).Scan(&retired, &live, &successors); err != nil {
			t.Fatal(err)
		}
		if retired != 2 || live != 1 || successors != 1 {
			t.Fatalf("generation %d retired=%d live=%d successors=%d", generation, retired, live, successors)
		}
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

func TestLegacyCutoverSweepsQueuedWorkAndFencesRunningRetry(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewDiscoveryRepository(db)
	queue := jobs.NewQueue(db)
	ctx := context.Background()

	runningID, err := queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: TickJobKind, IdempotencyKey: "cutover:running", MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := queue.Claim(ctx, jobs.LaneInteractive, "cutover-worker", time.Minute)
	if err != nil || lease == nil || lease.ID != runningID {
		t.Fatalf("claim=%+v err=%v", lease, err)
	}
	for index, status := range []string{"queued", "waiting", "retryable"} {
		id, enqueueErr := queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: QualifyJobKind,
			IdempotencyKey: fmt.Sprintf("cutover:%s", status), MaxAttempts: 3})
		if enqueueErr != nil {
			t.Fatal(enqueueErr)
		}
		if index > 0 {
			if _, err := db.Pool.Exec(ctx, `UPDATE sync_jobs SET status=$2 WHERE id=$1`, id, status); err != nil {
				t.Fatal(err)
			}
		}
	}

	result, err := repo.CutOverLegacyDiscovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Cancelled != 3 || result.RunningFenced != 1 {
		t.Fatalf("cutover=%+v", result)
	}
	outcome, err := queue.Fail(ctx, *lease, errors.New("post-cutover failure"), time.Now().UTC())
	if err != nil || outcome != jobs.FailureDead {
		t.Fatalf("failure outcome=%q err=%v", outcome, err)
	}
	if count, err := repo.CountNonterminalLegacyJobs(ctx); err != nil || count != 0 {
		t.Fatalf("nonterminal=%d err=%v", count, err)
	}

	// Idempotent calls still sweep a legacy row written out of band, without
	// changing the original cutover timestamp.
	var firstCutover time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT legacy_cutover_at FROM catalog_workflow_state WHERE singleton`).Scan(&firstCutover); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: BackfillJobKind, IdempotencyKey: "cutover:late"}); err != nil {
		t.Fatal(err)
	}
	second, err := repo.CutOverLegacyDiscovery(ctx)
	if err != nil || second.Cancelled != 1 || second.RunningFenced != 0 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	var secondCutover time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT legacy_cutover_at FROM catalog_workflow_state WHERE singleton`).Scan(&secondCutover); err != nil {
		t.Fatal(err)
	}
	if !secondCutover.Equal(firstCutover) {
		t.Fatalf("cutover timestamp changed: %s -> %s", firstCutover, secondCutover)
	}
}

func TestLegacyEnqueueRaceWithCutoverLeavesNoSurvivingWork(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewDiscoveryRepository(db)
	service := &DiscoveryService{repo: repo, queue: jobs.NewQueue(db)}
	ctx := context.Background()
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		_, err := service.enqueueLegacy(ctx, tickSpec(time.Now().UTC().Truncate(time.Hour)))
		errs <- err
	}()
	go func() {
		<-start
		_, err := repo.CutOverLegacyDiscovery(ctx)
		errs <- err
	}()
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if count, err := repo.CountNonterminalLegacyJobs(ctx); err != nil || count != 0 {
		t.Fatalf("race left nonterminal=%d err=%v", count, err)
	}
}

func TestLegacyCutoverStopsClaimedTickAndScanSuccessors(t *testing.T) {
	s, _ := discoveryFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.source = &discoverySourceStub{pages: []DiscoveryPage{{
		Apps:      []DiscoveredApp{{AppID: 42, LastModified: now}},
		NextAppID: 42,
	}}}

	tickID, err := s.queue.Enqueue(ctx, tickSpec(now))
	if err != nil {
		t.Fatal(err)
	}
	tick := leaseDiscoveryID(t, s, tickID)
	scan, _ := scanLease(t, s, DiscoveryBackfill, now, 0)

	seeded := make([]uuid.UUID, 0, 3)
	for _, status := range []string{"queued", "waiting", "retryable"} {
		id, enqueueErr := s.queue.Enqueue(ctx, jobs.Spec{
			Lane: jobs.LaneInteractive, Kind: QualifyJobKind,
			IdempotencyKey: "claimed-cutover:" + status, MaxAttempts: 3,
		})
		if enqueueErr != nil {
			t.Fatal(enqueueErr)
		}
		if status != "queued" {
			if _, err = s.repo.store.Pool.Exec(ctx, `UPDATE sync_jobs SET status=$2 WHERE id=$1`, id, status); err != nil {
				t.Fatal(err)
			}
		}
		seeded = append(seeded, id)
	}

	cutover, err := s.repo.CutOverLegacyDiscovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cutover.Cancelled != 3 || cutover.RunningFenced != 2 {
		t.Fatalf("cutover=%+v", cutover)
	}
	if err = s.handleTick(ctx, tick); err != nil {
		t.Fatalf("claimed tick completion: %v", err)
	}
	if err = s.queue.Complete(ctx, tick); err != nil {
		t.Fatal(err)
	}
	scanErr := s.handleScan(ctx, scan)
	var superseded legacySupersededError
	if !errors.As(scanErr, &superseded) {
		t.Fatalf("claimed scan error=%v, want superseded", scanErr)
	}
	outcome, err := s.queue.Fail(ctx, scan, scanErr, now)
	if err != nil || outcome != jobs.FailureDead {
		t.Fatalf("claimed scan outcome=%q err=%v", outcome, err)
	}

	var cancelled, tickSucceeded, scanDead, successorTicks, childQualifications, deferred, legacyLive int
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT
		count(*) FILTER(WHERE id=ANY($1::uuid[]) AND status='dead' AND last_error='superseded_catalog_workflow'),
		count(*) FILTER(WHERE id=$3 AND status='succeeded'),
		count(*) FILTER(WHERE id=$4 AND status='dead' AND last_error='superseded_catalog_workflow'),
		count(*) FILTER(WHERE kind=$2 AND id<>$3),
		count(*) FILTER(WHERE parent_job_id=$4),
		count(*) FILTER(WHERE id IN ($3,$4) AND status IN ('waiting','retryable','queued')),
		count(*) FILTER(WHERE kind=ANY($5::text[]) AND status NOT IN ('succeeded','dead'))
		FROM sync_jobs`, seeded, TickJobKind, tick.ID, scan.ID,
		[]string{TickJobKind, IncrementalJobKind, BackfillJobKind, QualifyJobKind, RebuildWakeJobKind}).
		Scan(&cancelled, &tickSucceeded, &scanDead, &successorTicks, &childQualifications, &deferred, &legacyLive); err != nil {
		t.Fatal(err)
	}
	if cancelled != 3 || tickSucceeded != 1 || scanDead != 1 || successorTicks != 0 || childQualifications != 0 || deferred != 0 || legacyLive != 0 {
		t.Fatalf("cancelled=%d tick_succeeded=%d scan_dead=%d successor_ticks=%d children=%d deferred=%d live=%d", cancelled, tickSucceeded, scanDead, successorTicks, childQualifications, deferred, legacyLive)
	}
	if count, err := s.repo.CountNonterminalLegacyJobs(ctx); err != nil || count != 0 {
		t.Fatalf("nonterminal=%d err=%v", count, err)
	}
}

func TestLegacyCutoverMakesClaimedRebuildWakeupANoop(t *testing.T) {
	s, _ := discoveryFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now.Add(catalogRebuildDebounce) }
	userID := uuid.New()
	if _, err := s.repo.store.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.repo.store.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, uuid.New(), userID); err != nil {
		t.Fatal(err)
	}
	markCatalogDirty(t, s, now)
	payload, _ := json.Marshal(rebuildWakePayload{Version: 1, UserID: userID, Revision: 1})
	wakeID, err := s.queue.Enqueue(ctx, jobs.Spec{
		Lane: jobs.LaneInteractive, Kind: RebuildWakeJobKind, UserID: userID,
		IdempotencyKey: "legacy-rebuild-wakeup:claimed", Payload: payload, MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	wake := leaseDiscoveryID(t, s, wakeID)
	if _, err = s.repo.CutOverLegacyDiscovery(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.handleRebuildWakeup(ctx, wake); err != nil {
		t.Fatal(err)
	}
	if err = s.queue.Complete(ctx, wake); err != nil {
		t.Fatal(err)
	}
	var rebuilds int
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='recommendations.rebuild'`).Scan(&rebuilds); err != nil {
		t.Fatal(err)
	}
	if rebuilds != 0 {
		t.Fatalf("claimed legacy wakeup published %d recommendation rebuilds after cutover", rebuilds)
	}
}

func TestLegacyRebuildWakeupCrashReplayNeverPublishesRecommendationWork(t *testing.T) {
	s, _ := discoveryFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now.Add(catalogRebuildDebounce) }
	userID := uuid.New()
	if _, err := s.repo.store.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.repo.store.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, uuid.New(), userID); err != nil {
		t.Fatal(err)
	}
	markCatalogDirty(t, s, now)
	payload, _ := json.Marshal(rebuildWakePayload{Version: 1, UserID: userID, Revision: 1})
	wakeID, err := s.queue.Enqueue(ctx, jobs.Spec{
		Lane: jobs.LaneInteractive, Kind: RebuildWakeJobKind, UserID: userID,
		IdempotencyKey: "legacy-rebuild-wakeup:crash-replay", Payload: payload, MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	wake := leaseDiscoveryID(t, s, wakeID)
	// The first handler return is the crash boundary: queue completion never
	// happens, so the same fenced lease is replayed. Neither pass may publish.
	for range 2 {
		if err = s.handleRebuildWakeup(ctx, wake); err != nil {
			t.Fatal(err)
		}
	}
	var rebuilds int
	var dirtyRevision, completedRevision int64
	var pending bool
	if err = s.repo.store.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM sync_jobs WHERE kind='recommendations.rebuild'),
		dirty_revision,last_completed_revision,pending_job_id IS NOT NULL
		FROM catalog_rebuild_state WHERE user_id=$1`, userID).
		Scan(&rebuilds, &dirtyRevision, &completedRevision, &pending); err != nil {
		t.Fatal(err)
	}
	if rebuilds != 0 || dirtyRevision != 1 || completedRevision != 0 || pending {
		t.Fatalf("rebuilds=%d dirty=%d completed=%d pending=%v", rebuilds, dirtyRevision, completedRevision, pending)
	}
	if err = s.queue.Complete(ctx, wake); err != nil {
		t.Fatal(err)
	}
}
