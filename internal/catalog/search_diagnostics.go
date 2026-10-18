package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/store"
)

type SearchDiagnosticsStatus string

const (
	SearchDiagnosticsReady       SearchDiagnosticsStatus = "ready"
	SearchDiagnosticsBackfilling SearchDiagnosticsStatus = "backfilling"
	SearchDiagnosticsStale       SearchDiagnosticsStatus = "stale"
	SearchDiagnosticsUnavailable SearchDiagnosticsStatus = "unavailable"
	SearchDiagnosticsDisabled    SearchDiagnosticsStatus = "disabled"
)

var SearchDiagnosticFeeds = [...]string{"popular_upcoming", "popular_new", "recent", "coming_soon"}

type SearchDiagnosticsConfig struct {
	Enabled            bool
	StoreBudgetPerHour int
	AIBudgetPerHour    int
	StaleAfter         time.Duration
	Now                func() time.Time
}

type SearchRunDiagnostic struct {
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	OccurredAt  time.Time  `json:"occurredAt"`
	CompletedAt *time.Time `json:"completedAt"`
	SafeReason  string     `json:"reason,omitempty"`
}

type SearchFeedDiagnostic struct {
	Status       string `json:"status"`
	Pages        int64  `json:"pages"`
	Discovered   int64  `json:"discovered"`
	Deduplicated int64  `json:"deduplicated"`
	Active       int64  `json:"active"`
	BoundaryStop bool   `json:"boundaryStop"`
	SafetyCap    string `json:"safetyCap,omitempty"`
	SafeReason   string `json:"reason,omitempty"`
}

type LaneDiagnostic struct {
	Queued   int64 `json:"queued"`
	Running  int64 `json:"running"`
	Deferred int64 `json:"deferred"`
	Failed   int64 `json:"failed"`
}

type SearchLaneDiagnostics struct {
	Interactive LaneDiagnostic `json:"interactive"`
	Catalog     LaneDiagnostic `json:"catalog"`
}

type BudgetDiagnostic struct {
	Used      int64 `json:"used"`
	Remaining int64 `json:"remaining"`
	Limit     int64 `json:"limit"`
}

type StoreBudgetDiagnostic struct {
	BudgetDiagnostic
	Deferred int64 `json:"deferred"`
}

type SearchBudgetDiagnostics struct {
	Store StoreBudgetDiagnostic `json:"store"`
	AI    BudgetDiagnostic      `json:"ai"`
}

type ThrottleDiagnostic struct {
	Concurrency int64      `json:"concurrency"`
	NextAt      *time.Time `json:"nextAt"`
}

type AdmissionDiagnostic struct {
	Upcoming int64 `json:"upcoming"`
	Released int64 `json:"released"`
}

type RejectionDiagnostic struct {
	ReviewRejected int64 `json:"reviewRejected"`
	UnratedGrace   int64 `json:"unratedGrace"`
	OutsideWindow  int64 `json:"outsideWindow"`
	Unavailable    int64 `json:"unavailable"`
	Failed         int64 `json:"failed"`
}

type SearchCacheDiagnostic struct {
	MetadataEntries int64 `json:"metadataEntries"`
	AIEntries       int64 `json:"aiEntries"`
}

type SearchDueDiagnostic struct {
	Count  int64      `json:"count"`
	NextAt *time.Time `json:"nextAt"`
}

type SearchRebuildDiagnostic struct {
	DirtyRevision  int64 `json:"dirtyRevision"`
	PendingWakeups int64 `json:"pendingWakeups"`
}

