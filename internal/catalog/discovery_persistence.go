package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
	dbgen "next-game-crush/internal/store/sqlc"
)

func lockDiscoveryLease(ctx context.Context, tx pgx.Tx, lease jobs.Lease) ([]byte, error) {
	var result []byte
	err := tx.QueryRow(ctx, `SELECT result FROM sync_jobs WHERE id=$1 AND status='running' AND worker_id=$2 AND fencing_token=$3 AND lease_until>clock_timestamp() FOR UPDATE`, lease.ID, lease.WorkerID, lease.FencingToken).Scan(&result)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, jobs.ErrLeaseLost
	}
	return result, err
}
func commitDiscoveryLease(ctx context.Context, tx pgx.Tx, lease jobs.Lease) error {
	var owned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sync_jobs WHERE id=$1 AND status='running' AND worker_id=$2 AND fencing_token=$3 AND lease_until>clock_timestamp())`, lease.ID, lease.WorkerID, lease.FencingToken).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return jobs.ErrLeaseLost
	}
	return tx.Commit(ctx)
}
func lockDiscoveryApp(ctx context.Context, tx pgx.Tx, appID int64) error {
	// The advisory namespace serializes first insertion as well as existing rows.
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(481913,$1::integer)`, int32(uint32(appID)))
	return err
}
func (r *DiscoveryRepository) MarkCandidatesChanged(ctx context.Context, tx pgx.Tx, now time.Time) error {
	_, err := tx.Exec(ctx, `WITH live_users AS MATERIALIZED (
	 SELECT u.id AS user_id FROM users u
	 WHERE EXISTS(SELECT 1 FROM preference_profiles p WHERE p.user_id=u.id)
	 AND NOT EXISTS(SELECT 1 FROM deleted_user_tombstones d WHERE d.user_id=u.id)
	 ORDER BY u.id FOR KEY SHARE OF u
	)
	 INSERT INTO catalog_rebuild_state(user_id,dirty_revision,dirty_since,first_dirty_at,retry_generation)
	 SELECT user_id,1,$1::timestamptz,$1::timestamptz,0 FROM live_users
	 ON CONFLICT(user_id) DO UPDATE SET dirty_revision=catalog_rebuild_state.dirty_revision+1,
	 dirty_since=COALESCE(catalog_rebuild_state.dirty_since,EXCLUDED.dirty_since),
	 first_dirty_at=COALESCE(catalog_rebuild_state.first_dirty_at,EXCLUDED.first_dirty_at),retry_generation=0`, now)
	return err
}
func (r *DiscoveryRepository) persistQualification(ctx context.Context, lease jobs.Lease, p QualificationPayload, details steam.GameDetails, result Qualification, deck *deckCompatibilityObservation, now time.Time, planner DiscoveryEnrichmentPlanner) error {
	tx, err := r.store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	allowed, err := r.LegacyEnqueueAllowedForUpdate(ctx, tx)
	if err != nil {
		return err
	}
	if !allowed {
		planner = nil
	}
	if _, err = lockDiscoveryLease(ctx, tx, lease); err != nil {
		return err
	}
	if err = lockDiscoveryApp(ctx, tx, p.App.AppID); err != nil {
		return err
	}
	q := r.store.Queries.WithTx(tx)
	previous, previousErr := q.LockDiscoveryObservation(ctx, dbgen.LockDiscoveryObservationParams{SteamAppID: p.App.AppID, LastModified: discoveryTimestamp(p.App.LastModified.Truncate(time.Second))})
	if previousErr != nil && !errors.Is(previousErr, pgx.ErrNoRows) {
		return previousErr
	}
	if previousErr == nil && int(previous.RetryGeneration) > p.RetryGeneration {
		return commitDiscoveryLease(ctx, tx, lease)
	}
	var newer bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_discovery_observations WHERE steam_app_id=$1 AND last_modified>$2 AND status IN ('admitted','outside_horizon','released','not_game','unavailable'))`, p.App.AppID, p.App.LastModified).Scan(&newer); err != nil {
		return err
	}
	attempts := lease.Attempts
	if previousErr == nil {
		if int(previous.RetryGeneration) == p.RetryGeneration && previous.Status != "failed" {
			// A crash after commit may replay the same lease. Retain counters/history,
			// but allow a changed model to create a new guarded enrichment identity.
			if previous.Status == "admitted" && !newer && planner != nil {
				game, e := q.GetGameByAppID(ctx, p.App.AppID)
				if e != nil {
					return e
				}
				var stored steam.GameDetails
				if json.Unmarshal(game.SourceSnapshot, &stored) == nil {
					if e = enqueueDiscoveryFeatures(ctx, q, planner, uuid.UUID(game.ID.Bytes), stored, p.Priority); e != nil {
						return e
					}
				}
			}
			return commitDiscoveryLease(ctx, tx, lease)
		}
		attempts += int(previous.Attempts)
	}
	args := dbgen.UpsertDiscoveryObservationParams{SteamAppID: p.App.AppID, LastModified: discoveryTimestamp(p.App.LastModified.Truncate(time.Second)), Status: result.Status, Attempts: int32(attempts), RetryGeneration: int32(p.RetryGeneration), Precision: text(string(result.Window.Precision)), RawReleaseText: result.Window.Raw}
	if result.Window.Date != nil {
		args.ReleaseDate = pgtype.Date{Time: *result.Window.Date, Valid: true}
	}
	if err = q.UpsertDiscoveryObservation(ctx, args); err != nil {
		return err
	}
	if newer {
		return commitDiscoveryLease(ctx, tx, lease)
	}
	old, oldErr := q.GetGameByAppID(ctx, p.App.AppID)
	if oldErr != nil && !errors.Is(oldErr, pgx.ErrNoRows) {
		return oldErr
	}
	if result.Status == "unavailable" {
		if oldErr == nil {
			if _, err = tx.Exec(ctx, `UPDATE games SET availability='unavailable',last_attempt_at=$2 WHERE id=$1`, old.ID, now); err != nil {
				return err
			}
			if err = q.UpsertCatalogFetchState(ctx, dbgen.UpsertCatalogFetchStateParams{GameID: old.ID, Availability: string(steam.Unavailable), LastAttemptAt: timestamptz(now), Error: text("Steam catalog entry unavailable")}); err != nil {
				return err
			}
		}
		return commitDiscoveryLease(ctx, tx, lease)
	}
	if errors.Is(oldErr, pgx.ErrNoRows) && result.Status != "admitted" {
		return commitDiscoveryLease(ctx, tx, lease)
	}
	snapshot, err := json.Marshal(details)
	if err != nil {
		return errors.New("invalid discovery snapshot")
	}
	gameID := uuid.New()
	if oldErr == nil {
		gameID = uuid.UUID(old.ID.Bytes)
	}
	// Keep the source snapshot hash compatible with every existing metadata reader.
	hash := fmt.Sprintf("%x", sha256.Sum256(snapshot))
	game, err := q.UpsertCatalogGame(ctx, dbgen.UpsertCatalogGameParams{ID: uuidPG(gameID), SteamAppID: p.App.AppID, Name: details.Name, Source: "steam_store", LastSuccessfulFetchAt: timestamptz(now), SourceSnapshot: snapshot, ContentHash: text(hash)})
	if err != nil {
		return err
	}
	if err = replaceSteamModes(ctx, q, gameID, details.Categories, hash, now); err != nil {
		return err
	}
	if err = q.UpsertGamePlatforms(ctx, dbgen.UpsertGamePlatformsParams{GameID: game.ID, Windows: details.Platforms.Windows, Mac: details.Platforms.Mac, Linux: details.Platforms.Linux, PlatformsLastSuccessfulFetchAt: timestamptz(now)}); err != nil {
		return err
	}
	if deck != nil {
		if err = recordDeckCompatibility(ctx, q, game.ID, *deck, now); err != nil {
			return err
		}
	}
	if err = q.UpsertCatalogFetchState(ctx, dbgen.UpsertCatalogFetchStateParams{GameID: game.ID, Availability: string(steam.Available), LastAttemptAt: timestamptz(now), LastSuccessfulFetchAt: timestamptz(now)}); err != nil {
		return err
	}
	if err = observeReleaseWithQueries(ctx, q, game.ID, result.Window, "steam_store", now); err != nil {
		return err
	}
	changed, err := setSharedSource(ctx, tx, gameID, "steam_discovery", result.Status == "admitted", now)
	if err != nil {
		return err
	}
	if result.Status == "released" || result.Status == "not_game" {
		tag, e := tx.Exec(ctx, `UPDATE catalog_entry_sources SET active=false WHERE game_id=$1 AND active`, gameID)
		if e != nil {
			return e
		}
		changed = changed || tag.RowsAffected() > 0
		if _, e = tx.Exec(ctx, `DELETE FROM catalog_entries WHERE game_id=$1`, gameID); e != nil {
			return e
		}
	}
	if result.Status == "admitted" {
		if planner != nil {
			if err = enqueueDiscoveryFeatures(ctx, q, planner, gameID, details, p.Priority); err != nil {
				return err
			}
		}
		changed = changed || oldErr != nil || !sameDiscoveryContent(old.SourceSnapshot, details) || releaseSnapshotChanged(old.SourceSnapshot, details)
	}
	// A discovery policy rejection can leave seed/wishlist eligibility intact.
	// Those candidates still need a rebuild when trustworthy metadata changes.
	if oldErr == nil && (!sameDiscoveryContent(old.SourceSnapshot, details) || releaseSnapshotChanged(old.SourceSnapshot, details) || platformSnapshotChanged(old.SourceSnapshot, details)) {
		var candidate bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_entries WHERE game_id=$1) OR EXISTS(SELECT 1 FROM wishlist_entries WHERE game_id=$1)`, gameID).Scan(&candidate); err != nil {
			return err
		}
		changed = changed || candidate
	}
	if changed {
		if err = r.MarkCandidatesChanged(ctx, tx, now); err != nil {
			return err
		}
	}
	return commitDiscoveryLease(ctx, tx, lease)
}
func enqueueDiscoveryFeatures(ctx context.Context, q *dbgen.Queries, planner DiscoveryEnrichmentPlanner, gameID uuid.UUID, details steam.GameDetails, priority int) error {
	spec, err := planner.PlanGameFeatureJob(ctx, gameID, discoveryContentHash(details), priority)
	if err != nil {
		return err
	}
	if spec.Kind == "" {
		return nil
	}
	// Content/model hashes identify logical input, while the generation identifies
	// a new occurrence of that input. Singleton feature/vector rows may have been
	// replaced by B when canonical content returns from A to B and then back to A.
	// All callers hold a transaction; serialize both qualification and cached-page
	// planning here so replay cannot spend a new execution generation twice.
	prefix := "catalog:enrichment:v1:" + gameID.String() + ":"
	if err = q.LockCatalogEnrichmentExecution(ctx, prefix); err != nil {
		return err
	}
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(spec.Kind+":"+spec.IdempotencyKey)))
	latest, err := q.GetLatestCatalogEnrichmentExecution(ctx, prefix)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	generation := int64(1)
	if err == nil {
		if strings.HasSuffix(latest.IdempotencyKey, ":"+identity) {
			// Do not rearm or edit a succeeded/dead execution merely because the
			// same observation is seen again. Pending retries keep their identity.
			if latest.Status == "succeeded" || latest.Status == "dead" {
				return nil
			}
			spec.IdempotencyKey = latest.IdempotencyKey
			_, err = jobs.EnqueueInTx(ctx, q, spec)
			return err
		}
		if latest.Generation == math.MaxInt64 {
			return errors.New("catalog enrichment execution generation exhausted")
		}
		generation = latest.Generation + 1
	}
	spec.IdempotencyKey = fmt.Sprintf("%s%d:%s", prefix, generation, identity)
	if binder, ok := planner.(interface {
		BindGameFeatureExecution(jobs.Spec, int64, string) (jobs.Spec, error)
	}); ok {
		spec, err = binder.BindGameFeatureExecution(spec, generation, spec.IdempotencyKey)
		if err != nil {
			return err
		}
	}
	_, err = jobs.EnqueueInTx(ctx, q, spec)
	return err
}

