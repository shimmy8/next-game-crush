package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
	dbgen "next-game-crush/internal/store/sqlc"
)

var searchFeedOrder = []steam.SearchFeed{
	steam.FeedPopularUpcoming, steam.FeedPopularNew, steam.FeedRecent, steam.FeedComingSoon,
}

var errStaleSearchPage = errors.New("stale search page")

type SearchScanService struct {
	repo                             *SearchRepository
	source                           steam.SearchCatalog
	queue                            *jobs.Queue
	qualificationProducer            SearchQualificationProducer
	cfg                              DiscoveryConfig
	now                              func() time.Time
	afterPageCommitHook              func() error
	beforePagePersistHook            func() error
	afterReconciliationTerminalHook  func() error
	afterReconciliationSuccessorHook func() error
}

// SearchQualificationProducer is installed explicitly by the catalog worker
// composition. Keeping it optional lets callers that only persist pages (and
// Task 8's direct claim flow) retain ownership of when metadata is claimed.
type SearchQualificationProducer interface {
	EnqueueReadySearchQualifications(context.Context, *jobs.Queue, DiscoveryConfig, time.Time) (int, error)
	EnqueueReadyIndependentSearchQualifications(context.Context, *jobs.Queue, DiscoveryConfig, time.Time) (int, error)
}

type catalogSearchFailure struct {
	cause      error
	reason     string
	retryAfter time.Duration
}

func (e *catalogSearchFailure) Error() string      { return e.reason }
func (e *catalogSearchFailure) Unwrap() error      { return e.cause }
func (e *catalogSearchFailure) SafeReason() string { return e.reason }
func (e *catalogSearchFailure) Retryable() bool    { return true }
func (e *catalogSearchFailure) RetryAfter() (time.Duration, bool) {
	return e.retryAfter, e.retryAfter > 0
}

func classifyCatalogSearchFailure(err error) *catalogSearchFailure {
	failure := &catalogSearchFailure{cause: err, reason: "request_failed"}
	if errors.Is(err, steam.ErrUnavailable) {
		failure.reason = "unavailable"
	}
	if errors.Is(err, steam.ErrInvalidResponse) || errors.Is(err, steam.ErrResponseTooLarge) {
		failure.reason = "invalid_response"
	}
	var request *steam.RequestError
	if errors.As(err, &request) {
		failure.retryAfter = request.RetryAfter
		if request.StatusCode == 429 {
			failure.reason = "rate_limited"
		}
	}
	return failure
}

