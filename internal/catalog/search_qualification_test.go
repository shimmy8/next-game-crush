package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

func TestSearchQualificationKindAndKeyAreDistinctFromLegacyV1(t *testing.T) {
	payload := SearchQualificationPayload{Version: 1, AppID: 620,
		CandidateRevision: CandidateRevision{RunRevision: 9, ObservationOrdinal: 17},
		ReservationID:     uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		ReservationToken:  uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		RetryGeneration:   3, Priority: 80}
	spec, err := NewSearchQualificationSpec(payload, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := "catalog.search.qualify:v1:620:run:9:observation:17:reservation:11111111-1111-4111-8111-111111111111:retry:3"
	if spec.Kind != SearchQualificationJobKind || spec.Kind == QualifyJobKind || spec.IdempotencyKey != wantKey ||
		spec.Lane != jobs.LaneCatalog || spec.Stage != "catalog_search_qualify" || spec.MaxAttempts != 4 || !spec.Optional || spec.Required {
		t.Fatalf("unexpected spec: %+v", spec)
	}
	wantJSON := `{"version":1,"app_id":620,"candidate_revision":{"run_revision":9,"observation_ordinal":17},"reservation_id":"11111111-1111-4111-8111-111111111111","reservation_token":"22222222-2222-4222-8222-222222222222","retry_generation":3,"priority":80}`
	if string(spec.Payload) != wantJSON {
		t.Fatalf("payload=%s", spec.Payload)
	}
	legacy := qualificationSpec(QualificationPayload{Version: 1, App: DiscoveredApp{AppID: 620, LastModified: time.Unix(100, 0).UTC()}, BudgetHour: time.Unix(0, 0).UTC(), Priority: 50}, uuid.Nil)
	if legacy.Kind != QualifyJobKind || legacy.Lane != jobs.LaneInteractive || legacy.MaxAttempts != 3 || legacy.IdempotencyKey == spec.IdempotencyKey {
		t.Fatalf("legacy protocol changed: %+v", legacy)
	}
	invalid := []SearchQualificationPayload{payload, payload, payload, payload, payload, payload}
	invalid[0].Version = 2
	invalid[1].AppID = 0
	invalid[2].CandidateRevision.RunRevision = 0
	invalid[3].CandidateRevision.ObservationOrdinal = 0
	invalid[4].ReservationID = uuid.Nil
	invalid[5].ReservationToken = uuid.Nil
	for i, candidate := range invalid {
		if _, err := NewSearchQualificationSpec(candidate, uuid.Nil); err == nil {
			t.Fatalf("invalid payload %d accepted", i)
		}
	}
}

func TestSemanticIdentityExcludesReviewReleaseAndPrice(t *testing.T) {
	base := steam.GameDetails{AppID: 42, Type: "game", Name: "A Game", ShortDescription: "semantic words",
		HeaderImage: "https://cdn.example/old.jpg", ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "Oct 2026"},
		Genres: []steam.Genre{{ID: "1", Description: "Action"}}}
	changedPresentation := base
	changedPresentation.HeaderImage = "https://cdn.example/new.jpg"
	changedPresentation.ReleaseDate = steam.ReleaseDate{ComingSoon: false, Raw: "18 Oct, 2026"}
	if SemanticContentHash(base) != SemanticContentHash(changedPresentation) {
		t.Fatal("presentation/release change altered semantic identity")
	}
	changedMeaning := base
	changedMeaning.ShortDescription = "different semantic words"
	if SemanticContentHash(base) == SemanticContentHash(changedMeaning) {
		t.Fatal("semantic description change did not alter identity")
	}
}

func TestLegacyAndSearchAIJobsStayInTheirOriginLane(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	queue := jobs.NewQueue(db)
	enrichment, err := ai.NewService(ai.NewSQLRepository(db), discoveryEnrichmentClient{}, "text-v1", "embedding-v1")
	if err != nil {
		t.Fatal(err)
	}
	enrichment.SetQueue(queue)
	admission := &allowCatalogFeatureAdmission{}
	enrichment.SetCatalogFeatureAdmission(admission)

	legacyID, catalogID := uuid.New(), uuid.New()
	details := steam.GameDetails{AppID: 701, Type: "game", Name: "Semantic Lane Test", ShortDescription: "same semantic evidence",
		Genres: []steam.Genre{{ID: "1", Description: "Action"}}, ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "Coming Soon"}}
	insert := func(id uuid.UUID, appID int64, value steam.GameDetails) {
		t.Helper()
		snapshot, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, execErr := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at,source_snapshot,availability)
			VALUES($1,$2,$3,'steam_search',now(),$4,'available')`, id, appID, value.Name, snapshot); execErr != nil {
			t.Fatal(execErr)
		}
	}
	insert(legacyID, 701, details)
	catalogDetails := details
	catalogDetails.AppID = 702
	catalogDetails.Name = "Catalog Lane Test"
	insert(catalogID, 702, catalogDetails)

	enqueue := func(fn func(context.Context, *dbgen.Queries) error) {
		t.Helper()
		tx, beginErr := db.Pool.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer tx.Rollback(ctx)
		if callErr := fn(ctx, db.Queries.WithTx(tx)); callErr != nil {
			t.Fatal(callErr)
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			t.Fatal(commitErr)
		}
	}
	enqueue(func(ctx context.Context, q *dbgen.Queries) error {
		return enqueueDiscoveryFeatures(ctx, q, enrichment, legacyID, details, 80)
	})
	// Search observes the same semantic identity while the interactive root is
	// pending. It must join that root without changing its lane or attempts.
	enqueue(func(ctx context.Context, q *dbgen.Queries) error {
		return enqueueSearchDiscoveryFeatures(ctx, q, enrichment, legacyID, details, 80)
	})
	enqueue(func(ctx context.Context, q *dbgen.Queries) error {
		return enqueueSearchDiscoveryFeatures(ctx, q, enrichment, catalogID, catalogDetails, 80)
	})

	var roots, interactiveRoots, catalogRoots int
	if err = db.Pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE lane='interactive' AND max_attempts=3),count(*) FILTER(WHERE lane='catalog' AND max_attempts=4)
		FROM sync_jobs WHERE kind='ai.features'`).Scan(&roots, &interactiveRoots, &catalogRoots); err != nil {
		t.Fatal(err)
	}
	if roots != 2 || interactiveRoots != 1 || catalogRoots != 1 {
		t.Fatalf("feature roots=%d interactive/3=%d catalog/4=%d", roots, interactiveRoots, catalogRoots)
	}
	var legacyAttempts int
	if err = db.Pool.QueryRow(ctx, `SELECT attempts FROM sync_jobs WHERE kind='ai.features' AND lane='interactive'`).Scan(&legacyAttempts); err != nil {
		t.Fatal(err)
	}
	if legacyAttempts != 0 {
		t.Fatalf("same-semantic search join changed pending legacy attempts to %d", legacyAttempts)
	}

	interactive := jobs.NewWorker(queue, jobs.LaneInteractive, "lane-test-interactive", jobs.WithPollInterval(time.Millisecond))
	catalog := jobs.NewWorker(queue, jobs.LaneCatalog, "lane-test-catalog", jobs.WithPollInterval(time.Millisecond))
	if err = enrichment.RegisterHandlers(interactive); err != nil {
		t.Fatal(err)
	}
	if err = enrichment.RegisterCatalogHandlers(catalog); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 2)
	go func() { done <- interactive.Run(runCtx) }()
	go func() { done <- catalog.Run(runCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var pending, dead int
		if err = db.Pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE status NOT IN ('succeeded','dead')),count(*) FILTER(WHERE status='dead')
			FROM sync_jobs WHERE kind IN ('ai.features','ai.embedding')`).Scan(&pending, &dead); err != nil {
			cancel()
			t.Fatal(err)
		}
		if dead != 0 {
			cancel()
			t.Fatal("AI lane test produced a dead job")
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("AI lane jobs did not finish: pending=%d", pending)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	for range 2 {
		if workerErr := <-done; workerErr != nil {
			t.Fatal(workerErr)
		}
	}

	rows, err := db.Pool.Query(ctx, `SELECT payload->>'game_id',kind,lane,max_attempts FROM sync_jobs
		WHERE kind IN ('ai.features','ai.embedding') ORDER BY payload->>'game_id',kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	counts := map[uuid.UUID]int{}
	for rows.Next() {
		var gameRaw, kind, lane string
		var maxAttempts int
		if err = rows.Scan(&gameRaw, &kind, &lane, &maxAttempts); err != nil {
			t.Fatal(err)
		}
		gameID, parseErr := uuid.Parse(gameRaw)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		counts[gameID]++
		wantLane, wantAttempts := "catalog", 4
		if gameID == legacyID {
			wantLane, wantAttempts = "interactive", 3
		}
		if lane != wantLane || maxAttempts != wantAttempts {
			t.Fatalf("%s %s lane/max_attempts=%s/%d want %s/%d", gameID, kind, lane, maxAttempts, wantLane, wantAttempts)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if counts[legacyID] != 2 || counts[catalogID] != 2 {
		t.Fatalf("root+embedding counts legacy=%d catalog=%d", counts[legacyID], counts[catalogID])
	}
	identities := admission.identitiesSnapshot()
	if admission.calls.Load() != 2 || len(identities) != 2 || identities[0] != identities[1] {
		t.Fatalf("catalog admission calls=%d identities=%v; root and cache-hit resumption must share one identity and interactive execution must bypass it", admission.calls.Load(), identities)
	}
}

type allowCatalogFeatureAdmission struct {
	calls atomic.Int32
	mu    sync.Mutex
	ids   []string
}

func (a *allowCatalogFeatureAdmission) ReserveCatalogFeature(_ context.Context, _ jobs.Lease, identity string, _ time.Time) (bool, time.Time, error) {
	a.calls.Add(1)
	a.mu.Lock()
	a.ids = append(a.ids, identity)
	a.mu.Unlock()
	return true, time.Time{}, nil
}

func (a *allowCatalogFeatureAdmission) identitiesSnapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.ids...)
}