// Qualification observations cache admission, not enrichment capability. Keep
// checking the current content/model identity even when no metadata attempt is
// needed, and derive it only from a validated successful canonical snapshot.
func (r *DiscoveryRepository) enqueueCachedDiscoveryFeatures(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, planner DiscoveryEnrichmentPlanner, app DiscoveredApp, priority int) error {
	var gameID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM games WHERE steam_app_id=$1 FOR SHARE`, app.AppID).Scan(&gameID); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	game, err := q.GetGameByAppID(ctx, app.AppID)
	if err != nil {
		return err
	}
	details, valid := validatedStoredSteamDetails(game)
	if !valid || !details.ReleaseDate.ComingSoon || !strings.EqualFold(strings.TrimSpace(details.Type), "game") {
		return nil
	}
	var active bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_entry_sources WHERE game_id=$1 AND source='steam_discovery' AND active)`, gameID).Scan(&active); err != nil {
		return err
	}
	if !active {
		return nil
	}
	return enqueueDiscoveryFeatures(ctx, q, planner, gameID, details, priority)
}
func sameDiscoveryContent(raw []byte, details steam.GameDetails) bool {
	var old steam.GameDetails
	return json.Unmarshal(raw, &old) == nil && discoveryContentHash(old) == discoveryContentHash(details)
}
func releaseSnapshotChanged(raw []byte, details steam.GameDetails) bool {
	var old steam.GameDetails
	return json.Unmarshal(raw, &old) != nil || old.ReleaseDate != details.ReleaseDate
}
func platformSnapshotChanged(raw []byte, details steam.GameDetails) bool {
	var old steam.GameDetails
	return json.Unmarshal(raw, &old) != nil || old.Platforms != details.Platforms
}
