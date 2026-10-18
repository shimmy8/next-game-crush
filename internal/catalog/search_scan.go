package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

type SearchScanService struct {
	repo   *SearchRepository
	source steam.SearchCatalog
	queue  *jobs.Queue
	cfg    DiscoveryConfig
	now    func() time.Time
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
	return worker.Register(SearchPageJobKind, s.handlePage)
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
	var failure *catalogSearchFailure
	if !errors.As(err, &failure) {
		return err
	}
	if lease.Attempts < lease.MaxAttempts {
		return failure
	}
	if err := s.repo.finalizePageFailure(ctx, lease, payload, failure.reason, s.now().UTC()); err != nil {
		return err
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
	if err := s.repo.PrepareRun(ctx, run); err != nil {
		return err
	}
	_, err := s.queue.Enqueue(ctx, searchPageSpec(SearchPagePayload{Version: 1, RunID: run.ID, Feed: steam.FeedPopularUpcoming}))
	return err
}

func (s *SearchScanService) ProcessPage(ctx context.Context, payload SearchPagePayload) error {
	if !validSearchPagePayload(payload) {
		return errors.New("invalid search page payload")
	}
	pageKey := dbgen.GetCommittedSearchPageParams{RunID: catalogUUID(payload.RunID), Feed: string(payload.Feed), StartOffset: int32(payload.Start), PageGeneration: int32(payload.PageGeneration)}
	if _, err := s.repo.store.Queries.GetCommittedSearchPage(ctx, pageKey); err == nil {
		return s.afterPageCommit(ctx, payload.RunID, payload.Feed)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
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
	if err := s.commitPage(ctx, run, payload.Feed, payload.Start, payload.PageGeneration, page, runConfig); err != nil {
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
	if run.ID == uuid.Nil || run.Revision < 1 || !validSearchFeed(feed) || start < 0 || generation < 0 || page.Start != start || page.Count < 0 || len(page.Items) > page.Count || (page.HasMore && page.Count == 0) {
		return errors.New("invalid search page")
	}
	now := s.now().UTC()
	tx, err := s.repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.repo.store.Queries.WithTx(tx)
	state, err := q.LockSearchFeed(ctx, dbgen.LockSearchFeedParams{RunID: catalogUUID(run.ID), Feed: string(feed)})
	if err != nil {
		return err
	}
	if state.CursorOffset != int32(start) || state.PageGeneration != int32(generation) || (state.Status != "queued" && state.Status != "running") {
		if _, manifestErr := q.GetCommittedSearchPage(ctx, dbgen.GetCommittedSearchPageParams{RunID: catalogUUID(run.ID), Feed: string(feed), StartOffset: int32(start), PageGeneration: int32(generation)}); manifestErr == nil {
			return tx.Commit(ctx)
		}
		return errors.New("stale search page")
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
		if observation.AppID < 1 || observation.AppID > math.MaxUint32 {
			return errors.New("invalid search app id")
		}
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
		if _, err := q.UpsertCurrentSearchObservation(ctx, dbgen.UpsertCurrentSearchObservationParams{SteamAppID: observation.AppID, RunRevision: run.Revision, ObservationOrdinal: ordinal, ReleaseText: observation.ReleaseText, Precision: string(window.Precision), ReleaseDate: releaseDate, ReleaseYear: year, ReleaseMonth: month, ReleaseQuarter: quarter, Fingerprint: observation.Fingerprint, ReviewBand: string(observation.Review.Band), ReviewLabel: observation.Review.Label, PositivePercent: positive, ReviewTotal: observation.Review.Total, ReviewObservedAt: pgtype.Timestamptz{Time: now, Valid: true}, Priority: searchFeedPriority(feed)}); err != nil {
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
			created, err := q.UpsertSearchWorklist(ctx, dbgen.UpsertSearchWorklistParams{RunID: catalogUUID(run.ID), SteamAppID: observation.AppID, RunRevision: run.Revision, ObservationOrdinal: ordinal, Priority: searchFeedPriority(feed)})
			if err != nil {
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
	var cursor int
	var naturalEnd, monotonic, boundaryStop bool
	var capReason string
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT status,cursor_offset,natural_end,monotonic,boundary_stop,safety_cap_reason FROM catalog_search_feeds WHERE run_id=$1 AND feed=$2`, runID, feed).Scan(&status, &cursor, &naturalEnd, &monotonic, &boundaryStop, &capReason); err != nil {
		return err
	}
	if status == "running" {
		_, err = s.queue.Enqueue(ctx, searchPageSpec(SearchPagePayload{Version: 1, RunID: runID, Feed: feed, Start: cursor, PageGeneration: 0}))
		return err
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
		if err := s.repo.store.Pool.QueryRow(ctx, `SELECT status FROM catalog_search_feeds WHERE run_id=$1 AND feed=$2`, runID, searchFeedOrder[index+1]).Scan(&nextStatus); err != nil {
			return err
		}
		if nextStatus == "queued" {
			_, err = s.queue.Enqueue(ctx, searchPageSpec(SearchPagePayload{Version: 1, RunID: runID, Feed: searchFeedOrder[index+1]}))
			if err != nil {
				return err
			}
		}
	}
	_, err = s.repo.store.Queries.FinishSearchRunIfTerminal(ctx, dbgen.FinishSearchRunIfTerminalParams{CompletedAt: pgtype.Timestamptz{Time: s.now().UTC(), Valid: true}, RunID: catalogUUID(runID)})
	return err
}
