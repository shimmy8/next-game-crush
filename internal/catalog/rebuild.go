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
const catalogRebuildMaxRetryGenerations = 3
const catalogRecommendationRebuildJobKind = "recommendations.rebuild"

const (
	EligibilityChangeMembership        = "membership"
	EligibilityChangeSemanticContent   = "semantic_content"
	EligibilityChangeReviewEligibility = "review_eligibility"
)

// EligibilityChange is the persistence-neutral input to catalog rebuild
// coalescing. NextChangeAt is a clock deadline, not an invented release date.
type EligibilityChange struct {
	GameID       uuid.UUID
	Kind         string
	NextChangeAt *time.Time
}

func (c EligibilityChange) dirtiesRanking() bool {
	switch c.Kind {
	case EligibilityChangeMembership, EligibilityChangeSemanticContent, EligibilityChangeReviewEligibility:
		return true
	default:
		return false
	}
}

// rebuildWakePlan is deliberately independent of PostgreSQL and the queue.
// The repository integration can compute and persist this decision while
// holding its per-user row lock.
type rebuildWakePlan struct {
	UserID          uuid.UUID
	Revision        int64
	RetryGeneration int
	AvailableAt     time.Time
	IdempotencyKey  string
}

type rebuildCoordinatorState struct {
	UserID                uuid.UUID
	DirtyRevision         int64
	EnqueuedRevision      int64
	LastCompletedRevision int64
	RetryGeneration       int
	FirstDirtyAt          *time.Time
	LastScheduledAt       *time.Time
	Pending               *rebuildWakePlan
}

func (s *rebuildCoordinatorState) applyEligibilityChange(change EligibilityChange, at time.Time) bool {
	if change.GameID == uuid.Nil || !change.dirtiesRanking() {
		return false
	}
	at = at.UTC()
	if s.DirtyRevision <= s.EnqueuedRevision && s.Pending == nil {
		s.RetryGeneration = 0
		s.FirstDirtyAt = &at
	}
	s.DirtyRevision++
	if s.FirstDirtyAt == nil {
		s.FirstDirtyAt = &at
	}
	return true
}

func (s rebuildCoordinatorState) wakeDueAt() (time.Time, bool) {
	if s.UserID == uuid.Nil || s.Pending != nil || s.DirtyRevision <= s.LastCompletedRevision || s.FirstDirtyAt == nil || s.RetryGeneration > catalogRebuildMaxRetryGenerations {
		return time.Time{}, false
	}
	dueAt := s.FirstDirtyAt.UTC().Add(catalogRebuildDebounce)
	if s.LastScheduledAt != nil {
		minimum := s.LastScheduledAt.UTC().Add(catalogRebuildMinimumInterval)
		if minimum.After(dueAt) {
			dueAt = minimum
		}
	}
	return dueAt, true
}

func (s rebuildCoordinatorState) nextWake(now time.Time) (rebuildWakePlan, bool) {
	dueAt, ok := s.wakeDueAt()
	if !ok {
		return rebuildWakePlan{}, false
	}
	now = now.UTC()
	if dueAt.After(now) {
		return rebuildWakePlan{}, false
	}
	return rebuildWakePlan{
		UserID:          s.UserID,
		Revision:        s.DirtyRevision,
		RetryGeneration: s.RetryGeneration,
		AvailableAt:     now,
		IdempotencyKey:  fmt.Sprintf("catalog:recommendations.rebuild:v2:%s:%d:retry:%d", s.UserID, s.DirtyRevision, s.RetryGeneration),
	}, true
}

func (s *rebuildCoordinatorState) recordWake(wake rebuildWakePlan) bool {
	if s.Pending != nil || wake.UserID != s.UserID || wake.Revision != s.DirtyRevision || wake.RetryGeneration != s.RetryGeneration {
		return false
	}
	s.EnqueuedRevision = wake.Revision
	availableAt := wake.AvailableAt.UTC()
	s.LastScheduledAt = &availableAt
	wake.AvailableAt = availableAt
	s.Pending = &wake
	s.FirstDirtyAt = nil
	return true
}

func (s *rebuildCoordinatorState) finishWake(wake rebuildWakePlan, succeeded bool) bool {
	if s.Pending == nil || wake.UserID != s.Pending.UserID || wake.Revision != s.Pending.Revision || wake.RetryGeneration != s.Pending.RetryGeneration || wake.IdempotencyKey != s.Pending.IdempotencyKey {
		return false
	}
	s.Pending = nil
	if succeeded {
		if wake.Revision > s.LastCompletedRevision {
			s.LastCompletedRevision = wake.Revision
		}
		s.RetryGeneration = 0
	} else {
		if s.DirtyRevision > wake.Revision {
			// Retry generations belong to one immutable dirty revision. New
			// content schedules its own generation zero successor.
			s.RetryGeneration = 0
		} else {
			s.RetryGeneration++
		}
		if s.FirstDirtyAt == nil {
			failedAt := wake.AvailableAt.UTC()
			s.FirstDirtyAt = &failedAt
		}
	}
	return true
}

type eligibilityDueFence struct {
	GameID      uuid.UUID
	ExpectedAt  time.Time
	SuccessorAt *time.Time
}

// clockDrivenEligibilityChange converts a due deadline into the same semantic
// dirty signal used by review transitions. The returned fence lets persistence
// clear or advance the deadline only after the corresponding rebuild succeeds.
func clockDrivenEligibilityChange(gameID uuid.UUID, now, dueAt time.Time, successorAt *time.Time) (EligibilityChange, eligibilityDueFence, bool) {
	now, dueAt = now.UTC(), dueAt.UTC()
	if gameID == uuid.Nil || dueAt.After(now) {
		return EligibilityChange{}, eligibilityDueFence{}, false
	}
	var successor *time.Time
	if successorAt != nil {
		next := successorAt.UTC()
		successor = &next
	}
	return EligibilityChange{GameID: gameID, Kind: EligibilityChangeReviewEligibility, NextChangeAt: successor}, eligibilityDueFence{GameID: gameID, ExpectedAt: dueAt, SuccessorAt: successor}, true
}

func (f eligibilityDueFence) resolve(current *time.Time, succeeded bool) (*time.Time, bool) {
	if current == nil {
		return nil, false
	}
	currentUTC := current.UTC()
	if !succeeded || f.GameID == uuid.Nil || !currentUTC.Equal(f.ExpectedAt) {
		return &currentUTC, false
	}
	if f.SuccessorAt == nil {
		return nil, true
	}
	next := f.SuccessorAt.UTC()
	return &next, true
}

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
		jobID, err := jobs.EnqueueInTx(ctx, q, jobs.Spec{Lane: jobs.LaneInteractive, Kind: catalogRecommendationRebuildJobKind, Stage: "recommendations", IdempotencyKey: key, Payload: payload, AvailableAt: availableAt, MaxAttempts: 3, Priority: 50, UserID: row.userID})
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
		Lane: jobs.LaneInteractive,
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
