package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

const catalogRebuildDebounce = 10 * time.Minute
const catalogRebuildMinimumInterval = time.Hour
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

type dirtyRow struct {
	userID                                                 uuid.UUID
	dirtyRevision, enqueuedRevision, lastCompletedRevision int64
	retryGeneration                                        int
	firstDirtyAt, lastScheduledAt, dueAt                   *time.Time
	pendingJobID                                           *uuid.UUID
	pendingStatus, pendingKey                              string
}

// MarkCandidateSetDirty increments only for changes that can alter ranking.
// The deadline is retained separately so a disabled discovery source cannot
// disable clock-driven removal from the recommendation set.
func (r *DiscoveryRepository) MarkCandidateSetDirty(ctx context.Context, tx pgx.Tx, change EligibilityChange) error {
	return r.MarkCandidateSetDirtyAt(ctx, tx, change, time.Now().UTC())
}

func (r *DiscoveryRepository) MarkCandidateSetDirtyAt(ctx context.Context, tx pgx.Tx, change EligibilityChange, now time.Time) error {
	if r == nil || r.store == nil || tx == nil || change.GameID == uuid.Nil || !change.dirtiesRanking() {
		return nil
	}
	now = now.UTC()
	_, err := tx.Exec(ctx, `WITH live_users AS MATERIALIZED (
		SELECT u.id AS user_id FROM users u
		WHERE EXISTS(SELECT 1 FROM preference_profiles p WHERE p.user_id=u.id)
		  AND NOT EXISTS(SELECT 1 FROM deleted_user_tombstones d WHERE d.user_id=u.id)
		ORDER BY u.id FOR KEY SHARE OF u
	)
	INSERT INTO catalog_rebuild_state(user_id,dirty_revision,dirty_since,first_dirty_at,retry_generation)
	SELECT user_id,1,$1,$1,0 FROM live_users
	ON CONFLICT(user_id) DO UPDATE SET
		dirty_revision=catalog_rebuild_state.dirty_revision+1,
		dirty_since=COALESCE(catalog_rebuild_state.dirty_since,EXCLUDED.dirty_since),
		first_dirty_at=COALESCE(catalog_rebuild_state.first_dirty_at,EXCLUDED.first_dirty_at),
		retry_generation=0`, now)
	return err
}

func (r *SearchRepository) MarkCandidateSetDirty(ctx context.Context, tx pgx.Tx, change EligibilityChange) error {
	if r == nil {
		return nil
	}
	return NewDiscoveryRepository(r.store).MarkCandidateSetDirty(ctx, tx, change)
}

// ScheduleDueRebuilds converts dirty revisions and due eligibility boundaries
// into immutable detached recommendation roots. It intentionally ignores the
// search discovery feature flag.
func (s *DiscoveryService) ScheduleDueRebuilds(ctx context.Context, now time.Time) error {
	if s == nil || s.repo == nil {
		return errors.New("catalog rebuild scheduler is required")
	}
	return scheduleDueRebuilds(ctx, s.repo.store, s.cfg, now.UTC())
}

func (r *SearchRepository) ScheduleDueRebuilds(ctx context.Context, cfg DiscoveryConfig, now time.Time) error {
	if r == nil || r.store == nil {
		return errors.New("catalog rebuild repository is required")
	}
	return scheduleDueRebuilds(ctx, r.store, cfg, now.UTC())
}

// ScheduleRebuilds remains the legacy call seam while producers migrate. It
// now delegates to the durable scheduler and no longer depends on Enabled.
func (s *DiscoveryService) ScheduleRebuilds(ctx context.Context) error {
	return s.ScheduleDueRebuilds(ctx, s.now().UTC())
}