type SearchDiagnostics struct {
	Status                    SearchDiagnosticsStatus         `json:"status"`
	LastScheduledOccurrenceAt *time.Time                      `json:"lastScheduledOccurrenceAt"`
	LastCompleteRun           *SearchRunDiagnostic            `json:"lastCompleteRun"`
	LastPartialRun            *SearchRunDiagnostic            `json:"lastPartialRun"`
	LastFailedRun             *SearchRunDiagnostic            `json:"lastFailedRun"`
	Feeds                     map[string]SearchFeedDiagnostic `json:"feeds"`
	Lanes                     SearchLaneDiagnostics           `json:"lanes"`
	Budgets                   SearchBudgetDiagnostics         `json:"budgets"`
	Throttles                 map[string]ThrottleDiagnostic   `json:"throttles"`
	Admitted                  AdmissionDiagnostic             `json:"admitted"`
	Rejections                RejectionDiagnostic             `json:"rejections"`
	Cache                     SearchCacheDiagnostic           `json:"cache"`
	Due                       SearchDueDiagnostic             `json:"due"`
	Rebuild                   SearchRebuildDiagnostic         `json:"rebuild"`
	LastReconciliationAt      *time.Time                      `json:"lastReconciliationAt"`
	FreshAt                   *time.Time                      `json:"freshAt"`
}

type SearchDiagnosticsReader struct {
	store *store.Store
	cfg   SearchDiagnosticsConfig
}

func NewSearchDiagnosticsReader(database *store.Store, cfg SearchDiagnosticsConfig) *SearchDiagnosticsReader {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 48 * time.Hour
	}
	return &SearchDiagnosticsReader{store: database, cfg: cfg}
}

