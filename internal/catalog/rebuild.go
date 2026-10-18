package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/jobs"
	dbgen "next-game-crush/internal/store/sqlc"
)

const catalogRebuildDebounce = 10 * time.Minute
const catalogRebuildMinimumInterval = time.Hour
const catalogRebuildWakePoll = 5 * time.Second

type rebuildWakePayload struct {
	Version  int       `json:"version"`
	UserID   uuid.UUID `json:"user_id"`
	Revision int64     `json:"revision"`
}

type dirtyRow struct {
	userID                          uuid.UUID
	dirtyRevision, enqueuedRevision int64
	dirtySince, lastScheduledAt     *time.Time
	pendingJobID                    *uuid.UUID
	pendingStatus                   string
	pendingAttempts                 int
}

// ScheduleRebuilds converts global catalog dirtiness into bounded per-user
// recommendation jobs. The row and user key locks fence concurrent schedulers
// and user deletion in the same transaction.
func (s *DiscoveryService) ScheduleRebuilds(ctx context.Context) error {
	if !s.cfg.Enabled {
		return nil
	}
	now := s.now().UTC()
	tx, err := s.repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT state.user_id,state.dirty_revision,state.enqueued_revision,state.dirty_since,
		state.last_scheduled_at,state.pending_job_id,COALESCE(job.status,''),COALESCE(job.attempts,0)
		FROM catalog_rebuild_state state JOIN users u ON u.id=state.user_id
		LEFT JOIN sync_jobs job ON job.id=state.pending_job_id
		WHERE state.dirty_revision>state.enqueued_revision
		ORDER BY state.user_id FOR UPDATE OF state,u`)
	if err != nil {
		return err
	}
	var dirty []dirtyRow
	for rows.Next() {
		var row dirtyRow
		if err := rows.Scan(&row.userID, &row.dirtyRevision, &row.enqueuedRevision, &row.dirtySince, &row.lastScheduledAt, &row.pendingJobID, &row.pendingStatus, &row.pendingAttempts); err != nil {
			rows.Close()
			return err
		}
		dirty = append(dirty, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	q := s.repo.store.Queries.WithTx(tx)
	for _, row := range dirty {
		switch row.pendingStatus {
		case "queued", "retryable", "waiting":
			if row.pendingAttempts > 0 {
				if err := enqueueRebuildWakeup(ctx, q, row, now); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE catalog_rebuild_state SET enqueued_revision=dirty_revision,dirty_since=NULL WHERE user_id=$1`, row.userID); err != nil {
				return err
			}
			continue
		case "running":
			if err := enqueueRebuildWakeup(ctx, q, row, now); err != nil {
				return err
			}
			continue
		}
		if row.dirtySince == nil {
			continue
		}
		availableAt := row.dirtySince.UTC().Add(catalogRebuildDebounce)
		if row.lastScheduledAt != nil {
			minimum := row.lastScheduledAt.UTC().Add(catalogRebuildMinimumInterval)
			if minimum.After(availableAt) {
				availableAt = minimum
			}
		}
		if availableAt.Before(now) {
			availableAt = now
		}
		payload, _ := json.Marshal(struct {
			Version int       `json:"version"`
			UserID  uuid.UUID `json:"user_id"`
		}{Version: 1, UserID: row.userID})
		key := fmt.Sprintf("catalog:recommendations.rebuild:v1:%s:%d", row.userID, row.dirtyRevision)
		jobID, err := jobs.EnqueueInTx(ctx, q, jobs.Spec{Kind: "recommendations.rebuild", Stage: "recommendations", IdempotencyKey: key, Payload: payload, AvailableAt: availableAt, MaxAttempts: 3, Priority: 50, UserID: row.userID})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE catalog_rebuild_state SET enqueued_revision=dirty_revision,dirty_since=NULL,last_enqueued_at=$2,last_scheduled_at=$3,pending_job_id=$4 WHERE user_id=$1`, row.userID, now, availableAt, jobID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func enqueueRebuildWakeup(ctx context.Context, q *dbgen.Queries, row dirtyRow, now time.Time) error {
	payload, _ := json.Marshal(rebuildWakePayload{Version: 1, UserID: row.userID, Revision: row.dirtyRevision})
	_, err := jobs.EnqueueInTx(ctx, q, jobs.Spec{
		Kind: RebuildWakeJobKind, Stage: "recommendations", UserID: row.userID, Priority: 50, MaxAttempts: 3,
		IdempotencyKey: fmt.Sprintf("catalog:rebuild-wakeup:v1:%s:%d", row.userID, row.dirtyRevision),
		Payload:        payload, AvailableAt: now.Add(catalogRebuildWakePoll),
	})
	return err
}

func (s *DiscoveryService) handleRebuildWakeup(ctx context.Context, lease jobs.Lease) error {
	var payload rebuildWakePayload
	if strictDiscoveryPayload(lease.Payload, &payload) != nil || payload.Version != 1 || payload.UserID == uuid.Nil || payload.Revision < 1 || lease.UserID != payload.UserID {
		return errors.New("unsupported catalog rebuild wakeup payload schema")
	}
	var status string
	var attempts int
	err := s.repo.store.Pool.QueryRow(ctx, `SELECT COALESCE(job.status,''),COALESCE(job.attempts,0)
		FROM catalog_rebuild_state state LEFT JOIN sync_jobs job ON job.id=state.pending_job_id
		WHERE state.user_id=$1 AND state.dirty_revision>state.enqueued_revision`, payload.UserID).Scan(&status, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == "running" || ((status == "retryable" || status == "waiting") && attempts > 0) {
		if err := s.queue.DeferUntil(ctx, lease, time.Now().UTC().Add(catalogRebuildWakePoll)); err != nil {
			return err
		}
		return jobs.ErrDeferred
	}
	return s.ScheduleRebuilds(ctx)
}

// MarkCatalogChanged is the narrow AI completion callback. Only active shared
// or wishlist candidates can dirty personal rankings.
func (s *DiscoveryService) MarkCatalogChanged(ctx context.Context, gameID uuid.UUID) error {
	if gameID == uuid.Nil || !s.cfg.Enabled {
		return nil
	}
	tx, err := s.repo.store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var candidate bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_entry_sources WHERE game_id=$1 AND active)
		OR EXISTS(SELECT 1 FROM wishlist_entries WHERE game_id=$1)`, gameID).Scan(&candidate); err != nil {
		return err
	}
	if candidate {
		if err := s.repo.MarkCandidatesChanged(ctx, tx, s.now().UTC()); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if candidate {
		return s.ScheduleRebuilds(ctx)
	}
	return nil
}
