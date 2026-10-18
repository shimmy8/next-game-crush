package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

type SearchRepository struct{ store *store.Store }

func NewSearchRepository(db *store.Store) *SearchRepository { return &SearchRepository{store: db} }

func catalogUUID(value uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: value, Valid: value != uuid.Nil}
}

func (r *SearchRepository) PrepareRun(ctx context.Context, run RunRef) error {
	if r == nil || r.store == nil || run.ID == uuid.Nil || run.Revision < 1 || run.Generation < 0 {
		return errors.New("invalid search run")
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	rows, err := q.MarkSearchRunRunning(ctx, dbgen.MarkSearchRunRunningParams{
		StartTime: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}, RunID: catalogUUID(run.ID),
		RunRevision: run.Revision, RetryGeneration: int32(run.Generation),
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("search run is stale or terminal")
	}
	if _, err := q.PrepareSearchRunFeeds(ctx, catalogUUID(run.ID)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *SearchRepository) PublishFeed(ctx context.Context, run RunRef, feed steam.SearchFeed, completion FeedCompletion) (bool, error) {
	if r == nil || r.store == nil || run.ID == uuid.Nil || run.Revision < 1 || !validSearchFeed(feed) {
		return false, errors.New("invalid feed publication")
	}
	if completion.Status != "complete" || completion.SafetyCapReason != "" {
		return false, nil
	}
	if isPopularFeed(feed) {
		if !completion.NaturalEnd || completion.BoundaryStop {
			return false, nil
		}
	} else if !completion.NaturalEnd && !(completion.BoundaryStop && completion.Monotonic) {
		return false, nil
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	if _, err := q.LockSearchFeedPublication(ctx, string(feed)); err != nil {
		return false, err
	}
	published, err := q.PublishFeedMembership(ctx, dbgen.PublishFeedMembershipParams{
		PublishedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}, RunID: catalogUUID(run.ID), Feed: string(feed),
	})
	if err != nil {
		return false, err
	}
	return published, tx.Commit(ctx)
}

// finalizePageFailure owns the final-attempt crash boundary: the queue row,
// attempt audit, feed status and next-feed scheduling commit together. Earlier
// attempts remain under the ordinary queue retry lifecycle.
func (r *SearchRepository) finalizePageFailure(ctx context.Context, lease jobs.Lease, payload SearchPagePayload, safeReason string, at time.Time) error {
	if r == nil || r.store == nil || lease.ID == uuid.Nil || lease.FencingToken == uuid.Nil || lease.Attempts < lease.MaxAttempts || lease.MaxAttempts < 1 {
		return errors.New("invalid terminal search page failure")
	}
	switch safeReason {
	case "unavailable", "request_failed", "invalid_response", "rate_limited", "retry_exhausted", "malformed":
	default:
		safeReason = "request_failed"
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	rows, err := q.FailJob(ctx, dbgen.FailJobParams{
		Retryable: false, RetryAt: pgtype.Timestamptz{Time: at.UTC(), Valid: true}, Cause: pgtype.Text{String: safeReason, Valid: true},
		ID: catalogUUID(lease.ID), WorkerID: pgtype.Text{String: lease.WorkerID, Valid: true}, FencingToken: catalogUUID(lease.FencingToken),
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return jobs.ErrLeaseLost
	}
	if err := q.FinishFailedAttempt(ctx, dbgen.FinishFailedAttemptParams{JobID: catalogUUID(lease.ID), WorkerID: lease.WorkerID, FencingToken: catalogUUID(lease.FencingToken), Status: "dead", Error: pgtype.Text{String: safeReason, Valid: true}}); err != nil {
		return err
	}
	failed, err := q.FailSearchFeedAtCursor(ctx, dbgen.FailSearchFeedAtCursorParams{
		CompletedAt: pgtype.Timestamptz{Time: at.UTC(), Valid: true}, SafeError: safeReason, RunID: catalogUUID(payload.RunID),
		Feed: string(payload.Feed), ExpectedOffset: int32(payload.Start), PageGeneration: int32(payload.PageGeneration),
	})
	if err != nil {
		return err
	}
	if failed == 1 {
		for index, feed := range searchFeedOrder {
			if feed != payload.Feed || index+1 >= len(searchFeedOrder) {
				continue
			}
			nextFeed := searchFeedOrder[index+1]
			var status string
			if err := tx.QueryRow(ctx, `SELECT status FROM catalog_search_feeds WHERE run_id=$1 AND feed=$2`, payload.RunID, nextFeed).Scan(&status); err != nil {
				return err
			}
			if status == "queued" {
				if _, err := jobs.EnqueueInTx(ctx, q, searchPageSpec(SearchPagePayload{Version: 1, RunID: payload.RunID, Feed: nextFeed})); err != nil {
					return err
				}
			}
		}
	}
	if _, err := q.FinishSearchRunIfTerminal(ctx, dbgen.FinishSearchRunIfTerminalParams{CompletedAt: pgtype.Timestamptz{Time: at.UTC(), Valid: true}, RunID: catalogUUID(payload.RunID)}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func searchRunKey(schedule string, at time.Time, generation int) string {
	return fmt.Sprintf("catalog.search.run:v1:%s:%d:retry:%d", schedule, at.Unix(), generation)
}

func searchRunRef(row dbgen.CatalogSearchRun) RunRef {
	return RunRef{ID: uuid.UUID(row.ID.Bytes), Revision: row.RunRevision, Generation: int(row.RetryGeneration), Kind: RunKind(row.Kind), OccurredAt: row.OccurrenceAt.Time.UTC()}
}

func enqueueSearchRun(ctx context.Context, q *dbgen.Queries, row dbgen.CatalogSearchRun) error {
	ref := searchRunRef(row)
	payload, err := json.Marshal(SearchRunPayload{Version: 1, RunID: ref.ID, RunRevision: ref.Revision, Generation: ref.Generation})
	if err != nil {
		return err
	}
	priority := 70
	if ref.Kind == RunReconciliation {
		priority = 10
	}
	_, err = jobs.EnqueueInTx(ctx, q, jobs.Spec{Lane: jobs.LaneCatalog, Kind: SearchRunJobKind, Stage: "catalog_search_run", Payload: payload, MaxAttempts: 4, Priority: priority, IdempotencyKey: searchRunKey(row.ScheduleName, ref.OccurredAt, ref.Generation)})
	return err
}

func (r *SearchRepository) createRun(ctx context.Context, tx pgx.Tx, cfg DiscoveryConfig, schedule string, kind RunKind, at time.Time) (RunRef, error) {
	snapshot, err := json.Marshal(cfg)
	if err != nil {
		return RunRef{}, err
	}
	q := r.store.Queries.WithTx(tx)
	row, err := q.CreateOrGetSearchRun(ctx, dbgen.CreateOrGetSearchRunParams{ScheduleName: schedule, OccurrenceAt: pgtype.Timestamptz{Time: at.UTC().Truncate(time.Second), Valid: true}, Kind: string(kind), ConfigSnapshot: snapshot})
	if err != nil {
		return RunRef{}, err
	}
	// Idempotent enqueue never rearms a terminal queue row. The watchdog owns
	// generation changes and is fenced by the occurrence's row lock.
	if row.Status == "queued" || row.Status == "running" {
		exists, err := q.SearchRunJobExists(ctx, searchRunKey(row.ScheduleName, row.OccurrenceAt.Time, int(row.RetryGeneration)))
		if err != nil {
			return RunRef{}, err
		}
		if !exists {
			if err := enqueueSearchRun(ctx, q, row); err != nil {
				return RunRef{}, err
			}
		}
	}
	return searchRunRef(row), nil
}

func occurrenceRetryDelay(generation int) time.Duration {
	if generation >= 5 {
		return 24 * time.Hour
	}
	return time.Hour * time.Duration(1<<generation)
}

func (r *SearchRepository) rearmTerminalRuns(ctx context.Context, tx pgx.Tx, now time.Time) error {
	q := r.store.Queries.WithTx(tx)
	rows, err := q.ListTerminalSearchRuns(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		due := row.NextRetryAt
		if !due.Valid {
			due = pgtype.Timestamptz{Time: row.JobUpdatedAt.Time.Add(occurrenceRetryDelay(int(row.RetryGeneration))), Valid: true}
		}
		if row.Status != "failed" || !row.NextRetryAt.Valid {
			if err := q.MarkSearchRunFailed(ctx, dbgen.MarkSearchRunFailedParams{ID: row.ID, NextRetryAt: due}); err != nil {
				return err
			}
		}
		if due.Time.After(now) {
			continue
		}
		run, err := q.AdvanceSearchRunGeneration(ctx, dbgen.AdvanceSearchRunGenerationParams{ID: row.ID, RetryGeneration: row.RetryGeneration, DueAt: pgtype.Timestamptz{Time: now, Valid: true}})
		if err != nil {
			return err
		}
		if err := enqueueSearchRun(ctx, q, run); err != nil {
			return err
		}
	}
	return nil
}