func (r *SearchDiagnosticsReader) Diagnostics(ctx context.Context) (SearchDiagnostics, error) {
	if r == nil {
		return SearchDiagnostics{}, errors.New("search diagnostics reader is required")
	}
	result := emptySearchDiagnostics(r.cfg)
	if !r.cfg.Enabled {
		result.Status = SearchDiagnosticsDisabled
		return result, nil
	}
	if r.store == nil {
		return result, errors.New("search diagnostics store is required")
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	now := r.cfg.Now().UTC()

	if err = tx.QueryRow(ctx, `SELECT max(occurrence_at) FROM catalog_search_runs WHERE schedule_name='daily'`).Scan(&result.LastScheduledOccurrenceAt); err != nil {
		return result, err
	}
	if result.LastCompleteRun, err = readSearchRunDiagnostic(ctx, tx, "complete"); err != nil {
		return result, err
	}
	if result.LastPartialRun, err = readSearchRunDiagnostic(ctx, tx, "partial"); err != nil {
		return result, err
	}
	if result.LastFailedRun, err = readSearchRunDiagnostic(ctx, tx, "failed"); err != nil {
		return result, err
	}
	var activeRuns int64
	var latestRunStatus *string
	if err = tx.QueryRow(ctx, `SELECT count(*),(SELECT status FROM catalog_search_runs WHERE kind IN ('initial','scheduled','manual') ORDER BY run_revision DESC LIMIT 1)
		FROM catalog_search_runs WHERE kind IN ('initial','scheduled','manual') AND status IN ('queued','running')`).Scan(&activeRuns, &latestRunStatus); err != nil {
		return result, err
	}
	result.Status = publicSearchStatus(result.LastCompleteRun, latestRunStatus, activeRuns, now, r.cfg.StaleAfter)

	var latestRunID *string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM catalog_search_runs WHERE kind IN ('initial','scheduled','manual') ORDER BY run_revision DESC LIMIT 1`).Scan(&latestRunID); errors.Is(err, pgx.ErrNoRows) {
		latestRunID = nil
		err = nil
	} else if err != nil {
		return result, err
	}
	if latestRunID != nil {
		rows, queryErr := tx.Query(ctx, `SELECT f.feed,f.status,f.pages,f.observed_count,
			(SELECT count(DISTINCT o.steam_app_id) FROM catalog_search_observations o WHERE o.run_id=f.run_id AND o.feed=f.feed),
			(SELECT count(*) FROM catalog_search_memberships m WHERE m.feed=f.feed AND m.active),
			f.boundary_stop,f.safety_cap_reason,f.safe_error FROM catalog_search_feeds f WHERE f.run_id=$1`, *latestRunID)
		if queryErr != nil {
			return result, queryErr
		}
		for rows.Next() {
			var feed string
			var item SearchFeedDiagnostic
			if err = rows.Scan(&feed, &item.Status, &item.Pages, &item.Discovered, &item.Deduplicated, &item.Active, &item.BoundaryStop, &item.SafetyCap, &item.SafeReason); err != nil {
				rows.Close()
				return result, err
			}
			if _, ok := result.Feeds[feed]; ok {
				result.Feeds[feed] = item
			}
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return result, err
		}
		rows.Close()
	}

	if err = tx.QueryRow(ctx, `SELECT
		count(*) FILTER(WHERE lane='interactive' AND status='queued'),count(*) FILTER(WHERE lane='interactive' AND status='running'),
		count(*) FILTER(WHERE lane='interactive' AND status IN ('waiting','retryable')),count(*) FILTER(WHERE lane='interactive' AND status='dead'),
		count(*) FILTER(WHERE lane='catalog' AND status='queued'),count(*) FILTER(WHERE lane='catalog' AND status='running'),
		count(*) FILTER(WHERE lane='catalog' AND status IN ('waiting','retryable')),count(*) FILTER(WHERE lane='catalog' AND status='dead') FROM sync_jobs`).Scan(
		&result.Lanes.Interactive.Queued, &result.Lanes.Interactive.Running, &result.Lanes.Interactive.Deferred, &result.Lanes.Interactive.Failed,
		&result.Lanes.Catalog.Queued, &result.Lanes.Catalog.Running, &result.Lanes.Catalog.Deferred, &result.Lanes.Catalog.Failed); err != nil {
		return result, err
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(used) FILTER(WHERE kind='appdetails'),0),COALESCE(max(used) FILTER(WHERE kind='ai'),0)
		FROM catalog_budget_reservations WHERE hour_at=date_trunc('hour',$1::timestamptz)`, now).Scan(&result.Budgets.Store.Used, &result.Budgets.AI.Used); err != nil {
		return result, err
	}
	result.Budgets.Store.BudgetDiagnostic = finishBudget(result.Budgets.Store.Used, r.cfg.StoreBudgetPerHour)
	result.Budgets.AI = finishBudget(result.Budgets.AI.Used, r.cfg.AIBudgetPerHour)
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM catalog_search_worklist WHERE status='deferred'`).Scan(&result.Budgets.Store.Deferred); err != nil {
		return result, err
	}

	rows, err := tx.Query(ctx, `SELECT host,concurrency_limit,next_eligible_at FROM catalog_host_throttles`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var host string
		var item ThrottleDiagnostic
		if err = rows.Scan(&host, &item.Concurrency, &item.NextAt); err != nil {
			rows.Close()
			return result, err
		}
		if alias := throttleAlias(host); alias != "" {
			result.Throttles[alias] = item
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()

	if err = tx.QueryRow(ctx, `SELECT
		count(*) FILTER(WHERE active AND coming_soon=true),count(*) FILTER(WHERE active AND coming_soon=false),
		count(*) FILTER(WHERE tombstone_reason='mixed_or_below'),count(*) FILTER(WHERE active AND review_band='unrated' AND release_date<=($1::timestamptz)::date),
		count(*) FILTER(WHERE tombstone_reason='outside_window'),count(*) FILTER(WHERE tombstone_reason='unavailable'),
		count(*) FILTER(WHERE last_successful_appdetails_at IS NOT NULL),
		count(*) FILTER(WHERE next_eligibility_at IS NOT NULL AND next_eligibility_at<=$1::timestamptz),min(next_eligibility_at) FILTER(WHERE next_eligibility_at IS NOT NULL)
		FROM catalog_search_current`, now).Scan(&result.Admitted.Upcoming, &result.Admitted.Released,
		&result.Rejections.ReviewRejected, &result.Rejections.UnratedGrace, &result.Rejections.OutsideWindow,
		&result.Rejections.Unavailable, &result.Cache.MetadataEntries, &result.Due.Count, &result.Due.NextAt); err != nil {
		return result, err
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM catalog_search_worklist WHERE status='failed'`).Scan(&result.Rejections.Failed); err != nil {
		return result, err
	}
	if err = tx.QueryRow(ctx, `SELECT count(DISTINCT game_id) FROM game_features`).Scan(&result.Cache.AIEntries); err != nil {
		return result, err
	}
	if err = tx.QueryRow(ctx, `SELECT max(last_successful_at) FROM catalog_search_runs WHERE schedule_name='reconciliation' AND status='complete'`).Scan(&result.LastReconciliationAt); err != nil {
		return result, err
	}
	if err = tx.QueryRow(ctx, `SELECT max(last_successful_at) FROM catalog_search_runs WHERE kind IN ('initial','scheduled','manual') AND status='complete'`).Scan(&result.FreshAt); err != nil {
		return result, err
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(dirty_revision),0),count(*) FILTER(WHERE pending_job_id IS NOT NULL) FROM catalog_rebuild_state`).Scan(&result.Rebuild.DirtyRevision, &result.Rebuild.PendingWakeups); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return result, nil
}

func emptySearchDiagnostics(cfg SearchDiagnosticsConfig) SearchDiagnostics {
	feeds := make(map[string]SearchFeedDiagnostic, len(SearchDiagnosticFeeds))
	for _, feed := range SearchDiagnosticFeeds {
		feeds[feed] = SearchFeedDiagnostic{Status: "queued"}
	}
	return SearchDiagnostics{Status: SearchDiagnosticsUnavailable, Feeds: feeds, Throttles: map[string]ThrottleDiagnostic{
		"store": {}, "steam_api": {}, "openai": {},
	}, Budgets: SearchBudgetDiagnostics{Store: StoreBudgetDiagnostic{BudgetDiagnostic: finishBudget(0, cfg.StoreBudgetPerHour)}, AI: finishBudget(0, cfg.AIBudgetPerHour)}}
}

// UnavailableSearchDiagnostics is the complete public fallback used when the
// aggregate query fails. Keeping every fixed bucket present makes partial
// upstream/database failure safe for older and newer clients alike.
func UnavailableSearchDiagnostics() SearchDiagnostics {
	return emptySearchDiagnostics(SearchDiagnosticsConfig{})
}

func readSearchRunDiagnostic(ctx context.Context, tx pgx.Tx, status string) (*SearchRunDiagnostic, error) {
	var item SearchRunDiagnostic
	err := tx.QueryRow(ctx, `SELECT kind,status,occurrence_at,completed_at,safe_error FROM catalog_search_runs
		WHERE status=$1 AND kind IN ('initial','scheduled','manual') ORDER BY run_revision DESC LIMIT 1`, status).
		Scan(&item.Kind, &item.Status, &item.OccurredAt, &item.CompletedAt, &item.SafeReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func publicSearchStatus(last *SearchRunDiagnostic, latestStatus *string, active int64, now time.Time, staleAfter time.Duration) SearchDiagnosticsStatus {
	if last == nil {
		if active > 0 {
			return SearchDiagnosticsBackfilling
		}
		return SearchDiagnosticsUnavailable
	}
	if latestStatus != nil && (*latestStatus == "failed" || *latestStatus == "partial") {
		return SearchDiagnosticsStale
	}
	if last.CompletedAt == nil || now.Sub(last.CompletedAt.UTC()) > staleAfter {
		return SearchDiagnosticsStale
	}
	return SearchDiagnosticsReady
}

func finishBudget(used int64, limit int) BudgetDiagnostic {
	if limit < 0 {
		limit = 0
	}
	remaining := int64(limit) - used
	if remaining < 0 {
		remaining = 0
	}
	return BudgetDiagnostic{Used: used, Remaining: remaining, Limit: int64(limit)}
}

func throttleAlias(host string) string {
	switch host {
	case "store.steampowered.com":
		return "store"
	case "api.steampowered.com":
		return "steam_api"
	case "api.openai.com":
		return "openai"
	default:
		return ""
	}
}
