package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

type SearchRepository struct{ store *store.Store }

func NewSearchRepository(db *store.Store) *SearchRepository { return &SearchRepository{store: db} }

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