func NewSearchScanService(repo *SearchRepository, source steam.SearchCatalog, queue *jobs.Queue, cfg DiscoveryConfig, now func() time.Time) (*SearchScanService, error) {
	if repo == nil || repo.store == nil || source == nil || queue == nil {
		return nil, errors.New("search scan dependencies are required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &SearchScanService{repo: repo, source: source, queue: queue, cfg: cfg, now: now}, nil
}

func (s *SearchScanService) SetSearchQualificationProducer(producer SearchQualificationProducer) {
	s.qualificationProducer = producer
}

func validSearchFeed(feed steam.SearchFeed) bool {
	for _, candidate := range searchFeedOrder {
		if feed == candidate {
			return true
		}
	}
	return false
}

func isPopularFeed(feed steam.SearchFeed) bool {
	return feed == steam.FeedPopularUpcoming || feed == steam.FeedPopularNew
}

func searchFeedPriority(feed steam.SearchFeed) int16 {
	switch feed {
	case steam.FeedPopularUpcoming:
		return 80
	case steam.FeedPopularNew:
		return 70
	case steam.FeedRecent:
		return 60
	default:
		return 50
	}
}

func searchPageKey(payload SearchPagePayload) string {
	return fmt.Sprintf("catalog.search.page:v1:%s:%s:%d:retry:%d", payload.RunID, payload.Feed, payload.Start, payload.PageGeneration)
}

func searchPageSpec(payload SearchPagePayload) jobs.Spec {
	raw, _ := json.Marshal(payload)
	return jobs.Spec{Lane: jobs.LaneCatalog, Kind: SearchPageJobKind, Stage: "catalog_search_page", Payload: raw,
		IdempotencyKey: searchPageKey(payload), MaxAttempts: 4, Priority: int(searchFeedPriority(payload.Feed))}
}

func (s *SearchScanService) RegisterHandlers(worker *jobs.Worker) error {
	if worker == nil {
		return errors.New("search scan worker is required")
	}
	if err := worker.Register(SearchRunJobKind, s.handleRun); err != nil {
		return err
	}
	if err := worker.Register(SearchPageJobKind, s.handlePage); err != nil {
		return err
	}
	return worker.Register(ReconciliationPageJobKind, s.handleReconciliationPage)
}

func (s *SearchScanService) handleRun(ctx context.Context, lease jobs.Lease) error {
	var payload SearchRunPayload
	if strictDiscoveryPayload(lease.Payload, &payload) != nil || payload.Version != 1 || payload.RunID == uuid.Nil || payload.RunRevision < 1 || payload.Generation < 0 {
		return errors.New("invalid search run payload")
	}
	return s.ProcessRun(ctx, payload)
}

func (s *SearchScanService) handlePage(ctx context.Context, lease jobs.Lease) error {
	var payload SearchPagePayload
	if strictDiscoveryPayload(lease.Payload, &payload) != nil || !validSearchPagePayload(payload) || lease.IdempotencyKey != searchPageKey(payload) {
		return errors.New("invalid search page payload")
	}
	err := s.ProcessPage(ctx, payload)
	if err == nil {
		return nil
	}
	type nextEligible interface{ NextEligibleAt() time.Time }
	var throttled nextEligible
	if errors.As(err, &throttled) {
		at := throttled.NextEligibleAt().UTC()
		if at.After(time.Now().UTC()) {
			if deferErr := s.queue.DeferUntil(ctx, lease, at); deferErr != nil {
				return deferErr
			}
			return jobs.ErrDeferred
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, jobs.ErrLeaseLost) {
		return err
	}
	var failure *catalogSearchFailure
	reason := "request_failed"
	if errors.As(err, &failure) {
		reason = failure.reason
	} else {
		type safeReasoner interface{ SafeReason() string }
		var safe safeReasoner
		if errors.As(err, &safe) {
			switch safe.SafeReason() {
			case "unavailable", "request_failed", "invalid_response", "rate_limited", "retry_exhausted", "malformed":
				reason = safe.SafeReason()
			}
		}
	}
	if lease.Attempts < lease.MaxAttempts {
		if failure != nil {
			return failure
		}
		return jobs.NewCatalogRetryError(err, reason, nil)
	}
	if err := s.repo.finalizePageFailure(ctx, lease, payload, reason, s.now().UTC()); err != nil {
		return err
	}
	if s.qualificationProducer == nil {
		return jobs.ErrDeferred
	}
	if ready, readyErr := s.qualificationBoundaryReady(ctx, payload.RunID, payload.Feed); readyErr != nil {
		return readyErr
	} else if ready {
		if _, err := s.qualificationProducer.EnqueueReadySearchQualifications(ctx, s.queue, s.cfg, s.now().UTC()); err != nil {
			return err
		}
	}
	// The terminal lifecycle was completed transactionally above; suppress the
	// worker's separate Fail call, which no longer owns the cleared lease.
	return jobs.ErrDeferred
}

func validSearchPagePayload(payload SearchPagePayload) bool {
	return payload.Version == 1 && payload.RunID != uuid.Nil && validSearchFeed(payload.Feed) && payload.Start >= 0 && payload.PageGeneration >= 0
}

func (s *SearchScanService) ProcessRun(ctx context.Context, payload SearchRunPayload) error {
	if payload.Version != 1 || payload.RunID == uuid.Nil || payload.RunRevision < 1 || payload.Generation < 0 {
		return errors.New("invalid search run payload")
	}
	run := RunRef{ID: payload.RunID, Revision: payload.RunRevision, Generation: payload.Generation}
	runRow, err := s.repo.store.Queries.GetSearchRun(ctx, catalogUUID(payload.RunID))
	if err != nil {
		return err
	}
	if RunKind(runRow.Kind) == RunReconciliation {
		startedAt := s.now().UTC()
		command, err := s.repo.store.Pool.Exec(ctx, `UPDATE catalog_search_runs SET status='running',started_at=COALESCE(started_at,$2)
			WHERE id=$1 AND run_revision=$3 AND retry_generation=$4 AND status IN ('queued','running')`,
			payload.RunID, startedAt, payload.RunRevision, payload.Generation)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return jobs.ErrLeaseLost
		}
		if runRow.StartedAt.Valid {
			startedAt = runRow.StartedAt.Time.UTC()
		}
		page := ReconciliationPagePayload{Version: 1, RunID: run.ID, RunRevision: run.Revision, PageGeneration: run.Generation,
			ScanStartedAt: startedAt, NewAppCount: int(runRow.NewAppCount)}
		var priorPayload, priorResult []byte
		var priorStatus string
		var priorUsed int
		err = s.repo.store.Pool.QueryRow(ctx, `WITH previous AS (
			SELECT id,status,new_app_count FROM catalog_search_runs WHERE kind='reconciliation' AND id<>$2
			ORDER BY occurrence_at DESC,run_revision DESC LIMIT 1
		), continuations AS (
			SELECT j.payload,j.result,'failed'::text AS status,r.new_app_count AS used_count,0 AS source_order
			FROM sync_jobs j JOIN catalog_search_runs r ON r.id=$2
			WHERE j.kind=$1 AND j.payload->>'run_id'=$2::text
				AND (j.payload->>'page_generation')::integer<$3
				AND (j.result->>'continuation'='true' OR j.status='dead')
			UNION ALL
			SELECT j.payload,j.result,previous.status,previous.new_app_count,1 FROM previous
			JOIN sync_jobs j ON (j.payload->>'run_id')::uuid=previous.id
			WHERE j.kind=$1 AND previous.status IN ('partial','failed')
				AND (j.result->>'continuation'='true' OR j.status='dead')
		) SELECT payload,result,status,used_count FROM continuations ORDER BY source_order,
			(payload->>'page_generation')::integer DESC,(payload->>'page')::integer DESC LIMIT 1`,
			ReconciliationPageJobKind, payload.RunID, payload.Generation).Scan(&priorPayload, &priorResult, &priorStatus, &priorUsed)
		if err == nil {
			var previous ReconciliationPagePayload
			var manifest reconciliationManifest
			if strictDiscoveryPayload(priorPayload, &previous) != nil || strictDiscoveryPayload(priorResult, &manifest) != nil {
				return errors.New("invalid reconciliation continuation")
			}
			page.Since = previous.Since
			if !previous.ScanStartedAt.IsZero() {
				page.ScanStartedAt = previous.ScanStartedAt
			}
			if manifest.Continuation {
				page.AfterAppID = manifest.ResumeAfterAppID
			} else {
				page.AfterAppID = previous.AfterAppID
			}
			if priorStatus == "failed" {
				page.Page = previous.Page
				page.NewAppCount = priorUsed
				if manifest.Continuation {
					page.Page = manifest.ResumePage
					page.NewAppCount = manifest.NewAppsUsed
				}
				if _, err = s.repo.store.Pool.Exec(ctx, `UPDATE catalog_search_runs SET new_app_count=GREATEST(new_app_count,$2)
					WHERE id=$1 AND run_revision=$3 AND retry_generation=$4 AND status='running'`,
					payload.RunID, page.NewAppCount, payload.RunRevision, payload.Generation); err != nil {
					return err
				}
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		} else {
			var watermark *time.Time
			watermarkErr := s.repo.store.Pool.QueryRow(ctx, `SELECT COALESCE(NULLIF(page.payload->>'scan_started_at','')::timestamptz,
				r.started_at,r.occurrence_at) FROM catalog_search_runs r LEFT JOIN LATERAL (
					SELECT payload FROM sync_jobs WHERE kind=$2 AND payload->>'run_id'=r.id::text
					ORDER BY (payload->>'page_generation')::integer DESC,(payload->>'page')::integer DESC LIMIT 1
				) page ON true WHERE r.kind='reconciliation' AND r.id<>$1 AND r.status='complete'
				ORDER BY r.occurrence_at DESC,r.run_revision DESC LIMIT 1`, payload.RunID, ReconciliationPageJobKind).Scan(&watermark)
			if watermarkErr != nil && !errors.Is(watermarkErr, pgx.ErrNoRows) {
				return watermarkErr
			}
			if watermark != nil {
				page.Since = watermark.UTC().Add(-reconciliationWatermarkOverlap)
			}
		}
		_, err = s.queue.Enqueue(ctx, reconciliationPageSpec(page))
		return err
	}
	if err := s.repo.PrepareRun(ctx, run); err != nil {
		return err
	}
	var feed steam.SearchFeed
	var cursor, generation int
	err = s.repo.store.Pool.QueryRow(ctx, `SELECT feed,cursor_offset,page_generation FROM catalog_search_feeds
		WHERE run_id=$1 AND status IN ('queued','running')
		ORDER BY array_position(ARRAY['popular_upcoming','popular_new','recent','coming_soon']::text[],feed) LIMIT 1`, run.ID).
		Scan(&feed, &cursor, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = s.queue.Enqueue(ctx, searchPageSpec(SearchPagePayload{Version: 1, RunID: run.ID, Feed: feed, Start: cursor, PageGeneration: generation}))
	return err
}

func (s *SearchScanService) ProcessPage(ctx context.Context, payload SearchPagePayload) error {
	if !validSearchPagePayload(payload) {
		return errors.New("invalid search page payload")
	}
	pageKey := dbgen.GetCommittedSearchPageParams{RunID: catalogUUID(payload.RunID), Feed: string(payload.Feed), StartOffset: int32(payload.Start), PageGeneration: int32(payload.PageGeneration)}
	if manifest, err := s.repo.store.Queries.GetCommittedSearchPage(ctx, pageKey); err == nil {
		var cursor, feedGeneration, runGeneration int
		if stateErr := s.repo.store.Pool.QueryRow(ctx, `SELECT f.cursor_offset,f.page_generation,r.retry_generation
			FROM catalog_search_feeds f JOIN catalog_search_runs r ON r.id=f.run_id
			WHERE f.run_id=$1 AND f.feed=$2`, payload.RunID, payload.Feed).
			Scan(&cursor, &feedGeneration, &runGeneration); stateErr != nil {
			return stateErr
		}
		if cursor != int(manifest.NextOffset) || feedGeneration != payload.PageGeneration || runGeneration != payload.PageGeneration {
			return nil
		}
		return s.afterPageCommit(ctx, payload.RunID, payload.Feed)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var feedStatus, runStatus string
	var cursor, feedGeneration, runGeneration int
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT f.status,f.cursor_offset,f.page_generation,r.status,r.retry_generation
		FROM catalog_search_feeds f JOIN catalog_search_runs r ON r.id=f.run_id
		WHERE f.run_id=$1 AND f.feed=$2`, payload.RunID, payload.Feed).
		Scan(&feedStatus, &cursor, &feedGeneration, &runStatus, &runGeneration); err != nil {
		return err
	}
	if (feedStatus != "queued" && feedStatus != "running") || (runStatus != "queued" && runStatus != "running") ||
		cursor != payload.Start || feedGeneration != payload.PageGeneration || feedGeneration != runGeneration {
		// A prior occurrence generation already superseded this durable job.
		// Completing it as an idempotent no-op prevents stale queue rows from
		// consuming their retry budget while the current generation resumes.
		return nil
	}
	runRow, err := s.repo.store.Queries.GetSearchRun(ctx, catalogUUID(payload.RunID))
	if err != nil {
		return err
	}
	runConfig := DiscoveryConfig{}
	if err := json.Unmarshal(runRow.ConfigSnapshot, &runConfig); err != nil || runConfig.Validate() != nil {
		return errors.New("invalid search run configuration")
	}
	page, err := s.source.Search(ctx, steam.SearchRequest{Feed: payload.Feed, Start: payload.Start, Count: runConfig.SearchPageSize})
	if err != nil {
		return classifyCatalogSearchFailure(err)
	}
	if page.Start != payload.Start || page.Count < 0 || page.Count > runConfig.SearchPageSize || len(page.Items) > page.Count || (page.HasMore && (page.Count == 0 || payload.Start > math.MaxInt-page.Count)) {
		return classifyCatalogSearchFailure(steam.ErrInvalidResponse)
	}
	run := searchRunRef(runRow)
	if err := s.commitPage(ctx, run, payload.Feed, payload.Start, payload.PageGeneration, page, runConfig); errors.Is(err, errStaleSearchPage) {
		return nil
	} else if err != nil {
		return err
	}
	return nil
}

// CommitPage is the durable crash boundary. It stores the manifest,
// observations, current projection, worklist and cursor in one transaction.
func (s *SearchScanService) CommitPage(ctx context.Context, run RunRef, feed steam.SearchFeed, start, generation int, page steam.SearchPage) error {
	return s.commitPage(ctx, run, feed, start, generation, page, s.cfg)
}

func (s *SearchScanService) commitPage(ctx context.Context, run RunRef, feed steam.SearchFeed, start, generation int, page steam.SearchPage, cfg DiscoveryConfig) error {
	if run.ID == uuid.Nil || run.Revision < 1 || run.Generation < 0 || run.Generation != generation || !validSearchFeed(feed) || start < 0 || generation < 0 || page.Start != start || page.Count < 0 || len(page.Items) > page.Count || (page.HasMore && page.Count == 0) {
		return errors.New("invalid search page")
	}
	now := s.now().UTC()
	tx, err := s.repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.repo.store.Queries.WithTx(tx)
	runRow, err := q.LockSearchRun(ctx, catalogUUID(run.ID))
	if err != nil {
		return err
	}
	if runRow.RunRevision != run.Revision || int(runRow.RetryGeneration) != generation ||
		(runRow.Status != "queued" && runRow.Status != "running") {
		return errStaleSearchPage
	}
	state, err := q.LockSearchFeed(ctx, dbgen.LockSearchFeedParams{RunID: catalogUUID(run.ID), Feed: string(feed)})
	if err != nil {
		return err
	}
	if state.CursorOffset != int32(start) || state.PageGeneration != int32(generation) || (state.Status != "queued" && state.Status != "running") {
		if _, manifestErr := q.GetCommittedSearchPage(ctx, dbgen.GetCommittedSearchPageParams{RunID: catalogUUID(run.ID), Feed: string(feed), StartOffset: int32(start), PageGeneration: int32(generation)}); manifestErr == nil {
			if err := tx.Commit(ctx); err != nil {
				return err
			}
			return s.afterPageCommit(ctx, run.ID, feed)
		}
		return errors.New("stale search page")
	}
	if s.beforePagePersistHook != nil {
		if err := s.beforePagePersistHook(); err != nil {
			return err
		}
	}
	appIDs := make([]int64, 0, len(page.Items))
	seenAppIDs := make(map[int64]struct{}, len(page.Items))
	for _, observation := range page.Items {
		if observation.AppID < 1 || observation.AppID > math.MaxUint32 {
			return errors.New("invalid search app id")
		}
		if _, exists := seenAppIDs[observation.AppID]; !exists {
			seenAppIDs[observation.AppID] = struct{}{}
			appIDs = append(appIDs, observation.AppID)
		}
	}
	sort.Slice(appIDs, func(i, j int) bool { return appIDs[i] < appIDs[j] })
	for _, appID := range appIDs {
		if err := q.LockMetadataApp(ctx, appID); err != nil {
			return err
		}
	}
	ordinal, err := q.AllocateObservationOrdinal(ctx, catalogUUID(run.ID))
	if err != nil {
		return err
	}
	next := start + page.Count
	if _, err := q.CommitSearchPage(ctx, dbgen.CommitSearchPageParams{RunID: catalogUUID(run.ID), Feed: string(feed), StartOffset: int32(start), PageGeneration: int32(generation), ObservationOrdinal: ordinal, NextOffset: int32(next), ResultCount: int32(page.Count), HasMore: page.HasMore}); err != nil {
		return err
	}

	monotonic, lastDate, crossed := validatePageOrdering(feed, state.LastOrderingDate, page.Items, now, cfg)
	workCount, err := q.CountSearchRunWorklist(ctx, catalogUUID(run.ID))
	if err != nil {
		return err
	}
	skippedForCap := false
	for _, observation := range page.Items {
		window := ParseReleaseDate(observation.ReleaseText, now)
		positive := pgtype.Int4{}
		if observation.Review.PositivePercent != nil {
			positive = pgtype.Int4{Int32: int32(*observation.Review.PositivePercent), Valid: true}
		}
		releaseDate := pgtype.Date{}
		if window.Date != nil {
			releaseDate = pgtype.Date{Time: *window.Date, Valid: true}
		}
		year, month, quarter := pgtype.Int4{}, pgtype.Int4{}, pgtype.Int4{}
		if window.Year != 0 {
			year = pgtype.Int4{Int32: int32(window.Year), Valid: true}
		}
		if window.Month != 0 {
			month = pgtype.Int4{Int32: int32(window.Month), Valid: true}
		}
		if window.Quarter != 0 {
			quarter = pgtype.Int4{Int32: int32(window.Quarter), Valid: true}
		}
		params := dbgen.InsertSearchObservationParams{RunID: catalogUUID(run.ID), Feed: string(feed), StartOffset: int32(start), PageGeneration: int32(generation), SteamAppID: observation.AppID, RunRevision: run.Revision, ObservationOrdinal: ordinal, Position: int32(observation.Position), ReleaseText: observation.ReleaseText, Precision: string(window.Precision), Fingerprint: observation.Fingerprint, ReviewBand: string(observation.Review.Band), ReviewLabel: observation.Review.Label, PositivePercent: positive, ReviewTotal: observation.Review.Total, ObservedAt: pgtype.Timestamptz{Time: now, Valid: true}}
		if _, err := q.InsertSearchObservation(ctx, params); err != nil {
			return err
		}
		material, err := q.UpsertMetadataCandidateHead(ctx, dbgen.UpsertMetadataCandidateHeadParams{
			SteamAppID: observation.AppID, RunRevision: run.Revision, ObservationOrdinal: ordinal,
			Fingerprint: observation.Fingerprint, NowAt: pgtype.Timestamptz{Time: now, Valid: true},
		})
		if err != nil {
			return err
		}
		if _, err := q.UpsertCurrentSearchObservation(ctx, dbgen.UpsertCurrentSearchObservationParams{SteamAppID: observation.AppID, RunRevision: run.Revision, ObservationOrdinal: ordinal, ReleaseText: observation.ReleaseText, Precision: string(window.Precision), ReleaseDate: releaseDate, ReleaseYear: year, ReleaseMonth: month, ReleaseQuarter: quarter, Fingerprint: observation.Fingerprint, ReviewBand: string(observation.Review.Band), ReviewLabel: observation.Review.Label, PositivePercent: positive, ReviewTotal: observation.Review.Total, ReviewObservedAt: pgtype.Timestamptz{Time: now, Valid: true}, Priority: searchFeedPriority(feed)}); err != nil {
			return err
		}
		if _, err := q.SyncCurrentMaterialCandidate(ctx, dbgen.SyncCurrentMaterialCandidateParams{RunRevision: pgtype.Int8{Int64: material.MaterialRunRevision, Valid: true}, ObservationOrdinal: pgtype.Int8{Int64: material.MaterialObservationOrdinal, Valid: true}, Fingerprint: pgtype.Text{String: material.MaterialFingerprint, Valid: true}, SteamAppID: observation.AppID}); err != nil {
			return err
		}
		if _, err := q.SupersedeStaleMetadataWorkForApp(ctx, dbgen.SupersedeStaleMetadataWorkForAppParams{SteamAppID: observation.AppID, RunRevision: material.MaterialRunRevision, ObservationOrdinal: material.MaterialObservationOrdinal}); err != nil {
			return err
		}
		if _, err := q.SupersedeStaleMetadataIngressWork(ctx, dbgen.SupersedeStaleMetadataIngressWorkParams{SteamAppID: observation.AppID, RunRevision: material.MaterialRunRevision, ObservationOrdinal: material.MaterialObservationOrdinal, Fingerprint: material.MaterialFingerprint, NowAt: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil {
			return err
		}
		shouldUpsert := int(workCount) < cfg.SearchMaxNewAppsPerRun
		if !shouldUpsert {
			shouldUpsert, err = q.SearchWorklistContains(ctx, dbgen.SearchWorklistContainsParams{RunID: catalogUUID(run.ID), SteamAppID: observation.AppID})
			if err != nil {
				return err
			}
		}
		if shouldUpsert {
			created, err := q.UpsertSearchWorklist(ctx, dbgen.UpsertSearchWorklistParams{RunID: catalogUUID(run.ID), SteamAppID: observation.AppID, RunRevision: run.Revision, ObservationOrdinal: ordinal, Priority: searchFeedPriority(feed), MaterialRunRevision: material.MaterialRunRevision, MaterialObservationOrdinal: material.MaterialObservationOrdinal})
			if err != nil {
				return err
			}
			if err := q.RecordMetadataProvenance(ctx, dbgen.RecordMetadataProvenanceParams{
				SteamAppID: observation.AppID, IngressKind: runRow.Kind, IngressIdentity: run.ID.String(),
				RunRevision: run.Revision, ObservationOrdinal: ordinal,
				FirstSeenAt: pgtype.Timestamptz{Time: now, Valid: true},
			}); err != nil {
				return err
			}
			if created {
				workCount++
			}
		} else {
			skippedForCap = true
		}
	}
	if err := q.UpdateSearchRunNewAppCount(ctx, dbgen.UpdateSearchRunNewAppCountParams{ID: catalogUUID(run.ID), NewAppCount: workCount}); err != nil {
		return err
	}
	naturalEnd := !page.HasMore
	boundaryStop := !isPopularFeed(feed) && page.HasMore && state.Monotonic && monotonic && crossed
	status, capReason := "running", ""
	if naturalEnd || boundaryStop {
		status = "complete"
	}
	if page.HasMore && int(state.Pages)+1 >= cfg.SearchMaxPagesPerFeed {
		status, capReason, boundaryStop = "partial", "page_limit", false
	}
	if skippedForCap || (page.HasMore && int(workCount) >= cfg.SearchMaxNewAppsPerRun) {
		status, capReason, boundaryStop = "partial", "new_app_limit", false
	}
	last := pgtype.Date{}
	if !lastDate.IsZero() {
		last = pgtype.Date{Time: lastDate, Valid: true}
	}
	rows, err := q.RecordSearchFeedProgress(ctx, dbgen.RecordSearchFeedProgressParams{Status: status, CursorOffset: int32(next), ObservedAt: pgtype.Timestamptz{Time: now, Valid: true}, ObservedCount: int32(len(page.Items)), NaturalEnd: naturalEnd, Monotonic: monotonic, BoundaryStop: boundaryStop, LastOrderingDate: last, SafetyCapReason: capReason, RunID: catalogUUID(run.ID), Feed: string(feed), ExpectedOffset: int32(start), PageGeneration: int32(generation)})
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("search feed cursor lost")
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if s.afterPageCommitHook != nil {
		if err := s.afterPageCommitHook(); err != nil {
			return err
		}
	}
	return s.afterPageCommit(ctx, run.ID, feed)
}

func validatePageOrdering(feed steam.SearchFeed, previous pgtype.Date, items []steam.SearchObservation, now time.Time, cfg DiscoveryConfig) (bool, time.Time, bool) {
	if isPopularFeed(feed) {
		return true, time.Time{}, false
	}
	monotonic := true
	var last time.Time
	if previous.Valid {
		last = utcCalendarDay(previous.Time)
	}
	for _, item := range items {
		window := ParseReleaseDate(item.ReleaseText, now)
		if window.Precision != PrecisionDay || window.Date == nil {
			monotonic = false
			continue
		}
		current := utcCalendarDay(*window.Date)
		if !last.IsZero() && ((feed == steam.FeedRecent && current.After(last)) || (feed == steam.FeedComingSoon && current.Before(last))) {
			monotonic = false
		}
		last = current
	}
	if !monotonic || last.IsZero() {
		return false, last, false
	}
	today := utcCalendarDay(now)
	if feed == steam.FeedRecent {
		return true, last, last.Before(today.AddDate(0, 0, -cfg.LookbackDays))
	}
	return true, last, last.After(today.AddDate(0, 0, cfg.HorizonDays))
}

func (s *SearchScanService) afterPageCommit(ctx context.Context, runID uuid.UUID, feed steam.SearchFeed) error {
	runRow, err := s.repo.store.Queries.GetSearchRun(ctx, catalogUUID(runID))
	if err != nil {
		return err
	}
	run := searchRunRef(runRow)
	var status string
	var cursor, pageGeneration int
	var naturalEnd, monotonic, boundaryStop bool
	var capReason string
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT status,cursor_offset,page_generation,natural_end,monotonic,boundary_stop,safety_cap_reason FROM catalog_search_feeds WHERE run_id=$1 AND feed=$2`, runID, feed).Scan(&status, &cursor, &pageGeneration, &naturalEnd, &monotonic, &boundaryStop, &capReason); err != nil {
		return err
	}
	// These candidates do not depend on Popular New/TBA authority, so they can
	// begin after every durable page. Popularity-dependent work remains pending
	// until the safe feed-completion boundary below.
	if s.qualificationProducer != nil {
		if _, err = s.qualificationProducer.EnqueueReadyIndependentSearchQualifications(ctx, s.queue, s.cfg, s.now().UTC()); err != nil {
			return err
		}
	}
	if status == "running" {
		if _, err = s.queue.Enqueue(ctx, searchPageSpec(SearchPagePayload{Version: 1, RunID: runID, Feed: feed, Start: cursor, PageGeneration: pageGeneration})); err != nil {
			return err
		}
		return nil
	}
	if status == "complete" {
		if _, err := s.repo.PublishFeed(ctx, run, feed, FeedCompletion{Status: status, NaturalEnd: naturalEnd, Monotonic: monotonic, BoundaryStop: boundaryStop, SafetyCapReason: capReason}); err != nil {
			return err
		}
	}
	for index, candidate := range searchFeedOrder {
		if candidate != feed || index+1 >= len(searchFeedOrder) {
			continue
		}
		var nextStatus string
		var nextCursor, nextGeneration int
		if err := s.repo.store.Pool.QueryRow(ctx, `SELECT status,cursor_offset,page_generation FROM catalog_search_feeds WHERE run_id=$1 AND feed=$2`, runID, searchFeedOrder[index+1]).Scan(&nextStatus, &nextCursor, &nextGeneration); err != nil {
			return err
		}
		if nextStatus == "queued" {
			_, err = s.queue.Enqueue(ctx, searchPageSpec(SearchPagePayload{Version: 1, RunID: runID, Feed: searchFeedOrder[index+1], Start: nextCursor, PageGeneration: nextGeneration}))
			if err != nil {
				return err
			}
		}
	}
	// Popular Upcoming alone is not a safe qualification boundary: TBA and
	// unrated rules depend on the authoritative Popular New publication too.
	// Once both popular feeds are terminal, later ordinary completions can
	// safely expose their tails and re-evaluate the accumulated worklist.
	if ready, readyErr := s.qualificationBoundaryReady(ctx, runID, feed); readyErr != nil {
		return readyErr
	} else if ready {
		if _, err = s.qualificationProducer.EnqueueReadySearchQualifications(ctx, s.queue, s.cfg, s.now().UTC()); err != nil {
			return err
		}
	}
	_, err = s.repo.store.Queries.FinishSearchRunIfTerminal(ctx, dbgen.FinishSearchRunIfTerminalParams{CompletedAt: pgtype.Timestamptz{Time: s.now().UTC(), Valid: true}, RunID: catalogUUID(runID)})
	return err
}

func (s *SearchScanService) qualificationBoundaryReady(ctx context.Context, runID uuid.UUID, feed steam.SearchFeed) (bool, error) {
	if s.qualificationProducer == nil {
		return false, nil
	}
	if feed == steam.FeedPopularUpcoming {
		return false, nil
	}
	if feed != steam.FeedPopularNew {
		// Every ordinary completion re-evaluates the tail against the already
		// published popular authority. Production priorities publish the popular
		// pair first; direct/reconciliation runs can reuse the prior publication.
		return true, nil
	}
	var terminal bool
	err := s.repo.store.Pool.QueryRow(ctx, `SELECT count(*)=2 AND bool_and(status IN ('complete','partial','failed'))
		FROM catalog_search_feeds WHERE run_id=$1 AND feed IN ('popular_upcoming','popular_new')`, runID).Scan(&terminal)
	return terminal, err
}
