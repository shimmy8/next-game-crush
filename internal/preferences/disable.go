package preferences

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/jobs"
	dbgen "next-game-crush/internal/store/sqlc"
)

const DisableJobKind = "preferences.disable"

type DisableAccepted struct {
	AdjustmentID           uuid.UUID `json:"adjustmentId"`
	RebuildJobID           uuid.UUID `json:"rebuildJobId"`
	PreviousRankingVersion int64     `json:"previousRankingVersion"`
	AlreadyInactive        bool      `json:"alreadyInactive"`
}
type DisablePayload struct {
	Version      int       `json:"version"`
	AdjustmentID uuid.UUID `json:"adjustment_id"`
}
type DisableWorkflow struct {
	AdjustmentID, UserID, JobID uuid.UUID
	PreviousRankingVersion      int64
}

func (r *SQLRepository) DisableAndEnqueue(ctx context.Context, userID, adjustmentID uuid.UUID) (DisableAccepted, error) {
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return DisableAccepted{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text,0))`, userID); err != nil {
		return DisableAccepted{}, err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1) AND NOT EXISTS(SELECT 1 FROM deleted_user_tombstones WHERE user_id=$1)`, userID).Scan(&exists); err != nil {
		return DisableAccepted{}, err
	}
	if !exists {
		return DisableAccepted{}, pgx.ErrNoRows
	}
	queries := r.store.Queries.WithTx(tx)
	row, err := queries.LockPreferenceAdjustment(ctx, dbgen.LockPreferenceAdjustmentParams{UserID: uuidToPG(userID), ID: uuidToPG(adjustmentID)})
	if err != nil {
		return DisableAccepted{}, err
	}
	accepted := DisableAccepted{AdjustmentID: adjustmentID, AlreadyInactive: !row.Active}
	if !row.Active {
		if !row.DisableRebuildJobID.Valid || !row.DisablePreviousRankingVersion.Valid {
			return DisableAccepted{}, errors.New("disabled preference workflow is unavailable")
		}
		accepted.RebuildJobID = uuid.UUID(row.DisableRebuildJobID.Bytes)
		accepted.PreviousRankingVersion = row.DisablePreviousRankingVersion.Int64
	} else {
		if err = tx.QueryRow(ctx, `SELECT COALESCE(max(ranking_version),0) FROM recommendation_runs WHERE user_id=$1 AND published`, userID).Scan(&accepted.PreviousRankingVersion); err != nil {
			return DisableAccepted{}, err
		}
		if accepted.PreviousRankingVersion < 1 {
			return DisableAccepted{}, errors.New("published ranking is required")
		}
		payload, _ := json.Marshal(DisablePayload{Version: 1, AdjustmentID: adjustmentID})
		accepted.RebuildJobID, err = jobs.EnqueueInTx(ctx, queries, jobs.Spec{Lane: jobs.LaneInteractive, Kind: DisableJobKind, Stage: "preferences", UserID: userID, Priority: 100, Required: true, IdempotencyKey: "adjustment:" + adjustmentID.String() + ":disable:v1", Payload: payload, MaxAttempts: 3})
		if err != nil {
			return DisableAccepted{}, err
		}
		count, err := queries.MarkPreferenceAdjustmentDisabled(ctx, dbgen.MarkPreferenceAdjustmentDisabledParams{UserID: uuidToPG(userID), ID: uuidToPG(adjustmentID), DisableRebuildJobID: uuidToPG(accepted.RebuildJobID), DisablePreviousRankingVersion: pgtype.Int8{Int64: accepted.PreviousRankingVersion, Valid: true}, DisabledAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}})
		if err != nil {
			return DisableAccepted{}, err
		}
		if count != 1 {
			return DisableAccepted{}, pgx.ErrNoRows
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return DisableAccepted{}, err
	}
	return accepted, nil
}

func (r *SQLRepository) DisableWorkflow(ctx context.Context, adjustmentID uuid.UUID) (DisableWorkflow, error) {
	var workflow DisableWorkflow
	err := r.store.Pool.QueryRow(ctx, `SELECT a.id,a.user_id,a.disable_rebuild_job_id,a.disable_previous_ranking_version FROM preference_adjustments a JOIN users u ON u.id=a.user_id JOIN sync_jobs j ON j.id=a.disable_rebuild_job_id AND j.user_id=a.user_id AND j.kind='preferences.disable' WHERE a.id=$1 AND NOT a.active`, adjustmentID).Scan(&workflow.AdjustmentID, &workflow.UserID, &workflow.JobID, &workflow.PreviousRankingVersion)
	return workflow, err
}

func (r *SQLRepository) DisableResult(ctx context.Context, workflow DisableWorkflow) (int64, error) {
	var version int64
	err := r.store.Pool.QueryRow(ctx, `SELECT (j.result->>'rankingVersion')::bigint FROM sync_jobs j JOIN recommendation_runs run ON run.user_id=j.user_id AND run.ranking_version=(j.result->>'rankingVersion')::bigint AND run.published WHERE j.parent_job_id=$1 AND j.user_id=$2 AND j.idempotency_key=$3 AND j.status='succeeded'`, workflow.JobID, workflow.UserID, "adjustment:"+workflow.AdjustmentID.String()+":disable:ranking:v1").Scan(&version)
	return version, err
}