type searchQualificationPlanner struct{ calls int }

func (p *searchQualificationPlanner) PlanCatalogGameFeatureJob(_ context.Context, gameID uuid.UUID, hash string, priority int) (jobs.Spec, error) {
	p.calls++
	raw, _ := json.Marshal(map[string]any{"game_id": gameID, "content_hash": hash})
	return jobs.Spec{Lane: jobs.LaneCatalog, Kind: "feature.test", Stage: "feature", IdempotencyKey: "feature:" + gameID.String() + ":" + hash,
		Payload: raw, Priority: priority, MaxAttempts: 4}, nil
}

func enableSearchQualificationProducer(t *testing.T, scan *SearchScanService, repo *SearchRepository) {
	t.Helper()
	scan.SetSearchQualificationProducer(repo)
}

func searchQualificationFixture(t *testing.T, observation steam.SearchObservation, details steam.GameDetails) (*DiscoveryService, *discoveryMetadata, *SearchRepository, jobs.Lease) {
	t.Helper()
	db := openCatalogTestStore(t)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	repo := NewSearchRepository(db)
	scan, err := NewSearchScanService(repo, &searchStub{}, jobs.NewQueue(db), cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	enableSearchQualificationProducer(t, scan, repo)
	run := searchTestRun(t, repo, cfg, now.Add(-time.Hour))
	if err := scan.CommitPage(context.Background(), run, steam.FeedPopularUpcoming, 0, 0,
		steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{observation}}); err != nil {
		t.Fatal(err)
	}
	if err := scan.CommitPage(context.Background(), run, steam.FeedPopularNew, 0, 0,
		steam.SearchPage{Start: 0, Count: 0, Total: 0}); err != nil {
		t.Fatal(err)
	}
	metadata := &discoveryMetadata{details: details}
	service, err := NewDiscoveryService(NewDiscoveryRepository(db), &discoverySourceStub{}, metadata, jobs.NewQueue(db), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	lease, err := service.queue.Claim(context.Background(), jobs.LaneCatalog, "search-qualification-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if lease == nil || lease.Kind != SearchQualificationJobKind {
		t.Fatalf("claimed %+v", lease)
	}
	return service, metadata, repo, *lease
}

func TestSearchQualificationProducerAtomicallyReservesAndEnqueues(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	cfg := DefaultDiscoveryConfig()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	scan, err := NewSearchScanService(repo, &searchStub{}, jobs.NewQueue(db), cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	enableSearchQualificationProducer(t, scan, repo)
	run := searchTestRun(t, repo, cfg, now)
	page := steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{
		testObservation(619, "20 Oct, 2026", steam.ReviewUnknown, ""),
	}}
	for range 2 {
		if err := scan.CommitPage(context.Background(), run, steam.FeedPopularUpcoming, 0, 0, page); err != nil {
			t.Fatal(err)
		}
		if err := scan.CommitPage(context.Background(), run, steam.FeedPopularNew, 0, 0, steam.SearchPage{Start: 0, Count: 0, Total: 0}); err != nil {
			t.Fatal(err)
		}
	}
	var jobsCount, reservations, reservedWork, budgetUsed int
	if err := db.Pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM sync_jobs WHERE kind='catalog.search.qualify'),
		(SELECT count(*) FROM catalog_store_reservations WHERE status='reserved'),
		(SELECT count(*) FROM catalog_search_worklist WHERE status='reserved'),
		(SELECT used FROM catalog_budget_reservations WHERE kind='appdetails')`).
		Scan(&jobsCount, &reservations, &reservedWork, &budgetUsed); err != nil {
		t.Fatal(err)
	}
	if jobsCount != 1 || reservations != 1 || reservedWork != 1 || budgetUsed != 1 {
		t.Fatalf("jobs=%d reservations=%d work=%d budget=%d", jobsCount, reservations, reservedWork, budgetUsed)
	}
}

func TestPopularityDependentCandidatesWaitForAuthoritativePopularFeeds(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	cfg := DefaultDiscoveryConfig()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	scan, err := NewSearchScanService(repo, &searchStub{}, jobs.NewQueue(db), cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	enableSearchQualificationProducer(t, scan, repo)
	run := searchTestRun(t, repo, cfg, now)
	tba := testObservation(617, "Coming Soon", steam.ReviewUnknown, "")
	unrated := testObservation(618, "17 Oct, 2026", steam.ReviewUnrated, "No user reviews")
	unrated.Review.Total = 0
	if err := scan.CommitPage(context.Background(), run, steam.FeedPopularUpcoming, 0, 0,
		steam.SearchPage{Start: 0, Count: 2, Total: 2, Items: []steam.SearchObservation{tba, unrated}}); err != nil {
		t.Fatal(err)
	}
	var jobsBefore, reservationsBefore, tombstonesBefore int
	if err := db.Pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM sync_jobs WHERE kind='catalog.search.qualify'),
		(SELECT count(*) FROM catalog_store_reservations),
		(SELECT count(*) FROM catalog_search_current WHERE tombstone_reason<>'')`).Scan(&jobsBefore, &reservationsBefore, &tombstonesBefore); err != nil {
		t.Fatal(err)
	}
	if jobsBefore != 0 || reservationsBefore != 0 || tombstonesBefore != 0 {
		t.Fatalf("before popular-new authority jobs=%d reservations=%d tombstones=%d", jobsBefore, reservationsBefore, tombstonesBefore)
	}
	if err := scan.CommitPage(context.Background(), run, steam.FeedPopularNew, 0, 0,
		steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{unrated}}); err != nil {
		t.Fatal(err)
	}
	var jobsAfter, reservationsAfter int
	if err := db.Pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM sync_jobs WHERE kind='catalog.search.qualify'),
		(SELECT count(*) FROM catalog_store_reservations WHERE status='reserved')`).Scan(&jobsAfter, &reservationsAfter); err != nil {
		t.Fatal(err)
	}
	if jobsAfter != 2 || reservationsAfter != 2 {
		t.Fatalf("after popular authority jobs=%d reservations=%d", jobsAfter, reservationsAfter)
	}
}

func TestCommittedPageStartsOnlyIndependentlyEligibleQualificationEarly(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	cfg := DefaultDiscoveryConfig()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	scan, err := NewSearchScanService(repo, &searchStub{}, jobs.NewQueue(db), cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	enableSearchQualificationProducer(t, scan, repo)
	run := searchTestRun(t, repo, cfg, now)
	exact := testObservation(632, "20 Oct, 2026", steam.ReviewUnknown, "")
	tba := testObservation(633, "Coming Soon", steam.ReviewUnknown, "")
	if err := scan.CommitPage(context.Background(), run, steam.FeedPopularUpcoming, 0, 0,
		steam.SearchPage{Start: 0, Count: 2, Total: 3, Items: []steam.SearchObservation{exact, tba}}); err != nil {
		t.Fatal(err)
	}
	var exactJobs, tbaJobs, tbaReservations int
	if err := db.Pool.QueryRow(context.Background(), `SELECT
		count(*) FILTER(WHERE payload->>'app_id'='632'),count(*) FILTER(WHERE payload->>'app_id'='633'),
		(SELECT count(*) FROM catalog_store_reservations WHERE steam_app_id=633)
		FROM sync_jobs WHERE kind='catalog.search.qualify'`).Scan(&exactJobs, &tbaJobs, &tbaReservations); err != nil {
		t.Fatal(err)
	}
	if exactJobs != 1 || tbaJobs != 0 || tbaReservations != 0 {
		t.Fatalf("early exact jobs=%d TBA jobs/reservations=%d/%d", exactJobs, tbaJobs, tbaReservations)
	}
}

type countedCatalogAI struct {
	ai.Client
	calls          atomic.Int32
	embeddingCalls atomic.Int32
	fail           bool
	failEmbedding  bool
}

func (c *countedCatalogAI) ExtractGameFeatures(_ context.Context, evidence ai.GameEvidence) (ai.GameFeatures, error) {
	c.calls.Add(1)
	if c.fail {
		return ai.GameFeatures{}, errors.New("catalog AI unavailable")
	}
	return ai.GameFeatures{Themes: []string{"adventure"}, Mechanics: []string{}, Modes: []string{}, Tone: []string{}, EvidenceKeys: []string{evidence.Evidence[0].Key}}, nil
}

func (c *countedCatalogAI) Embed(context.Context, []string) ([][]float32, error) {
	c.embeddingCalls.Add(1)
	if c.failEmbedding {
		return nil, errors.New("catalog embedding unavailable")
	}
	vector := make([]float32, ai.EmbeddingDimensions)
	return [][]float32{vector}, nil
}

func enqueueCatalogAITestRoot(t *testing.T, db *store.Store, planner SearchDiscoveryEnrichmentPlanner, id uuid.UUID, details steam.GameDetails, priority int) {
	t.Helper()
	snapshot, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(context.Background(), `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at,source_snapshot,availability)
		VALUES($1,$2,$3,'steam_search',now(),$4,'available')`, id, details.AppID, details.Name, snapshot); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := enqueueSearchDiscoveryFeatures(context.Background(), db.Queries.WithTx(tx), planner, id, details, priority); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogAIBudgetDefersAtExecutionWithPriorityAndRestart(t *testing.T) {
	db := openCatalogTestStore(t)
	cfg := DefaultDiscoveryConfig()
	cfg.AIHourlyBudget = 1
	queue := jobs.NewQueue(db)
	gate, err := NewDiscoveryService(NewDiscoveryRepository(db), &discoverySourceStub{}, &discoveryMetadata{}, queue, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &countedCatalogAI{}
	enrichment, err := ai.NewService(ai.NewSQLRepository(db), client, "text-v1", "embedding-v1")
	if err != nil {
		t.Fatal(err)
	}
	enrichment.SetQueue(queue)
	enrichment.SetCatalogFeatureAdmission(gate)
	for i, priority := range []int{10, 50, 80} {
		details := steam.GameDetails{AppID: int64(710 + i), Type: "game", Name: "Budget Priority", ShortDescription: string(rune('a' + i)),
			ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "Coming Soon"}}
		enqueueCatalogAITestRoot(t, db, enrichment, uuid.New(), details, priority)
	}
	worker := jobs.NewWorker(queue, jobs.LaneCatalog, "ai-budget", jobs.WithPollInterval(time.Millisecond))
	if err := enrichment.RegisterCatalogHandlers(worker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var succeeded, deferred int
		if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FILTER(WHERE status='succeeded'),
			count(*) FILTER(WHERE status='queued' AND available_at>now()) FROM sync_jobs WHERE kind='ai.features'`).Scan(&succeeded, &deferred); err != nil {
			cancel()
			t.Fatal(err)
		}
		if succeeded == 1 && deferred == 2 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("budget execution did not settle: succeeded=%d deferred=%d calls=%d", succeeded, deferred, client.calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var used, buckets, highestSucceeded, attempts int
	if err := db.Pool.QueryRow(context.Background(), `SELECT
		(SELECT used FROM catalog_budget_reservations WHERE kind='ai'),
		(SELECT count(*) FROM catalog_budget_reservations WHERE kind='ai'),
		(SELECT count(*) FROM sync_jobs WHERE kind='ai.features' AND priority=80 AND status='succeeded'),
		(SELECT COALESCE(sum(attempts),0) FROM sync_jobs WHERE kind='ai.features' AND status='queued')`).
		Scan(&used, &buckets, &highestSucceeded, &attempts); err != nil {
		t.Fatal(err)
	}
	if used != 1 || buckets != 1 || highestSucceeded != 1 || attempts != 0 || client.calls.Load() != 1 {
		t.Fatalf("used=%d buckets=%d highest=%d deferred attempts=%d calls=%d", used, buckets, highestSucceeded, attempts, client.calls.Load())
	}

	restarted, err := ai.NewService(ai.NewSQLRepository(db), client, "text-v1", "embedding-v1")
	if err != nil {
		t.Fatal(err)
	}
	restarted.SetQueue(queue)
	restarted.SetCatalogFeatureAdmission(gate)
	restartedWorker := jobs.NewWorker(queue, jobs.LaneCatalog, "ai-budget-restart", jobs.WithPollInterval(time.Millisecond))
	if err := restarted.RegisterCatalogHandlers(restartedWorker); err != nil {
		t.Fatal(err)
	}
	restartCtx, restartCancel := context.WithCancel(context.Background())
	restartDone := make(chan error, 1)
	go func() { restartDone <- restartedWorker.Run(restartCtx) }()
	time.Sleep(25 * time.Millisecond)
	restartCancel()
	if err := <-restartDone; err != nil {
		t.Fatal(err)
	}
	if client.calls.Load() != 1 {
		t.Fatalf("restart bypassed durable deferral: calls=%d", client.calls.Load())
	}
}