func scheduleDueRebuilds(ctx context.Context, database *store.Store, cfg DiscoveryConfig, now time.Time) error {
	if database == nil || database.Pool == nil || now.IsZero() {
		return errors.New("catalog rebuild persistence and clock are required")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	now = now.UTC()
	tx, err := database.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Lock and project due candidates before any rebuild-state row. Search
	// qualification takes the same AppID/current -> rebuild-state order.
	due, err := lockDueEligibility(ctx, tx, database, cfg, now)
	if err != nil {
		return err
	}
	clockTransition := false
	for _, item := range due {
		changed, err := projectLockedEligibility(ctx, tx, database, item, now)
		if err != nil {
			return err
		}
		clockTransition = clockTransition || changed
	}
	if clockTransition {
		if _, err := tx.Exec(ctx, `WITH live_users AS MATERIALIZED (
		SELECT u.id FROM users u
		WHERE EXISTS(SELECT 1 FROM preference_profiles p WHERE p.user_id=u.id)
		  AND NOT EXISTS(SELECT 1 FROM deleted_user_tombstones d WHERE d.user_id=u.id)
		ORDER BY u.id FOR KEY SHARE OF u
	)
	INSERT INTO catalog_rebuild_state(user_id,dirty_revision,dirty_since,first_dirty_at,retry_generation)
	SELECT id,1,$1,$1,0 FROM live_users
	ON CONFLICT(user_id) DO UPDATE SET dirty_revision=catalog_rebuild_state.dirty_revision+1,
		dirty_since=COALESCE(catalog_rebuild_state.dirty_since,EXCLUDED.dirty_since),
		first_dirty_at=COALESCE(catalog_rebuild_state.first_dirty_at,EXCLUDED.first_dirty_at),retry_generation=0
	WHERE catalog_rebuild_state.due_at IS NULL OR catalog_rebuild_state.due_at<$1`, now); err != nil {
			return err
		}
	}
	// Migration-17 rows and legacy writers may carry dirty_since without the
	// v2 first_dirty_at marker. Upgrade them in place instead of stranding work.
	if _, err := tx.Exec(ctx, `UPDATE catalog_rebuild_state SET first_dirty_at=dirty_since
		WHERE first_dirty_at IS NULL AND dirty_since IS NOT NULL AND dirty_revision>last_completed_revision`); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT state.user_id,state.dirty_revision,state.enqueued_revision,state.last_completed_revision,
		state.retry_generation,state.first_dirty_at,state.last_scheduled_at,state.due_at,state.pending_job_id,
		COALESCE(job.status,''),COALESCE(job.idempotency_key,'')
		FROM catalog_rebuild_state state JOIN users u ON u.id=state.user_id
		LEFT JOIN sync_jobs job ON job.id=state.pending_job_id
		ORDER BY state.user_id FOR UPDATE OF state,u`)
	if err != nil {
		return err
	}
	var dirty []dirtyRow
	for rows.Next() {
		var row dirtyRow
		if err := rows.Scan(&row.userID, &row.dirtyRevision, &row.enqueuedRevision, &row.lastCompletedRevision,
			&row.retryGeneration, &row.firstDirtyAt, &row.lastScheduledAt, &row.dueAt, &row.pendingJobID, &row.pendingStatus, &row.pendingKey); err != nil {
			rows.Close()
			return err
		}
		dirty = append(dirty, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	q := database.Queries.WithTx(tx)
	for _, row := range dirty {
		if row.pendingJobID != nil && !isCatalogRebuildV2Key(row.pendingKey, row.userID) {
			switch row.pendingStatus {
			case "queued", "retryable", "waiting", "running":
				// The pre-upgrade handler owns this attempt. Let it finish, then
				// replace its pointer on a later scheduler pass.
				continue
			case "succeeded", "dead":
				// A v1 terminal result cannot satisfy the v2 publication fence.
				// Detach it without advancing last_completed_revision; the same
				// current dirty revision is rebuilt under a v2 immutable root.
				var first time.Time
				if err := tx.QueryRow(ctx, `UPDATE catalog_rebuild_state SET pending_job_id=NULL,retry_generation=0,
					first_dirty_at=COALESCE(first_dirty_at,dirty_since,$3),dirty_since=COALESCE(dirty_since,first_dirty_at,$3)
					WHERE user_id=$1 AND pending_job_id=$2 RETURNING first_dirty_at`, row.userID, row.pendingJobID, now).Scan(&first); err != nil {
					return err
				}
				row.pendingJobID = nil
				row.retryGeneration = 0
				row.firstDirtyAt = &first
				row.pendingStatus, row.pendingKey = "", ""
			}
		}
		switch row.pendingStatus {
		case "queued", "retryable", "waiting", "running":
			continue
		case "dead":
			if row.dirtyRevision > row.enqueuedRevision {
				row.retryGeneration = 0
			} else if row.retryGeneration >= catalogRebuildMaxRetryGenerations {
				continue
			} else {
				row.retryGeneration++
			}
			if _, err := tx.Exec(ctx, `UPDATE catalog_rebuild_state SET pending_job_id=NULL,retry_generation=$2,
				first_dirty_at=COALESCE(first_dirty_at,$3),dirty_since=COALESCE(dirty_since,$3)
				WHERE user_id=$1 AND pending_job_id=$4`, row.userID, row.retryGeneration, now, row.pendingJobID); err != nil {
				return err
			}
			row.pendingJobID = nil
		case "succeeded":
			// The handler completion callback normally clears this fence before
			// queue completion. Leave a succeeded orphan untouched: assuming its
			// side effects would incorrectly advance eligibility deadlines.
			continue
		}
		if row.pendingJobID != nil || row.dirtyRevision <= row.lastCompletedRevision || row.firstDirtyAt == nil || row.retryGeneration > catalogRebuildMaxRetryGenerations {
			continue
		}
		availableAt := row.firstDirtyAt.UTC().Add(catalogRebuildDebounce)
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
		key := fmt.Sprintf("catalog:recommendations.rebuild:v2:%s:%d:retry:%d", row.userID, row.dirtyRevision, row.retryGeneration)
		jobID, err := jobs.EnqueueInTx(ctx, q, jobs.Spec{Lane: jobs.LaneInteractive, Kind: catalogRecommendationRebuildJobKind, Stage: "recommendations", IdempotencyKey: key, Payload: payload, AvailableAt: availableAt, MaxAttempts: 1, Priority: 50, UserID: row.userID})
		if err != nil {
			return err
		}
		var cutoff any = row.dueAt
		if len(due) > 0 {
			cutoff = now
		}
		if _, err := tx.Exec(ctx, `UPDATE catalog_rebuild_state SET enqueued_revision=dirty_revision,dirty_since=NULL,
			first_dirty_at=NULL,last_enqueued_at=$2,last_scheduled_at=$3,pending_job_id=$4,due_at=$5
			WHERE user_id=$1`, row.userID, now, availableAt, jobID, cutoff); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func isCatalogRebuildV2Key(key string, userID uuid.UUID) bool {
	const prefix = "catalog:recommendations.rebuild:v2:"
	if userID == uuid.Nil || !strings.HasPrefix(key, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(key, prefix), ":")
	if len(parts) != 4 || parts[0] != userID.String() || parts[2] != "retry" {
		return false
	}
	revision, revisionErr := strconv.ParseInt(parts[1], 10, 64)
	retry, retryErr := strconv.Atoi(parts[3])
	return revisionErr == nil && revision > 0 && retryErr == nil && retry >= 0
}

// CatalogRebuildFinished is called by the recommendation handler only after a
// ranking is published (including an already-published replay). Failures keep
// the prior published ranking and advance a bounded immutable retry generation.
func (s *DiscoveryService) CatalogRebuildFinished(ctx context.Context, userID uuid.UUID, idempotencyKey string, succeeded bool) error {
	if s == nil || s.repo == nil || userID == uuid.Nil || !strings.HasPrefix(idempotencyKey, "catalog:recommendations.rebuild:v2:") {
		return nil
	}
	now := s.now().UTC()
	tx, err := s.repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var expectedPendingID uuid.UUID
	var expectedKey string
	var expectedCutoff *time.Time
	if succeeded {
		err = tx.QueryRow(ctx, `SELECT job.id,job.idempotency_key,state.due_at
			FROM catalog_rebuild_state state JOIN sync_jobs job ON job.id=state.pending_job_id
			WHERE state.user_id=$1`, userID).Scan(&expectedPendingID, &expectedKey, &expectedCutoff)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if expectedKey != idempotencyKey {
			return nil
		}
	}
	var due []lockedEligibility
	if succeeded && expectedCutoff != nil {
		due, err = lockDueEligibility(ctx, tx, s.repo.store, s.cfg, expectedCutoff.UTC())
		if err != nil {
			return err
		}
	}
	var dirtyRevision, enqueuedRevision, lastCompletedRevision int64
	var retryGeneration int
	var pendingJobID uuid.UUID
	var pendingKey string
	var cutoff *time.Time
	err = tx.QueryRow(ctx, `SELECT state.dirty_revision,state.enqueued_revision,state.last_completed_revision,
		state.retry_generation,job.id,job.idempotency_key,state.due_at
		FROM catalog_rebuild_state state JOIN sync_jobs job ON job.id=state.pending_job_id
		WHERE state.user_id=$1 FOR UPDATE OF state`, userID).
		Scan(&dirtyRevision, &enqueuedRevision, &lastCompletedRevision, &retryGeneration, &pendingJobID, &pendingKey, &cutoff)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if pendingKey != idempotencyKey || (succeeded && (pendingJobID != expectedPendingID || !sameOptionalTime(cutoff, expectedCutoff))) {
		return nil
	}
	if succeeded {
		for _, item := range due {
			if err := resolveLockedEligibility(ctx, tx, s.repo.store, item); err != nil {
				return err
			}
		}
		firstDirty := any(nil)
		dirtySince := any(nil)
		if dirtyRevision > enqueuedRevision {
			firstDirty, dirtySince = now, now
		}
		if _, err := tx.Exec(ctx, `UPDATE catalog_rebuild_state SET
			last_completed_revision=GREATEST(last_completed_revision,$2),pending_job_id=NULL,retry_generation=0,
			first_dirty_at=COALESCE(first_dirty_at,$3),dirty_since=COALESCE(dirty_since,$4),
			due_at=NULL
			WHERE user_id=$1 AND pending_job_id=$5`, userID, enqueuedRevision, firstDirty, dirtySince, pendingJobID); err != nil {
			return err
		}
	} else {
		nextGeneration := 0
		if dirtyRevision <= enqueuedRevision {
			nextGeneration = retryGeneration + 1
		}
		if nextGeneration > catalogRebuildMaxRetryGenerations {
			nextGeneration = catalogRebuildMaxRetryGenerations + 1
		}
		if _, err := tx.Exec(ctx, `UPDATE catalog_rebuild_state SET pending_job_id=NULL,retry_generation=$2,
			first_dirty_at=COALESCE(first_dirty_at,$3),dirty_since=COALESCE(dirty_since,$3)
			WHERE user_id=$1 AND pending_job_id=$4`, userID, nextGeneration, now, pendingJobID); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return s.ScheduleDueRebuilds(ctx, now)
}

type lockedEligibility struct {
	current  dbgen.CatalogSearchCurrent
	decision EligibilityDecision
}

func lockDueEligibility(ctx context.Context, tx pgx.Tx, database *store.Store, cfg DiscoveryConfig, cutoff time.Time) ([]lockedEligibility, error) {
	q := database.Queries.WithTx(tx)
	due, err := q.ListDueEligibilityChanges(ctx, dbgen.ListDueEligibilityChangesParams{
		DueAt: pgtype.Timestamptz{Time: cutoff, Valid: true}, ResultLimit: 100000,
	})
	if err != nil {
		return nil, err
	}
	result := make([]lockedEligibility, 0, len(due))
	for _, candidate := range due {
		if err := q.LockMetadataApp(ctx, candidate.SteamAppID); err != nil {
			return nil, err
		}
		current, err := q.LockCurrentMetadataCandidate(ctx, candidate.SteamAppID)
		if err != nil {
			return nil, err
		}
		if current.RunRevision != candidate.RunRevision || current.ObservationOrdinal != candidate.ObservationOrdinal ||
			!samePGTime(current.NextEligibilityAt, candidate.NextEligibilityAt) ||
			!current.NextEligibilityAt.Valid || current.NextEligibilityAt.Time.After(cutoff) {
			continue
		}
		popularUpcoming, popularNew, wishlisted, err := searchEligibilityProvenance(ctx, tx, current.SteamAppID)
		if err != nil {
			return nil, err
		}
		window := ParseReleaseDate(current.ReleaseText, cutoff)
		if current.ComingSoon.Bool && strings.TrimSpace(current.ReleaseText) == "" {
			window.Precision = PrecisionComingSoon
		}
		decision := EvaluateEligibility(EligibilityInput{Now: cutoff, Window: window, ComingSoon: current.ComingSoon.Bool,
			Review: reviewFromCurrent(current), PopularUpcoming: popularUpcoming, PopularNew: popularNew, Wishlisted: wishlisted}, cfg)
		result = append(result, lockedEligibility{current: current, decision: decision})
	}
	return result, nil
}

func projectLockedEligibility(ctx context.Context, tx pgx.Tx, database *store.Store, item lockedEligibility, now time.Time) (bool, error) {
	q := database.Queries.WithTx(tx)
	reason := inactiveReason(item.decision)
	changed := item.current.Active != item.decision.Active || item.current.TombstoneReason != reason
	if _, err := tx.Exec(ctx, `UPDATE catalog_search_current SET active=$2,tombstone_reason=$3 WHERE steam_app_id=$1`,
		item.current.SteamAppID, item.decision.Active, reason); err != nil {
		return false, err
	}
	game, err := q.GetGameByAppID(ctx, item.current.SteamAppID)
	if err == nil {
		sourceChanged, sourceErr := setSharedSource(ctx, tx, uuid.UUID(game.ID.Bytes), "steam_discovery", item.decision.Active, now)
		return changed || sourceChanged, sourceErr
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return changed, nil
	}
	return false, err
}

func resolveLockedEligibility(ctx context.Context, tx pgx.Tx, database *store.Store, item lockedEligibility) error {
	next := pgtype.Timestamptz{}
	if item.decision.NextChangeAt != nil {
		next = pgtype.Timestamptz{Time: item.decision.NextChangeAt.UTC(), Valid: true}
	}
	_, err := database.Queries.WithTx(tx).ResolveDueEligibilityChange(ctx, dbgen.ResolveDueEligibilityChangeParams{
		Active: item.decision.Active, TombstoneReason: inactiveReason(item.decision), NextEligibilityAt: next,
		SteamAppID: item.current.SteamAppID, ExpectedAt: item.current.NextEligibilityAt,
	})
	return err
}

func sameOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.UTC().Equal(right.UTC())
}

func samePGTime(left, right pgtype.Timestamptz) bool {
	if !left.Valid || !right.Valid {
		return left.Valid == right.Valid
	}
	return left.Time.UTC().Equal(right.Time.UTC())
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
		if err := s.repo.MarkCandidateSetDirtyAt(ctx, tx, EligibilityChange{GameID: gameID, Kind: EligibilityChangeSemanticContent}, s.now().UTC()); err != nil {
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
