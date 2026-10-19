package profileoverlay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/preferences"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/recommendations"
	"next-game-crush/internal/store"
)

const (
	BackfillJobKind = "profile_overlay.boundary_backfill"
	BackfillVersion = 1
)

type BackfillPayload struct {
	Version int       `json:"version"`
	UserID  uuid.UUID `json:"user_id"`
}

type profileResult struct {
	BuildKey string `json:"profileBuildKey"`
	Version  int64  `json:"profileVersion"`
}

type overlayResult struct {
	Version int64 `json:"preferenceOverlayVersion"`
}

type rankingResult struct {
	Version int64 `json:"rankingVersion"`
}

func EnsureBoundaryBackfill(ctx context.Context, database *store.Store, queue *jobs.Queue) error {
	if database == nil || database.Pool == nil || queue == nil {
		return errors.New("boundary backfill dependencies are required")
	}
	rows, err := database.Pool.Query(ctx, `SELECT id FROM users ORDER BY id`)
	if err != nil {
		return fmt.Errorf("list boundary backfill users: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var userID uuid.UUID
		if err := rows.Scan(&userID); err != nil {
			return fmt.Errorf("scan boundary backfill user: %w", err)
		}
		payload, _ := json.Marshal(BackfillPayload{Version: BackfillVersion, UserID: userID})
		if _, err := queue.Enqueue(ctx, jobs.Spec{
			Lane: jobs.LaneInteractive, Kind: BackfillJobKind, Stage: "profile_overlay_backfill",
			IdempotencyKey: boundaryKey(userID), Payload: payload, MaxAttempts: 3,
			Priority: 90, Required: true, UserID: userID,
		}); err != nil {
			return fmt.Errorf("enqueue boundary backfill: %w", err)
		}
	}
	return rows.Err()
}

func RegisterBoundaryBackfill(worker *jobs.Worker, queue *jobs.Queue) error {
	if worker == nil || queue == nil {
		return errors.New("boundary backfill worker and queue are required")
	}
	return worker.Register(BackfillJobKind, func(ctx context.Context, lease jobs.Lease) error {
		var payload BackfillPayload
		if json.Unmarshal(lease.Payload, &payload) != nil || payload.Version != BackfillVersion || payload.UserID == uuid.Nil || payload.UserID != lease.UserID || lease.WorkflowID == uuid.Nil {
			return errors.New("unsupported boundary backfill payload schema")
		}
		prefix := boundaryKey(payload.UserID)
		profileKey := prefix + ":activity:v1"
		profilePayload, _ := json.Marshal(profile.JobPayload{Version: profile.JobPayloadVersion, UserID: payload.UserID, SourceWorkflowID: lease.WorkflowID})
		ready, err := queue.AwaitChildren(ctx, lease, []jobs.Spec{{
			Lane: jobs.LaneInteractive, Kind: profile.JobKind, Stage: "profile", Required: true,
			IdempotencyKey: profileKey, Payload: profilePayload, MaxAttempts: 3, Priority: lease.Priority, UserID: payload.UserID,
		}})
		if err != nil {
			return err
		}
		if !ready {
			return jobs.ErrDeferred
		}
		var builtProfile profileResult
		if err := queue.CompletedChildResult(ctx, lease.ID, payload.UserID, profile.JobKind, "profile", &builtProfile); err != nil {
			return err
		}
		if strings.TrimSpace(builtProfile.BuildKey) == "" || builtProfile.Version < 1 {
			return errors.New("boundary activity profile result is incomplete")
		}

		overlayKey := prefix + ":overlay:v1"
		overlayPayload, _ := json.Marshal(preferences.OverlayJobPayload{Version: 1, UserID: payload.UserID})
		ready, err = queue.AwaitChildren(ctx, lease, []jobs.Spec{{
			Lane: jobs.LaneInteractive, Kind: preferences.OverlayJobKind, Stage: "preferences", Required: true,
			IdempotencyKey: overlayKey, Payload: overlayPayload, MaxAttempts: 3, Priority: lease.Priority, UserID: payload.UserID,
		}})
		if err != nil {
			return err
		}
		if !ready {
			return jobs.ErrDeferred
		}
		var builtOverlay overlayResult
		if err := queue.CompletedChildResult(ctx, lease.ID, payload.UserID, preferences.OverlayJobKind, "preferences", &builtOverlay); err != nil {
			return err
		}
		if builtOverlay.Version < 1 {
			return errors.New("boundary preference overlay result is incomplete")
		}

		rankingKey := prefix + ":ranking:v1"
		rankingPayload, _ := json.Marshal(recommendations.JobPayload{
			Version: recommendations.JobPayloadVersion, UserID: payload.UserID,
			ProfileBuildKey: builtProfile.BuildKey, RequestKey: rankingKey,
		})
		ready, err = queue.AwaitChildren(ctx, lease, []jobs.Spec{{
			Lane: jobs.LaneInteractive, Kind: recommendations.JobKind, Stage: "recommendations", Required: true,
			IdempotencyKey: rankingKey, Payload: rankingPayload, MaxAttempts: 3, Priority: lease.Priority, UserID: payload.UserID,
		}})
		if err != nil {
			return err
		}
		if !ready {
			return jobs.ErrDeferred
		}
		var builtRanking rankingResult
		if err := queue.CompletedChildResult(ctx, lease.ID, payload.UserID, recommendations.JobKind, "recommendations", &builtRanking); err != nil {
			return err
		}
		if builtRanking.Version < 1 {
			return errors.New("boundary ranking result is incomplete")
		}
		return queue.RecordResult(ctx, lease, map[string]any{
			"version": BackfillVersion, "profileVersion": builtProfile.Version,
			"preferenceOverlayVersion": builtOverlay.Version, "rankingVersion": builtRanking.Version,
		})
	})
}

func boundaryKey(userID uuid.UUID) string {
	return fmt.Sprintf("profile-overlay:boundary:v%d:%s", BackfillVersion, userID)
}