func TestCatalogAIDeadExecutionSchedulesRecoveryThroughRealWorker(t *testing.T) {
	db := openCatalogTestStore(t)
	cfg := DefaultDiscoveryConfig()
	cfg.AIHourlyBudget = 10
	queue := jobs.NewQueue(db)
	gate, err := NewDiscoveryService(NewDiscoveryRepository(db), &discoverySourceStub{}, &discoveryMetadata{}, queue, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &countedCatalogAI{fail: true}
	enrichment, err := ai.NewService(ai.NewSQLRepository(db), client, "text-v1", "embedding-v1")
	if err != nil {
		t.Fatal(err)
	}
	enrichment.SetQueue(queue)
	enrichment.SetCatalogFeatureAdmission(gate)
	details := steam.GameDetails{AppID: 720, Type: "game", Name: "AI Recovery", ShortDescription: "stable semantic identity",
		ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "Coming Soon"}}
	enqueueCatalogAITestRoot(t, db, enrichment, uuid.New(), details, 80)
	worker := jobs.NewWorker(queue, jobs.LaneCatalog, "ai-recovery", jobs.WithPollInterval(time.Millisecond),
		jobs.WithRetrySchedule(time.Millisecond, time.Millisecond, time.Millisecond), jobs.WithRetryJitter(func(delay time.Duration) time.Duration { return delay }))
	if err := enrichment.RegisterCatalogHandlers(worker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var dead, recovery, embeddings int
		if err := db.Pool.QueryRow(context.Background(), `SELECT
			count(*) FILTER(WHERE kind='ai.features' AND status='dead'),
			count(*) FILTER(WHERE kind='ai.features' AND status='queued' AND idempotency_key LIKE '%:2:%'),
			count(*) FILTER(WHERE kind='ai.embedding') FROM sync_jobs`).Scan(&dead, &recovery, &embeddings); err != nil {
			cancel()
			t.Fatal(err)
		}
		if dead == 1 && recovery == 1 {
			if embeddings != 0 {
				cancel()
				t.Fatalf("failed feature extraction created %d embeddings", embeddings)
			}
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("AI recovery generation not scheduled: dead=%d recovery=%d calls=%d", dead, recovery, client.calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var attempts, maxAttempts, budgetUsed int
	var availableAt time.Time
	if err := db.Pool.QueryRow(context.Background(), `SELECT attempts,max_attempts,available_at,
		(SELECT used FROM catalog_budget_reservations WHERE kind='ai') FROM sync_jobs
		WHERE kind='ai.features' AND idempotency_key LIKE '%:2:%'`).Scan(&attempts, &maxAttempts, &availableAt, &budgetUsed); err != nil {
		t.Fatal(err)
	}
	if client.calls.Load() != 4 || attempts != 0 || maxAttempts != 4 || budgetUsed != 1 || availableAt.Before(time.Now().UTC().Add(59*time.Minute)) {
		t.Fatalf("calls=%d attempts/max=%d/%d budget=%d available=%v", client.calls.Load(), attempts, maxAttempts, budgetUsed, availableAt)
	}
}

func TestCatalogAIEmbeddingDeathSchedulesOneRecoveryWithoutDuplicateEmbedding(t *testing.T) {
	db := openCatalogTestStore(t)
	cfg := DefaultDiscoveryConfig()
	cfg.AIHourlyBudget = 10
	queue := jobs.NewQueue(db)
	gate, err := NewDiscoveryService(NewDiscoveryRepository(db), &discoverySourceStub{}, &discoveryMetadata{}, queue, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &countedCatalogAI{failEmbedding: true}
	enrichment, err := ai.NewService(ai.NewSQLRepository(db), client, "text-v1", "embedding-v1")
	if err != nil {
		t.Fatal(err)
	}
	enrichment.SetQueue(queue)
	enrichment.SetCatalogFeatureAdmission(gate)
	details := steam.GameDetails{AppID: 721, Type: "game", Name: "Embedding Recovery", ShortDescription: "one semantic execution",
		ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "Coming Soon"}}
	enqueueCatalogAITestRoot(t, db, enrichment, uuid.New(), details, 80)
	worker := jobs.NewWorker(queue, jobs.LaneCatalog, "embedding-recovery", jobs.WithPollInterval(time.Millisecond),
		jobs.WithRetrySchedule(time.Millisecond, time.Millisecond, time.Millisecond), jobs.WithRetryJitter(func(delay time.Duration) time.Duration { return delay }))
	if err := enrichment.RegisterCatalogHandlers(worker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var deadEmbedding, embeddings, recovery int
		if err := db.Pool.QueryRow(context.Background(), `SELECT
			count(*) FILTER(WHERE kind='ai.embedding' AND status='dead'),
			count(*) FILTER(WHERE kind='ai.embedding'),
			count(*) FILTER(WHERE kind='ai.features' AND status='queued' AND idempotency_key LIKE '%:2:%')
			FROM sync_jobs`).Scan(&deadEmbedding, &embeddings, &recovery); err != nil {
			cancel()
			t.Fatal(err)
		}
		if deadEmbedding == 1 && recovery == 1 {
			if embeddings != 1 {
				cancel()
				t.Fatalf("terminal embedding created duplicate embedding jobs: %d", embeddings)
			}
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("embedding recovery generation not scheduled: dead=%d recovery=%d feature calls=%d embedding calls=%d",
				deadEmbedding, recovery, client.calls.Load(), client.embeddingCalls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var recoveryAttempts, recoveryMaxAttempts, budgetUsed int
	if err := db.Pool.QueryRow(context.Background(), `SELECT attempts,max_attempts,
		(SELECT used FROM catalog_budget_reservations WHERE kind='ai') FROM sync_jobs
		WHERE kind='ai.features' AND idempotency_key LIKE '%:2:%'`).Scan(&recoveryAttempts, &recoveryMaxAttempts, &budgetUsed); err != nil {
		t.Fatal(err)
	}
	if client.calls.Load() != 1 || client.embeddingCalls.Load() != 4 || recoveryAttempts != 0 || recoveryMaxAttempts != 4 || budgetUsed != 1 {
		t.Fatalf("feature/embedding calls=%d/%d recovery attempts/max=%d/%d budget=%d",
			client.calls.Load(), client.embeddingCalls.Load(), recoveryAttempts, recoveryMaxAttempts, budgetUsed)
	}
}

func TestSearchQualificationRetryGenerationSchedule(t *testing.T) {
	want := []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 16 * time.Hour, 24 * time.Hour, 24 * time.Hour}
	for generation, expected := range want {
		if got := searchQualificationRetryDelay(generation); got != expected {
			t.Fatalf("generation %d delay=%s want=%s", generation, got, expected)
		}
	}
}

func TestSearchQualificationPersistsCanonicalAndReviewState(t *testing.T) {
	observation := testObservation(620, "20 Oct, 2026", steam.ReviewPositive, "Very Positive")
	percent := 91
	observation.Review.PositivePercent = &percent
	observation.Review.Total = 1284
	details := steam.GameDetails{AppID: 620, Type: "game", Name: "Portal Future", ShortDescription: "A semantic description",
		ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "20 Oct, 2026"}, Platforms: steam.Platforms{Windows: true}}
	service, metadata, repo, lease := searchQualificationFixture(t, observation, details)
	planner := &searchQualificationPlanner{}
	service.SetSearchEnrichmentPlanner(planner)
	if err := service.handleSearchQualification(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	var name, reviewBand, reviewLabel, tombstone, semanticHash, reservationStatus, workStatus string
	var reviewPercent *int32
	var reviewTotal int64
	var active, comingSoon bool
	var releaseCount, featureJobs int
	err := repo.store.Pool.QueryRow(context.Background(), `SELECT g.name,c.review_band,c.review_label,c.positive_percent,c.review_total,
		c.active,c.coming_soon,c.tombstone_reason,c.semantic_content_hash,r.status,w.status,
		(SELECT count(*) FROM game_release_dates WHERE game_id=g.id),
		(SELECT count(*) FROM sync_jobs WHERE kind='feature.test')
		FROM games g JOIN catalog_search_current c ON c.steam_app_id=g.steam_app_id
		JOIN catalog_store_reservations r ON r.steam_app_id=g.steam_app_id
		JOIN catalog_search_worklist w ON w.steam_app_id=g.steam_app_id WHERE g.steam_app_id=620`).
		Scan(&name, &reviewBand, &reviewLabel, &reviewPercent, &reviewTotal, &active, &comingSoon, &tombstone, &semanticHash,
			&reservationStatus, &workStatus, &releaseCount, &featureJobs)
	if err != nil {
		t.Fatal(err)
	}
	if name != details.Name || reviewBand != "positive" || reviewLabel != "Very Positive" || reviewPercent == nil || *reviewPercent != 91 || reviewTotal != 1284 ||
		!active || !comingSoon || tombstone != "" || semanticHash != SemanticContentHash(details) || reservationStatus != "complete" ||
		workStatus != "complete" || releaseCount != 1 || featureJobs != 1 || planner.calls != 1 || metadata.deckCalls != 0 {
		t.Fatalf("canonical=%q review=%q/%q/%v/%d active=%v soon=%v tombstone=%q hash=%q reservation=%q work=%q releases=%d feature=%d calls=%d deck=%d",
			name, reviewBand, reviewLabel, reviewPercent, reviewTotal, active, comingSoon, tombstone, semanticHash,
			reservationStatus, workStatus, releaseCount, featureJobs, planner.calls, metadata.deckCalls)
	}
}

func TestLegacyQualificationV1RemainsOperationalAlongsideSearchQualification(t *testing.T) {
	service, metadata := discoveryFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	legacyApp := DiscoveredApp{AppID: 42, LastModified: now}
	legacy := qualificationLease(t, service, legacyApp, 0)
	if err := service.handleQualification(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := service.queue.Complete(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	repo := NewSearchRepository(service.repo.store)
	cfg := service.cfg
	scan, err := NewSearchScanService(repo, &searchStub{}, service.queue, cfg, service.now)
	if err != nil {
		t.Fatal(err)
	}
	enableSearchQualificationProducer(t, scan, repo)
	run := searchTestRun(t, repo, cfg, now.Add(time.Minute))
	observation := testObservation(43, "20 Oct, 2026", steam.ReviewUnknown, "")
	if err := scan.CommitPage(ctx, run, steam.FeedPopularUpcoming, 0, 0, steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{observation}}); err != nil {
		t.Fatal(err)
	}
	if err := scan.CommitPage(ctx, run, steam.FeedPopularNew, 0, 0, steam.SearchPage{Start: 0, Count: 0, Total: 0}); err != nil {
		t.Fatal(err)
	}
	metadata.details = steam.GameDetails{AppID: 43, Type: "game", Name: "Search Candidate", ShortDescription: "new path",
		ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "20 Oct, 2026"}}
	searchLease, err := service.queue.Claim(ctx, jobs.LaneCatalog, "coexistence", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if searchLease == nil || searchLease.Kind != SearchQualificationJobKind || searchLease.IdempotencyKey == legacy.IdempotencyKey {
		t.Fatalf("search lease=%+v legacy=%+v", searchLease, legacy)
	}
	if err := service.handleSearchQualification(ctx, *searchLease); err != nil {
		t.Fatal(err)
	}
	var games int
	if err := service.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM games WHERE steam_app_id IN (42,43)`).Scan(&games); err != nil {
		t.Fatal(err)
	}
	if games != 2 {
		t.Fatalf("games=%d", games)
	}
}

func TestStaleCandidateResultCannotPublish(t *testing.T) {
	observation := testObservation(621, "20 Oct, 2026", steam.ReviewPositive, "Positive")
	details := steam.GameDetails{AppID: 621, Type: "game", Name: "Stale", ShortDescription: "old",
		ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "20 Oct, 2026"}}
	service, _, repo, lease := searchQualificationFixture(t, observation, details)
	if _, err := repo.store.Pool.Exec(context.Background(), `UPDATE catalog_metadata_heads SET material_run_revision=material_run_revision+1,
		material_observation_ordinal=material_observation_ordinal+1,material_fingerprint='newer' WHERE steam_app_id=621`); err != nil {
		t.Fatal(err)
	}
	if err := service.handleSearchQualification(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	var games int
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM games WHERE steam_app_id=621`).Scan(&games); err != nil {
		t.Fatal(err)
	}
	if games != 0 {
		t.Fatal("stale candidate published canonical game")
	}
}

func TestStoreFailurePreservesCanonicalSnapshot(t *testing.T) {
	observation := testObservation(623, "20 Oct, 2026", steam.ReviewPositive, "Positive")
	details := steam.GameDetails{AppID: 623, Type: "game", Name: "Replacement", ShortDescription: "new",
		ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "20 Oct, 2026"}}
	service, metadata, repo, lease := searchQualificationFixture(t, observation, details)
	old := steam.GameDetails{AppID: 623, Type: "game", Name: "Last Known Good", ShortDescription: "old",
		ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "Coming Soon"}}
	snapshot, _ := json.Marshal(old)
	gameID := uuid.New()
	if _, err := repo.store.Pool.Exec(context.Background(), `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at,source_snapshot,availability)
		VALUES($1,623,$2,'steam_store',$3,$4,'available')`, gameID, old.Name, service.now().Add(-time.Hour), snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.Pool.Exec(context.Background(), `INSERT INTO catalog_entry_sources(game_id,source,active) VALUES($1,'steam_discovery',true)`, gameID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.Pool.Exec(context.Background(), `INSERT INTO catalog_entries(game_id) VALUES($1)`, gameID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.Pool.Exec(context.Background(), `UPDATE catalog_search_current SET active=true WHERE steam_app_id=623`); err != nil {
		t.Fatal(err)
	}
	metadata.err = steam.ErrUnavailable
	lease.Attempts = lease.MaxAttempts
	err := service.handleSearchQualification(context.Background(), lease)
	if err == nil {
		t.Fatal("terminal Store failure returned success")
	}
	type safeReasoner interface{ SafeReason() string }
	var safe safeReasoner
	if !errors.As(err, &safe) || safe.SafeReason() != "unavailable" {
		t.Fatalf("failure=%T %v", err, err)
	}
	var name, availability, reservationStatus, workStatus string
	var stored []byte
	var retryAt time.Time
	var generations int
	var currentActive, sourceActive bool
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT g.name,g.availability,g.source_snapshot,r.status,w.status
		,r.next_retry_at,(SELECT count(*) FROM sync_jobs WHERE kind='catalog.search.qualify')
		,c.active,src.active
		FROM games g JOIN catalog_store_reservations r ON r.steam_app_id=g.steam_app_id
		JOIN catalog_search_worklist w ON w.steam_app_id=g.steam_app_id
		JOIN catalog_search_current c ON c.steam_app_id=g.steam_app_id
		JOIN catalog_entry_sources src ON src.game_id=g.id AND src.source='steam_discovery' WHERE g.steam_app_id=623`).
		Scan(&name, &availability, &stored, &reservationStatus, &workStatus, &retryAt, &generations, &currentActive, &sourceActive); err != nil {
		t.Fatal(err)
	}
	var retained steam.GameDetails
	if err := json.Unmarshal(stored, &retained); err != nil {
		t.Fatal(err)
	}
	if name != old.Name || availability != "available" || retained.Name != old.Name || retained.ShortDescription != old.ShortDescription || reservationStatus != "reserved" || workStatus != "reserved" || !retryAt.Equal(service.now().Add(time.Hour)) || generations != 2 || !currentActive || !sourceActive {
		t.Fatalf("canonical=%q/%q/%s reservation=%q work=%q retry=%v generations=%d", name, availability, stored, reservationStatus, workStatus, retryAt, generations)
	}
}

func TestStoreRetryGenerationRetainsSingleWorkOwner(t *testing.T) {
	t.Run("generation zero terminal then generation one succeeds", func(t *testing.T) {
		observation := testObservation(629, "20 Oct, 2026", steam.ReviewPositive, "Positive")
		details := steam.GameDetails{AppID: 629, Type: "game", Name: "Retry Success", ShortDescription: "stable",
			ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "20 Oct, 2026"}}
		service, metadata, repo, first := searchQualificationFixture(t, observation, details)
		metadata.err = errors.New("temporary Store failure")
		first.Attempts = first.MaxAttempts
		if err := service.handleSearchQualification(context.Background(), first); err == nil {
			t.Fatal("generation zero terminal failure returned success")
		}
		if _, err := repo.store.Pool.Exec(context.Background(), `UPDATE sync_jobs SET attempts=max_attempts WHERE id=$1`, first.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := service.queue.Fail(context.Background(), first, errors.New("temporary Store failure"), service.now()); err != nil {
			t.Fatal(err)
		}
		metadata.err = nil
		next, err := service.queue.Claim(context.Background(), jobs.LaneCatalog, "store-retry-success", time.Minute)
		if err != nil || next == nil {
			t.Fatalf("claim generation one=%+v err=%v", next, err)
		}
		var payload SearchQualificationPayload
		if err := json.Unmarshal(next.Payload, &payload); err != nil || payload.RetryGeneration != 1 {
			t.Fatalf("generation one payload=%+v err=%v", payload, err)
		}
		if err := service.handleSearchQualification(context.Background(), *next); err != nil {
			t.Fatal(err)
		}
		if err := service.queue.Complete(context.Background(), *next); err != nil {
			t.Fatal(err)
		}
		var reservationStatus, workStatus string
		var retryAt *time.Time
		if err := repo.store.Pool.QueryRow(context.Background(), `SELECT r.status,w.status,r.next_retry_at
			FROM catalog_store_reservations r JOIN catalog_search_worklist w USING(steam_app_id) WHERE r.steam_app_id=629`).
			Scan(&reservationStatus, &workStatus, &retryAt); err != nil {
			t.Fatal(err)
		}
		if reservationStatus != "complete" || workStatus != "complete" || retryAt != nil {
			t.Fatalf("reservation=%q work=%q retry=%v", reservationStatus, workStatus, retryAt)
		}
	})

	t.Run("generation one terminal owns generation two until due", func(t *testing.T) {
		observation := testObservation(630, "20 Oct, 2026", steam.ReviewPositive, "Positive")
		details := steam.GameDetails{AppID: 630, Type: "game", Name: "Retry Twice", ShortDescription: "stable",
			ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "20 Oct, 2026"}}
		service, metadata, repo, first := searchQualificationFixture(t, observation, details)
		metadata.err = errors.New("temporary Store failure")
		first.Attempts = first.MaxAttempts
		if err := service.handleSearchQualification(context.Background(), first); err == nil {
			t.Fatal("generation zero terminal failure returned success")
		}
		if _, err := repo.store.Pool.Exec(context.Background(), `UPDATE sync_jobs SET attempts=max_attempts WHERE id=$1`, first.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := service.queue.Fail(context.Background(), first, errors.New("temporary Store failure"), service.now()); err != nil {
			t.Fatal(err)
		}
		second, err := service.queue.Claim(context.Background(), jobs.LaneCatalog, "store-retry-terminal", time.Minute)
		if err != nil || second == nil {
			t.Fatalf("claim generation one=%+v err=%v", second, err)
		}
		second.Attempts = second.MaxAttempts
		if err := service.handleSearchQualification(context.Background(), *second); err == nil {
			t.Fatal("generation one terminal failure returned success")
		}
		if _, err := repo.store.Pool.Exec(context.Background(), `UPDATE sync_jobs SET attempts=max_attempts WHERE id=$1`, second.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := service.queue.Fail(context.Background(), *second, errors.New("temporary Store failure"), service.now()); err != nil {
			t.Fatal(err)
		}
		due := service.now().Add(2 * time.Hour)
		count, err := repo.EnqueueReadySearchQualifications(context.Background(), service.queue, service.cfg, due)
		if err != nil || count != 0 {
			t.Fatalf("producer stole retry before lease expiry: count=%d err=%v", count, err)
		}
		var reservationStatus, workStatus string
		var retryGeneration int
		var retryAt, leaseUntil time.Time
		var generationTwoJobs int
		if err := repo.store.Pool.QueryRow(context.Background(), `SELECT r.status,w.status,r.retry_generation,r.next_retry_at,r.lease_until,
			(SELECT count(*) FROM sync_jobs WHERE kind='catalog.search.qualify' AND payload->>'retry_generation'='2')
			FROM catalog_store_reservations r JOIN catalog_search_worklist w USING(steam_app_id) WHERE r.steam_app_id=630`).
			Scan(&reservationStatus, &workStatus, &retryGeneration, &retryAt, &leaseUntil, &generationTwoJobs); err != nil {
			t.Fatal(err)
		}
		if reservationStatus != "reserved" || workStatus != "reserved" || retryGeneration != 2 || !retryAt.Equal(due) || !leaseUntil.After(due) || generationTwoJobs != 1 {
			t.Fatalf("reservation=%q work=%q generation=%d retry=%v lease=%v jobs=%d", reservationStatus, workStatus, retryGeneration, retryAt, leaseUntil, generationTwoJobs)
		}
	})
}

func TestStoreFailureLastKnownGoodVersusNeverSuccessful(t *testing.T) {
	for _, tc := range []struct {
		name, reason, wantTombstone string
	}{
		{name: "new unavailable", reason: "unavailable", wantTombstone: "unavailable"},
		{name: "new malformed", reason: "malformed", wantTombstone: "malformed"},
		{name: "new transport failure", reason: "request_failed", wantTombstone: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observation := testObservation(631, "20 Oct, 2026", steam.ReviewPositive, "Positive")
			details := steam.GameDetails{AppID: 631, Type: "game", Name: "Never Successful",
				ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "20 Oct, 2026"}}
			service, metadata, repo, lease := searchQualificationFixture(t, observation, details)
			lease.Attempts = lease.MaxAttempts
			switch tc.reason {
			case "unavailable":
				metadata.err = steam.ErrUnavailable
			case "request_failed":
				metadata.err = errors.New("temporary Store failure")
			case "malformed":
				metadata.details.Type = ""
			}
			if err := service.handleSearchQualification(context.Background(), lease); err == nil {
				t.Fatal("terminal failure returned success")
			}
			var active bool
			var tombstone, safeError string
			if err := repo.store.Pool.QueryRow(context.Background(), `SELECT active,tombstone_reason,safe_error
				FROM catalog_search_current WHERE steam_app_id=631`).Scan(&active, &tombstone, &safeError); err != nil {
				t.Fatal(err)
			}
			if active || tombstone != tc.wantTombstone || safeError != tc.reason {
				t.Fatalf("active=%v tombstone=%q safe_error=%q", active, tombstone, safeError)
			}
		})
	}
}

func TestTerminalStoreFailureSchedulesReviewOnlyMaterialRevision(t *testing.T) {
	observation := testObservation(628, "20 Oct, 2026", steam.ReviewPositive, "Positive")
	details := steam.GameDetails{AppID: 628, Type: "game", Name: "Review-only Retry",
		ShortDescription: "stable product metadata", ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "20 Oct, 2026"}}
	service, metadata, repo, lease := searchQualificationFixture(t, observation, details)

	var oldRun, oldOrdinal int64
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT material_run_revision,material_observation_ordinal
		FROM catalog_metadata_heads WHERE steam_app_id=628`).Scan(&oldRun, &oldOrdinal); err != nil {
		t.Fatal(err)
	}
	newRevision := CandidateRevision{RunRevision: oldRun, ObservationOrdinal: oldOrdinal + 1}
	const reviewFingerprint = "review-only-material-revision"
	updates := []string{
		`UPDATE catalog_metadata_heads SET latest_run_revision=$2,latest_observation_ordinal=$3,latest_fingerprint=$4,
			material_run_revision=$2,material_observation_ordinal=$3,material_fingerprint=$4 WHERE steam_app_id=$1`,
		`UPDATE catalog_search_current SET run_revision=$2,observation_ordinal=$3,fingerprint=$4,
			material_run_revision=$2,material_observation_ordinal=$3,material_fingerprint=$4,
			review_label='Very Positive',review_total=200 WHERE steam_app_id=$1`,
		`UPDATE catalog_search_worklist SET run_revision=$2,observation_ordinal=$3,
			material_run_revision=$2,material_observation_ordinal=$3,status='reserved' WHERE steam_app_id=$1`,
		`UPDATE catalog_store_reservations SET run_revision=$2,observation_ordinal=$3,fingerprint=$4 WHERE steam_app_id=$1`,
	}
	for _, update := range updates {
		args := []any{628, newRevision.RunRevision, newRevision.ObservationOrdinal}
		if strings.Contains(update, "$4") {
			args = append(args, reviewFingerprint)
		}
		if _, err := repo.store.Pool.Exec(context.Background(), update, args...); err != nil {
			t.Fatal(err)
		}
	}
	var reservationID, token uuid.UUID
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT id,fencing_token FROM catalog_store_reservations WHERE steam_app_id=628`).Scan(&reservationID, &token); err != nil {
		t.Fatal(err)
	}
	payload := SearchQualificationPayload{Version: 1, AppID: 628, CandidateRevision: newRevision,
		ReservationID: reservationID, ReservationToken: token, Priority: 80}
	spec, err := NewSearchQualificationSpec(payload, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	lease.Payload, lease.IdempotencyKey, lease.Attempts = spec.Payload, spec.IdempotencyKey, lease.MaxAttempts
	metadata.err = errors.New("temporary Store failure")
	err = service.handleSearchQualification(context.Background(), lease)
	if err == nil || strings.Contains(err.Error(), "retry fence lost") {
		t.Fatalf("terminal review-only failure=%v", err)
	}
	var workStatus string
	var retryAt time.Time
	var queuedRevision int64
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT w.status,w.next_retry_at,
		(SELECT (payload->'candidate_revision'->>'run_revision')::bigint FROM sync_jobs
		 WHERE kind='catalog.search.qualify' AND payload->>'retry_generation'='1')
		FROM catalog_search_worklist w WHERE w.steam_app_id=628`).Scan(&workStatus, &retryAt, &queuedRevision); err != nil {
		t.Fatal(err)
	}
	if workStatus != "reserved" || !retryAt.Equal(service.now().Add(time.Hour)) || queuedRevision != newRevision.RunRevision {
		t.Fatalf("work=%q retry=%v queued revision=%d", workStatus, retryAt, queuedRevision)
	}
}

func TestNonGameQualificationDeactivatesExistingSearchSource(t *testing.T) {
	observation := testObservation(627, "20 Oct, 2026", steam.ReviewPositive, "Positive")
	observation.Review.PositivePercent = intPointerCatalog(80)
	details := steam.GameDetails{AppID: 627, Type: "dlc", Name: "No Longer A Game", ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "20 Oct, 2026"}}
	service, _, repo, lease := searchQualificationFixture(t, observation, details)
	old := steam.GameDetails{AppID: 627, Type: "game", Name: "Last Good", ShortDescription: "preserve me", ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "20 Oct, 2026"}}
	snapshot, _ := json.Marshal(old)
	gameID := uuid.New()
	if _, err := repo.store.Pool.Exec(context.Background(), `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at,source_snapshot,availability)
		VALUES($1,627,$2,'steam_store',$3,$4,'available')`,
		gameID, old.Name, service.now().Add(-time.Hour), snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.Pool.Exec(context.Background(), `INSERT INTO catalog_entry_sources(game_id,source,active) VALUES($1,'steam_discovery',true)`, gameID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.Pool.Exec(context.Background(), `INSERT INTO catalog_entries(game_id) VALUES($1)`, gameID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.Pool.Exec(context.Background(), `UPDATE catalog_search_current SET active=true WHERE steam_app_id=627`); err != nil {
		t.Fatal(err)
	}
	if err := service.handleSearchQualification(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	var name, tombstone string
	var sourceActive, currentActive bool
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT g.name,src.active,c.active,c.tombstone_reason
		FROM games g JOIN catalog_entry_sources src ON src.game_id=g.id AND src.source='steam_discovery'
		JOIN catalog_search_current c ON c.steam_app_id=g.steam_app_id WHERE g.steam_app_id=627`).
		Scan(&name, &sourceActive, &currentActive, &tombstone); err != nil {
		t.Fatal(err)
	}
	if name != old.Name || sourceActive || currentActive || tombstone != "not_game" {
		t.Fatalf("name=%q sourceActive=%v currentActive=%v tombstone=%q", name, sourceActive, currentActive, tombstone)
	}
}

func TestMixedTombstoneReactivatesWithoutDuplicateAI(t *testing.T) {
	observation := testObservation(624, "17 Oct, 2026", steam.ReviewPositive, "Positive")
	observation.Review.PositivePercent = intPointerCatalog(80)
	details := steam.GameDetails{AppID: 624, Type: "game", Name: "Review Cycle", ShortDescription: "stable semantics",
		ReleaseDate: steam.ReleaseDate{ComingSoon: false, Raw: "17 Oct, 2026"}}
	service, _, repo, first := searchQualificationFixture(t, observation, details)
	planner := &searchQualificationPlanner{}
	service.SetSearchEnrichmentPlanner(planner)
	if err := service.handleSearchQualification(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := service.queue.Complete(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.Pool.Exec(context.Background(), `UPDATE sync_jobs SET status='succeeded',updated_at=now()
		WHERE kind='feature.test' AND status='queued'`); err != nil {
		t.Fatal(err)
	}

	second := enqueueReservedSearchQualification(t, service, repo, 624, CandidateRevision{RunRevision: firstRevision(t, repo, 624), ObservationOrdinal: 2}, "mixed-material", steam.ReviewSummary{Band: steam.ReviewMixedOrBelow, Label: "Mixed", Total: 100, PositivePercent: intPointerCatalog(55)})
	if err := service.handleSearchQualification(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := service.queue.Complete(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	var active bool
	var tombstone string
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT active,tombstone_reason FROM catalog_search_current WHERE steam_app_id=624`).Scan(&active, &tombstone); err != nil {
		t.Fatal(err)
	}
	if active || tombstone != "mixed_or_below" {
		t.Fatalf("mixed state active=%v tombstone=%q", active, tombstone)
	}

	third := enqueueReservedSearchQualification(t, service, repo, 624, CandidateRevision{RunRevision: firstRevision(t, repo, 624), ObservationOrdinal: 3}, "positive-material", steam.ReviewSummary{Band: steam.ReviewPositive, Label: "Positive", Total: 110, PositivePercent: intPointerCatalog(80)})
	if err := service.handleSearchQualification(context.Background(), third); err != nil {
		t.Fatal(err)
	}
	var featureJobs int
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT c.active,c.tombstone_reason,
		(SELECT count(*) FROM sync_jobs WHERE kind='feature.test') FROM catalog_search_current c WHERE c.steam_app_id=624`).Scan(&active, &tombstone, &featureJobs); err != nil {
		t.Fatal(err)
	}
	if !active || tombstone != "" || featureJobs != 1 || planner.calls != 2 {
		t.Fatalf("reactivated active=%v tombstone=%q featureJobs=%d plannerCalls=%d", active, tombstone, featureJobs, planner.calls)
	}
}

func TestSearchReviewBandChangesReuseCanonicalWithoutStoreOrDuplicateAI(t *testing.T) {
	observation := testObservation(625, "17 Oct, 2026", steam.ReviewPositive, "Positive")
	observation.Fingerprint = "stable-search-product"
	observation.Review.PositivePercent = intPointerCatalog(80)
	details := steam.GameDetails{AppID: 625, Type: "game", Name: "Cached Review Cycle", ShortDescription: "stable semantics",
		ReleaseDate: steam.ReleaseDate{ComingSoon: false, Raw: "17 Oct, 2026"}}
	service, metadata, repo, first := searchQualificationFixture(t, observation, details)
	userID := uuid.New()
	if _, err := repo.store.Pool.Exec(context.Background(), `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.Pool.Exec(context.Background(), `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, uuid.New(), userID); err != nil {
		t.Fatal(err)
	}
	planner := &searchQualificationPlanner{}
	service.SetSearchEnrichmentPlanner(planner)
	if err := service.handleSearchQualification(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := service.queue.Complete(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	assertDirtyRevision := func(want int64) {
		t.Helper()
		var got int64
		if err := repo.store.Pool.QueryRow(context.Background(), `SELECT dirty_revision FROM catalog_rebuild_state WHERE user_id=$1`, userID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("dirty revision=%d want=%d", got, want)
		}
	}
	assertDirtyRevision(1)
	cfg := service.cfg
	scan, err := NewSearchScanService(repo, &searchStub{}, service.queue, cfg, service.now)
	if err != nil {
		t.Fatal(err)
	}
	enableSearchQualificationProducer(t, scan, repo)
	now := service.now()
	mixed := observation
	mixed.Review = steam.ReviewSummary{Band: steam.ReviewMixedOrBelow, Label: "Mixed", Total: 100, PositivePercent: intPointerCatalog(55)}
	run := searchTestRun(t, repo, cfg, now.Add(time.Hour))
	if err := scan.CommitPage(context.Background(), run, steam.FeedRecent, 0, 0, steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{mixed}}); err != nil {
		t.Fatal(err)
	}
	var active bool
	var tombstone string
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT active,tombstone_reason FROM catalog_search_current WHERE steam_app_id=625`).Scan(&active, &tombstone); err != nil {
		t.Fatal(err)
	}
	if active || tombstone != "mixed_or_below" {
		t.Fatalf("mixed cached state active=%v tombstone=%q", active, tombstone)
	}
	assertDirtyRevision(2)
	positive := observation
	positive.Review.Total = 150
	run = searchTestRun(t, repo, cfg, now.Add(2*time.Hour))
	if err := scan.CommitPage(context.Background(), run, steam.FeedRecent, 0, 0, steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{positive}}); err != nil {
		t.Fatal(err)
	}
	var featureJobs, qualificationJobs, budgetUsed int
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT c.active,c.tombstone_reason,
		(SELECT count(*) FROM sync_jobs WHERE kind='feature.test'),
		(SELECT count(*) FROM sync_jobs WHERE kind='catalog.search.qualify'),
		(SELECT used FROM catalog_budget_reservations WHERE kind='appdetails' ORDER BY hour_at LIMIT 1)
		FROM catalog_search_current c WHERE c.steam_app_id=625`).Scan(&active, &tombstone, &featureJobs, &qualificationJobs, &budgetUsed); err != nil {
		t.Fatal(err)
	}
	if !active || tombstone != "" || featureJobs != 1 || qualificationJobs != 1 || budgetUsed != 1 || planner.calls != 1 || metadata.deckCalls != 0 {
		t.Fatalf("cached review state active=%v tombstone=%q feature=%d qualification=%d budget=%d planner=%d deck=%d",
			active, tombstone, featureJobs, qualificationJobs, budgetUsed, planner.calls, metadata.deckCalls)
	}
	assertDirtyRevision(3)

	sameBand := positive
	sameBand.Review = steam.ReviewSummary{Band: steam.ReviewPositive, Label: "Overwhelmingly Positive", Total: 1000, PositivePercent: intPointerCatalog(99)}
	run = searchTestRun(t, repo, cfg, now.Add(3*time.Hour))
	if err := scan.CommitPage(context.Background(), run, steam.FeedRecent, 0, 0, steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{sameBand}}); err != nil {
		t.Fatal(err)
	}
	assertDirtyRevision(3)
	if metadata.calls != 1 || planner.calls != 1 {
		t.Fatalf("same-band presentation refresh made Store/AI calls: store=%d planner=%d", metadata.calls, planner.calls)
	}

	// A due Store refresh can reveal semantic content that Search cannot encode.
	// The Search fingerprint stays identical, but the canonical semantic hash
	// must create a new ranking revision and one new enrichment root.
	metadata.details.ShortDescription = "meaningfully changed canonical semantics"
	if _, err := repo.store.Pool.Exec(context.Background(), `UPDATE catalog_search_current SET next_refresh_at=$2 WHERE steam_app_id=$1`, 625, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.Pool.Exec(context.Background(), `UPDATE sync_jobs SET status='succeeded',updated_at=now() WHERE kind='feature.test' AND status='queued'`); err != nil {
		t.Fatal(err)
	}
	var latestRun, latestOrdinal int64
	var latestRunID uuid.UUID
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT c.run_revision,c.observation_ordinal,o.run_id
		FROM catalog_search_current c JOIN catalog_search_observations o ON o.steam_app_id=c.steam_app_id
		 AND o.run_revision=c.run_revision AND o.observation_ordinal=c.observation_ordinal
		WHERE c.steam_app_id=625 LIMIT 1`).Scan(&latestRun, &latestOrdinal, &latestRunID); err != nil {
		t.Fatal(err)
	}
	semanticRevision := CandidateRevision{RunRevision: latestRun, ObservationOrdinal: latestOrdinal}
	reservation, err := repo.ReserveMetadata(context.Background(), 625, semanticRevision, observation.Fingerprint, service.now())
	if err != nil || !reservation.Granted {
		t.Fatalf("semantic reservation=%+v err=%v", reservation, err)
	}
	if _, err := repo.store.Pool.Exec(context.Background(), `UPDATE catalog_search_worklist SET status='reserved',
		material_run_revision=$3,material_observation_ordinal=$4 WHERE run_id=$1 AND steam_app_id=$2`,
		latestRunID, 625, reservation.Revision.RunRevision, reservation.Revision.ObservationOrdinal); err != nil {
		t.Fatal(err)
	}
	semanticPayload := SearchQualificationPayload{Version: 1, AppID: 625, CandidateRevision: reservation.Revision,
		ReservationID: reservation.ID, ReservationToken: reservation.FencingToken, Priority: 80}
	semanticSpec, err := NewSearchQualificationSpec(semanticPayload, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.queue.Enqueue(context.Background(), semanticSpec); err != nil {
		t.Fatal(err)
	}
	semanticLease, err := service.queue.Claim(context.Background(), jobs.LaneCatalog, "semantic-refresh", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if semanticLease == nil || semanticLease.Kind != SearchQualificationJobKind {
		t.Fatalf("semantic refresh claimed %+v", semanticLease)
	}
	semantic := *semanticLease
	if err := service.handleSearchQualification(context.Background(), semantic); err != nil {
		t.Fatal(err)
	}
	if err := service.queue.Complete(context.Background(), semantic); err != nil {
		t.Fatal(err)
	}
	assertDirtyRevision(4)
	if metadata.calls != 2 || planner.calls != 2 {
		t.Fatalf("semantic Store refresh calls: store=%d planner=%d", metadata.calls, planner.calls)
	}
}

func TestReleasedMixedSearchCandidateUsesTombstoneWithoutStoreBudget(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	cfg := DefaultDiscoveryConfig()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	scan, err := NewSearchScanService(repo, &searchStub{}, jobs.NewQueue(db), cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	enableSearchQualificationProducer(t, scan, repo)
	run := searchTestRun(t, repo, cfg, now)
	if err := scan.CommitPage(context.Background(), run, steam.FeedPopularUpcoming, 0, 0, steam.SearchPage{Start: 0, Count: 0, Total: 0}); err != nil {
		t.Fatal(err)
	}
	if err := scan.CommitPage(context.Background(), run, steam.FeedPopularNew, 0, 0, steam.SearchPage{Start: 0, Count: 0, Total: 0}); err != nil {
		t.Fatal(err)
	}
	mixed := testObservation(626, "17 Oct, 2026", steam.ReviewMixedOrBelow, "Mixed")
	mixed.Review.PositivePercent = intPointerCatalog(55)
	if err := scan.CommitPage(context.Background(), run, steam.FeedRecent, 0, 0, steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{mixed}}); err != nil {
		t.Fatal(err)
	}
	var tombstone, workStatus string
	var qualificationJobs, reservations, budgets int
	if err := db.Pool.QueryRow(context.Background(), `SELECT c.tombstone_reason,w.status,
		(SELECT count(*) FROM sync_jobs WHERE kind='catalog.search.qualify'),
		(SELECT count(*) FROM catalog_store_reservations),(SELECT count(*) FROM catalog_budget_reservations)
		FROM catalog_search_current c JOIN catalog_search_worklist w USING(steam_app_id) WHERE c.steam_app_id=626`).
		Scan(&tombstone, &workStatus, &qualificationJobs, &reservations, &budgets); err != nil {
		t.Fatal(err)
	}
	if tombstone != "mixed_or_below" || workStatus != "complete" || qualificationJobs != 0 || reservations != 0 || budgets != 0 {
		t.Fatalf("tombstone=%q work=%q qualification=%d reservations=%d budgets=%d", tombstone, workStatus, qualificationJobs, reservations, budgets)
	}
}

func firstRevision(t *testing.T, repo *SearchRepository, appID int64) int64 {
	t.Helper()
	var revision int64
	if err := repo.store.Pool.QueryRow(context.Background(), `SELECT material_run_revision FROM catalog_metadata_heads WHERE steam_app_id=$1`, appID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}

func intPointerCatalog(value int) *int { return &value }

func enqueueReservedSearchQualification(t *testing.T, service *DiscoveryService, repo *SearchRepository, appID int64, revision CandidateRevision, fingerprint string, review steam.ReviewSummary) jobs.Lease {
	t.Helper()
	if _, err := repo.store.Pool.Exec(context.Background(), `UPDATE catalog_search_current SET review_band=$2,review_label=$3,
		positive_percent=$4,review_total=$5 WHERE steam_app_id=$1`, appID, review.Band, review.Label, review.PositivePercent, review.Total); err != nil {
		t.Fatal(err)
	}
	reservation, err := repo.ReserveMetadata(context.Background(), appID, revision, fingerprint, service.now())
	if err != nil || !reservation.Granted {
		t.Fatalf("reservation=%+v err=%v", reservation, err)
	}
	if _, err := repo.store.Pool.Exec(context.Background(), `UPDATE catalog_search_worklist SET
		run_revision=$2,observation_ordinal=$3,material_run_revision=$2,material_observation_ordinal=$3,status='reserved'
		WHERE steam_app_id=$1`, appID, reservation.Revision.RunRevision, reservation.Revision.ObservationOrdinal); err != nil {
		t.Fatal(err)
	}
	payload := SearchQualificationPayload{Version: 1, AppID: appID, CandidateRevision: reservation.Revision,
		ReservationID: reservation.ID, ReservationToken: reservation.FencingToken, Priority: 80}
	spec, err := NewSearchQualificationSpec(payload, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.queue.Enqueue(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	lease, err := service.queue.Claim(context.Background(), jobs.LaneCatalog, "review-cycle", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if lease == nil || lease.Kind != SearchQualificationJobKind {
		t.Fatalf("claimed %+v", lease)
	}
	return *lease
}

func TestUnknownReviewPreservesLastKnownGood(t *testing.T) {
	db := openCatalogTestStore(t)
	repo := NewSearchRepository(db)
	cfg := DefaultDiscoveryConfig()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	run := searchTestRun(t, repo, cfg, now)
	scan, err := NewSearchScanService(repo, &searchStub{}, jobs.NewQueue(db), cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	positive := testObservation(622, "20 Oct, 2026", steam.ReviewPositive, "Positive")
	positive.Fingerprint = "stable"
	if err := scan.CommitPage(context.Background(), run, steam.FeedPopularUpcoming, 0, 0, steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{positive}}); err != nil {
		t.Fatal(err)
	}
	newer := searchTestRun(t, repo, cfg, now.Add(time.Hour))
	unknown := positive
	unknown.Review = steam.ReviewSummary{Band: steam.ReviewUnknown}
	if err := scan.CommitPage(context.Background(), newer, steam.FeedPopularUpcoming, 0, 0, steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{unknown}}); err != nil {
		t.Fatal(err)
	}
	var band, label string
	if err := db.Pool.QueryRow(context.Background(), `SELECT review_band,review_label FROM catalog_search_current WHERE steam_app_id=622`).Scan(&band, &label); err != nil {
		t.Fatal(err)
	}
	if band != "positive" || !strings.EqualFold(label, "positive") {
		t.Fatalf("review=%q/%q", band, label)
	}
}
