package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/observability"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

const PayloadVersion = 1

var (
	ErrPreflightBlocked = errors.New("Steam synchronization blocked by privacy preflight")
	ErrPreflightFailed  = errors.New("Steam privacy preflight failed")
)

type Payload struct {
	Version int       `json:"version"`
	UserID  uuid.UUID `json:"user_id"`
}

type StageStatus struct {
	Kind                  string     `json:"kind"`
	Stage                 string     `json:"stage"`
	Status                string     `json:"status"`
	Availability          string     `json:"availability,omitempty"`
	LastSuccessfulFetchAt *time.Time `json:"lastSuccessfulFetchAt,omitempty"`
	Error                 string     `json:"error,omitempty"`
}

type Status struct {
	ID                    uuid.UUID     `json:"id"`
	Stage                 string        `json:"stage"`
	Status                string        `json:"status"`
	Progress              int           `json:"progress"`
	LastSuccessfulFetchAt *time.Time    `json:"lastSuccessfulFetchAt,omitempty"`
	Error                 string        `json:"error,omitempty"`
	Stages                []StageStatus `json:"stages"`
}

type PreflightResult struct {
	CanSync               bool             `json:"canSync"`
	ProfileVisibility     steam.Visibility `json:"profileVisibility"`
	GameDetailsVisibility steam.Visibility `json:"gameDetailsVisibility"`
	Reason                *string          `json:"reason"`
	DisplayName           *string          `json:"displayName"`
	AvatarURL             *string          `json:"avatarUrl"`
}

type SteamIdentity struct {
	DisplayName *string `json:"displayName"`
	AvatarURL   *string `json:"avatarUrl"`
}

type Service struct {
	store                *store.Store
	queue                *jobs.Queue
	client               steam.Client
	catalog              *catalog.Service
	now                  func() time.Time
	aiJobs               AIJobPlanner
	rankingJobs          bool
	logger               *observability.Logger
	achievementSnapshots AchievementSnapshotRecorder
}

type ServiceOption func(*Service)

func WithLogger(logger *observability.Logger) ServiceOption {
	return func(service *Service) { service.logger = logger }
}

type AIJobPlanner interface {
	PlanFeatureJobs(context.Context, uuid.UUID, string) ([]jobs.Spec, error)
}

type AchievementSnapshotRecorder interface {
	RecordAchievementSnapshot(context.Context, uuid.UUID, uuid.UUID, steam.AchievementSummary, time.Time) error
}

func (s *Service) SetAIJobPlanner(planner AIJobPlanner) { s.aiJobs = planner }
func (s *Service) SetAchievementSnapshotRecorder(recorder AchievementSnapshotRecorder) {
	s.achievementSnapshots = recorder
}
func (s *Service) EnableRankingJobs() { s.rankingJobs = true }

func NewService(database *store.Store, queue *jobs.Queue, client steam.Client, catalogService *catalog.Service, options ...ServiceOption) (*Service, error) {
	if database == nil || queue == nil || client == nil || catalogService == nil {
		return nil, errors.New("sync store, queue, Steam client, and catalog service are required")
	}
	service := &Service{store: database, queue: queue, client: client, catalog: catalogService, now: time.Now, logger: observability.New(nil)}
	for _, option := range options {
		option(service)
	}
	if service.logger == nil {
		service.logger = observability.New(nil)
	}
	return service, nil
}

func (s *Service) Start(ctx context.Context, userID uuid.UUID) (uuid.UUID, error) {
	if userID == uuid.Nil {
		return uuid.Nil, errors.New("user ID is required")
	}
	preflight, err := s.Preflight(ctx, userID)
	if err != nil {
		return uuid.Nil, err
	}
	if !preflight.CanSync {
		return uuid.Nil, ErrPreflightBlocked
	}
	tx, err := s.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin sync workflow: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "steam.sync:"+userID.String()); err != nil {
		return uuid.Nil, fmt.Errorf("lock sync workflow: %w", err)
	}
	var existing uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM sync_jobs WHERE kind='steam.sync' AND payload->>'user_id'=$1 AND status IN ('queued','running','waiting','retryable') ORDER BY created_at DESC LIMIT 1`, userID.String()).Scan(&existing)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return uuid.Nil, err
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("find active sync: %w", err)
	}
	payload, _ := json.Marshal(Payload{Version: PayloadVersion, UserID: userID})
	id := uuid.New()
	key := fmt.Sprintf("steam.sync:%s:%s", userID, id)
	if _, err := tx.Exec(ctx, `INSERT INTO sync_jobs (id,kind,idempotency_key,payload,max_attempts,stage,required) VALUES ($1,'steam.sync',$2,$3,5,'steam',true)`, id, key, payload); err != nil {
		return uuid.Nil, fmt.Errorf("enqueue sync workflow: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit sync workflow: %w", err)
	}
	return id, nil
}

func (s *Service) Preflight(ctx context.Context, userID uuid.UUID) (PreflightResult, error) {
	if userID == uuid.Nil {
		return PreflightResult{}, errors.New("user ID is required")
	}
	account, err := s.store.Queries.GetSteamAccountForUser(ctx, uuidPG(userID))
	if err != nil {
		return PreflightResult{}, fmt.Errorf("load Steam account: %w", err)
	}
	result := PreflightResult{
		ProfileVisibility:     steam.VisibilityUnknown,
		GameDetailsVisibility: steam.VisibilityUnknown,
		DisplayName:           nullableTextPointer(account.DisplayName),
		AvatarURL:             nullableTextPointer(account.AvatarUrl),
	}
	summary, err := s.client.PlayerSummary(ctx, account.SteamID)
	if err != nil {
		result.Reason = stringPointer("check_failed")
		return result, ErrPreflightFailed
	}
	if err := s.store.Queries.UpdateSteamAccountIdentity(ctx, dbgen.UpdateSteamAccountIdentityParams{
		DisplayName: text(summary.DisplayName),
		AvatarUrl:   text(summary.AvatarURL),
		UserID:      uuidPG(userID),
	}); err != nil {
		return result, fmt.Errorf("update Steam account identity: %w", err)
	}
	if summary.DisplayName != "" {
		result.DisplayName = stringPointer(summary.DisplayName)
	}
	if summary.AvatarURL != "" {
		result.AvatarURL = stringPointer(summary.AvatarURL)
	}
	result.ProfileVisibility = summary.Visibility
	if err := s.store.Queries.UpsertUserSourceVisibility(ctx, dbgen.UpsertUserSourceVisibilityParams{
		UserID: uuidPG(userID), Source: "profile", ResourceKey: "", Visibility: text(string(summary.Visibility)),
	}); err != nil {
		return result, fmt.Errorf("save Steam profile visibility: %w", err)
	}
	if summary.Visibility == steam.VisibilityPrivate {
		result.Reason = stringPointer("profile_private")
		return result, nil
	}
	if summary.Visibility != steam.VisibilityPublic {
		result.ProfileVisibility = steam.VisibilityUnknown
		result.Reason = stringPointer("check_failed")
		return result, ErrPreflightFailed
	}
	games, availability, ownedErr := s.client.ProbeOwnedGames(ctx, account.SteamID)
	if ownedErr != nil || availability == steam.RequestFailed || availability == steam.NotModified {
		result.Reason = stringPointer("check_failed")
		return result, ErrPreflightFailed
	}
	if availability != steam.Available || len(games) == 0 {
		if err := s.saveVisibility(ctx, userID, "owned_games", steam.VisibilityUnknown); err != nil {
			return result, err
		}
		result.Reason = stringPointer("game_details_unavailable")
		return result, nil
	}
	if err := s.saveVisibility(ctx, userID, "owned_games", steam.VisibilityPublic); err != nil {
		return result, err
	}
	result.GameDetailsVisibility = steam.VisibilityPublic
	result.CanSync = true
	return result, nil
}

func (s *Service) SteamIdentity(ctx context.Context, userID uuid.UUID) (SteamIdentity, error) {
	if userID == uuid.Nil {
		return SteamIdentity{}, errors.New("user ID is required")
	}
	account, err := s.store.Queries.GetSteamAccountForUser(ctx, uuidPG(userID))
	if err != nil {
		return SteamIdentity{}, err
	}
	return SteamIdentity{DisplayName: nullableTextPointer(account.DisplayName), AvatarURL: nullableTextPointer(account.AvatarUrl)}, nil
}

func (s *Service) saveVisibility(ctx context.Context, userID uuid.UUID, source string, visibility steam.Visibility) error {
	if err := s.store.Queries.UpsertUserSourceVisibility(ctx, dbgen.UpsertUserSourceVisibilityParams{
		UserID: uuidPG(userID), Source: source, ResourceKey: "", Visibility: text(string(visibility)),
	}); err != nil {
		return fmt.Errorf("save Steam source visibility: %w", err)
	}
	return nil
}

func nullableTextPointer(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	return stringPointer(value.String)
}

func stringPointer(value string) *string { return &value }

// Run performs the complete workflow synchronously. The worker uses the same
// idempotent stage methods but schedules catalog and achievements as child jobs.
func (s *Service) Run(ctx context.Context, userID uuid.UUID) error {
	if err := s.importSteam(ctx, userID); err != nil {
		return err
	}
	if err := s.catalog.RefreshKnown(ctx); err != nil {
		return err
	}
	return s.syncAchievements(ctx, userID)
}

func (s *Service) RegisterHandlers(worker *jobs.Worker) error {
	if worker == nil {
		return errors.New("worker is required")
	}
	if err := worker.Register("steam.sync", s.handleSteamSync); err != nil {
		return err
	}
	if err := worker.Register("catalog.refresh", s.handleCatalogRefresh); err != nil {
		return err
	}
	return worker.Register("achievements.sync", s.handleAchievementsSync)
}

func (s *Service) handleSteamSync(ctx context.Context, lease jobs.Lease) error {
	payload, err := decodePayload(lease.Payload)
	if err != nil {
		return err
	}
	childJobs, err := s.store.Queries.GetJobChildren(ctx, uuidPG(lease.ID))
	if err != nil {
		return fmt.Errorf("inspect synchronization stages: %w", err)
	}
	if len(childJobs) > 0 {
		hasAI, hasProfile, hasRecommendations := false, false, false
		for _, child := range childJobs {
			switch child.Stage {
			case "ai_features":
				hasAI = true
			case "profile":
				hasProfile = true
			case "recommendations":
				hasRecommendations = true
			}
			if child.Required && child.Status != "succeeded" {
				return errors.New("required synchronization stage is incomplete")
			}
		}
		if s.aiJobs != nil && !hasAI {
			specs, err := s.aiJobs.PlanFeatureJobs(ctx, payload.UserID, lease.IdempotencyKey)
			if err != nil {
				return err
			}
			if !s.rankingJobs {
				for _, spec := range specs {
					if _, err := s.queue.Enqueue(ctx, spec); err != nil {
						return err
					}
				}
				return nil
			}
			for i := range specs {
				specs[i].Optional = true
				specs[i].Required = false
			}
			if len(specs) > 0 {
				ready, err := s.queue.AwaitChildren(ctx, lease, specs)
				if err != nil {
					return err
				}
				if !ready {
					return jobs.ErrDeferred
				}
			}
		}
		if !s.rankingJobs {
			return nil
		}
		encoded, _ := json.Marshal(payload)
		if !hasProfile {
			ready, err := s.queue.AwaitChildren(ctx, lease, []jobs.Spec{{Kind: "profile.rebuild", Stage: "profile", Required: true, IdempotencyKey: lease.IdempotencyKey + ":profile:v1", Payload: encoded, MaxAttempts: 3}})
			if err != nil {
				return err
			}
			if !ready {
				return jobs.ErrDeferred
			}
		}
		if !hasRecommendations {
			ready, err := s.queue.AwaitChildren(ctx, lease, []jobs.Spec{{Kind: "recommendations.rebuild", Stage: "recommendations", Required: true, IdempotencyKey: lease.IdempotencyKey + ":recommendations:v1", Payload: encoded, MaxAttempts: 3}})
			if err != nil {
				return err
			}
			if !ready {
				return jobs.ErrDeferred
			}
		}
		return nil
	}
	if err := s.importSteam(ctx, payload.UserID); err != nil {
		return err
	}
	encoded, _ := json.Marshal(payload)
	childSpecs := []jobs.Spec{
		{Kind: "catalog.refresh", Stage: "catalog", Required: true, IdempotencyKey: lease.IdempotencyKey + ":catalog:v1", Payload: encoded, MaxAttempts: 3},
		{Kind: "achievements.sync", Stage: "achievements", Required: true, IdempotencyKey: lease.IdempotencyKey + ":achievements:v1", Payload: encoded, MaxAttempts: 3},
	}
	ready, err := s.queue.AwaitChildren(ctx, lease, childSpecs)
	if err != nil {
		return err
	}
	if !ready {
		return jobs.ErrDeferred
	}
	return nil
}

func (s *Service) handleCatalogRefresh(ctx context.Context, lease jobs.Lease) error {
	if _, err := decodePayload(lease.Payload); err != nil {
		return err
	}
	return s.catalog.RefreshKnown(ctx)
}

func (s *Service) handleAchievementsSync(ctx context.Context, lease jobs.Lease) error {
	payload, err := decodePayload(lease.Payload)
	if err != nil {
		return err
	}
	return s.syncAchievements(ctx, payload.UserID)
}

func decodePayload(raw json.RawMessage) (Payload, error) {
	var payload Payload
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Version != PayloadVersion || payload.UserID == uuid.Nil {
		return Payload{}, errors.New("unsupported sync payload schema")
	}
	return payload, nil
}

func (s *Service) importSteam(ctx context.Context, userID uuid.UUID) error {
	account, err := s.store.Queries.GetSteamAccountForUser(ctx, uuidPG(userID))
	if err != nil {
		return fmt.Errorf("load Steam account: %w", err)
	}
	steamID := account.SteamID
	finishOwned := s.beginSource(ctx, "owned_games")
	owned, ownedAvailability, ownedErr := s.client.OwnedGames(ctx, steamID)
	if err := s.recordState(ctx, userID, "owned_games", "", ownedAvailability, ownedErr); err != nil {
		finishOwned(availability(ownedAvailability, ownedErr), steamDiagnostic{reason: observability.ReasonPersistenceFailed}, sourceCounts{total: len(owned)})
		return err
	}
	if ownedAvailability == steam.Available && ownedErr == nil {
		if err := s.saveOwned(ctx, userID, owned); err != nil {
			finishOwned(string(ownedAvailability), steamDiagnostic{reason: observability.ReasonPersistenceFailed}, sourceCounts{total: len(owned)})
			return err
		}
		snapshot, _ := json.Marshal(owned)
		if _, err := s.store.Pool.Exec(ctx, `UPDATE steam_accounts SET availability='available',last_successful_fetch_at=$2,raw_snapshot=$3 WHERE user_id=$1`, userID, s.now().UTC(), snapshot); err != nil {
			finishOwned(string(ownedAvailability), steamDiagnostic{reason: observability.ReasonPersistenceFailed}, sourceCounts{total: len(owned), available: len(owned)})
			return fmt.Errorf("save Steam account snapshot: %w", err)
		}
	} else {
		if _, err := s.store.Pool.Exec(ctx, `UPDATE steam_accounts SET availability=$2 WHERE user_id=$1`, userID, availability(ownedAvailability, ownedErr)); err != nil {
			finishOwned(availability(ownedAvailability, ownedErr), steamDiagnostic{reason: observability.ReasonPersistenceFailed}, sourceCounts{total: len(owned)})
			return fmt.Errorf("mark Steam account availability: %w", err)
		}
	}
	finishOwned(availability(ownedAvailability, ownedErr), diagnoseSteamFailure(ownedAvailability, ownedErr), itemCounts(len(owned), ownedAvailability, ownedErr))

	finishRecent := s.beginSource(ctx, "recently_played")
	recent, recentAvailability, recentErr := s.client.RecentlyPlayed(ctx, steamID)
	if err := s.recordState(ctx, userID, "recently_played", "", recentAvailability, recentErr); err != nil {
		finishRecent(availability(recentAvailability, recentErr), steamDiagnostic{reason: observability.ReasonPersistenceFailed}, sourceCounts{total: len(recent)})
		return err
	}
	if recentAvailability == steam.Available && recentErr == nil {
		if err := s.saveRecent(ctx, userID, recent); err != nil {
			finishRecent(string(recentAvailability), steamDiagnostic{reason: observability.ReasonPersistenceFailed}, sourceCounts{total: len(recent)})
			return err
		}
	}
	finishRecent(availability(recentAvailability, recentErr), diagnoseSteamFailure(recentAvailability, recentErr), itemCounts(len(recent), recentAvailability, recentErr))

	finishWishlist := s.beginSource(ctx, "wishlist")
	wishlist, wishlistAvailability, wishlistErr := s.client.Wishlist(ctx, steamID)
	if err := s.recordState(ctx, userID, "wishlist", "", wishlistAvailability, wishlistErr); err != nil {
		finishWishlist(availability(wishlistAvailability, wishlistErr), steamDiagnostic{reason: observability.ReasonPersistenceFailed}, sourceCounts{total: len(wishlist)})
		return err
	}
	if wishlistAvailability == steam.Available && wishlistErr == nil {
		if err := s.saveWishlist(ctx, userID, wishlist); err != nil {
			finishWishlist(string(wishlistAvailability), steamDiagnostic{reason: observability.ReasonPersistenceFailed}, sourceCounts{total: len(wishlist)})
			return err
		}
	}
	finishWishlist(availability(wishlistAvailability, wishlistErr), diagnoseSteamFailure(wishlistAvailability, wishlistErr), itemCounts(len(wishlist), wishlistAvailability, wishlistErr))
	return nil
}

func (s *Service) saveOwned(ctx context.Context, userID uuid.UUID, games []steam.OwnedGame) error {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := s.now().UTC()
	appIDs := make([]int64, 0, len(games))
	for _, game := range games {
		appIDs = append(appIDs, game.AppID)
		gameID, err := upsertDiscoveredGame(ctx, tx, game.AppID, game.Name, "steam_library", now)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_games (user_id,game_id,playtime_minutes,source,last_successful_fetch_at,last_played_at) VALUES ($1,$2,$3,'steam_owned',$4,$5)
			ON CONFLICT (user_id,game_id) DO UPDATE SET playtime_minutes=EXCLUDED.playtime_minutes,source=EXCLUDED.source,last_successful_fetch_at=EXCLUDED.last_successful_fetch_at,last_played_at=COALESCE(EXCLUDED.last_played_at,user_games.last_played_at)`, userID, gameID, game.PlaytimeForever, now, nullableTime(game.LastPlayedAt)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_games ug USING games g WHERE ug.game_id=g.id AND ug.user_id=$1 AND NOT (g.steam_app_id=ANY($2::bigint[]))`, userID, appIDs); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) saveRecent(ctx context.Context, userID uuid.UUID, games []steam.RecentGame) error {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := s.now().UTC()
	if _, err := tx.Exec(ctx, `UPDATE user_games SET recently_played_minutes=0 WHERE user_id=$1`, userID); err != nil {
		return err
	}
	for _, game := range games {
		gameID, err := upsertDiscoveredGame(ctx, tx, game.AppID, game.Name, "steam_recent", now)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_games (user_id,game_id,playtime_minutes,recently_played_minutes,source,last_successful_fetch_at,last_played_at) VALUES ($1,$2,$3,$4,'steam_recent',$5,$6)
			ON CONFLICT (user_id,game_id) DO UPDATE SET playtime_minutes=GREATEST(user_games.playtime_minutes,EXCLUDED.playtime_minutes),recently_played_minutes=EXCLUDED.recently_played_minutes,last_successful_fetch_at=EXCLUDED.last_successful_fetch_at,last_played_at=EXCLUDED.last_played_at`, userID, gameID, game.PlaytimeForever, game.PlaytimeTwoWeeks, now, nullableTime(game.LastPlayedAt)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Service) saveWishlist(ctx context.Context, userID uuid.UUID, items []steam.WishlistItem) error {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := s.now().UTC()
	appIDs := make([]int64, 0, len(items))
	for _, item := range items {
		appIDs = append(appIDs, item.AppID)
		gameID, err := upsertDiscoveredGame(ctx, tx, item.AppID, fmt.Sprintf("Steam App %d", item.AppID), "steam_wishlist", now)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO wishlist_entries (user_id,game_id,priority,source,last_successful_fetch_at) VALUES ($1,$2,$3,'steam_wishlist',$4)
			ON CONFLICT (user_id,game_id) DO UPDATE SET priority=EXCLUDED.priority,last_successful_fetch_at=EXCLUDED.last_successful_fetch_at`, userID, gameID, item.Priority, now); err != nil {
			return err
		}
		released, err := knownReleased(ctx, tx, gameID)
		if err != nil {
			return err
		}
		if !released {
			if _, err := tx.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin,added_at) VALUES($1,'wishlist',$2) ON CONFLICT (game_id) DO UPDATE SET origin=CASE WHEN catalog_entries.origin='seed' THEN 'seed' ELSE 'wishlist' END`, gameID, now); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM wishlist_entries w USING games g WHERE w.game_id=g.id AND w.user_id=$1 AND NOT (g.steam_app_id=ANY($2::bigint[]))`, userID, appIDs); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM catalog_entries ce USING games g WHERE ce.game_id=g.id AND ce.origin='wishlist' AND NOT (g.steam_app_id=ANY($1::bigint[]))`, appIDs); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type dbtx interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func knownReleased(ctx context.Context, tx dbtx, gameID uuid.UUID) (bool, error) {
	var snapshot []byte
	if err := tx.QueryRow(ctx, `SELECT source_snapshot FROM games WHERE id=$1`, gameID).Scan(&snapshot); err != nil {
		return false, err
	}
	if len(snapshot) == 0 {
		return false, nil
	}
	var details steam.GameDetails
	if err := json.Unmarshal(snapshot, &details); err != nil || details.AppID == 0 {
		return false, nil
	}
	return !details.ReleaseDate.ComingSoon, nil
}

func upsertDiscoveredGame(ctx context.Context, tx dbtx, appID int64, name, source string, now time.Time) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `INSERT INTO games (id,steam_app_id,name,source,last_successful_fetch_at,availability,last_attempt_at) VALUES ($1,$2,$3,$4,$5,'available',$5)
		ON CONFLICT (steam_app_id) DO UPDATE SET name=CASE WHEN games.name LIKE 'Steam App %' THEN EXCLUDED.name ELSE games.name END,updated_at=now()
		RETURNING id`, uuid.New(), appID, name, source, now).Scan(&id)
	return id, err
}

func (s *Service) recordState(ctx context.Context, userID uuid.UUID, source, resource string, state steam.Availability, sourceErr error) error {
	now := s.now().UTC()
	success := pgtype.Timestamptz{}
	if (state == steam.Available && sourceErr == nil) || (state == steam.NotModified && errors.Is(sourceErr, steam.ErrNotModified)) {
		success = timestamptz(now)
	}
	safe := pgtype.Text{}
	if sourceErr != nil && !errors.Is(sourceErr, steam.ErrNotModified) {
		safe = text("Steam " + strings.ReplaceAll(source, "_", " ") + " request failed")
	}
	return s.store.Queries.UpsertUserSourceState(ctx, dbgen.UpsertUserSourceStateParams{UserID: uuidPG(userID), Source: source, ResourceKey: resource, Availability: availability(state, sourceErr), LastAttemptAt: timestamptz(now), LastSuccessfulFetchAt: success, Error: safe})
}

func availability(state steam.Availability, err error) string {
	if state == steam.NotModified && errors.Is(err, steam.ErrNotModified) {
		return string(steam.NotModified)
	}
	if err != nil {
		return string(steam.RequestFailed)
	}
	if state == "" {
		return string(steam.RequestFailed)
	}
	return string(state)
}

func (s *Service) syncAchievements(ctx context.Context, userID uuid.UUID) error {
	finish := s.beginSource(ctx, "achievements")
	counts := sourceCounts{}
	account, err := s.store.Queries.GetSteamAccountForUser(ctx, uuidPG(userID))
	if err != nil {
		finish(string(steam.RequestFailed), steamDiagnostic{reason: observability.ReasonPersistenceFailed}, counts)
		return err
	}
	rows, err := s.store.Queries.ListUserGamesForAchievements(ctx, uuidPG(userID))
	if err != nil {
		finish(string(steam.RequestFailed), steamDiagnostic{reason: observability.ReasonPersistenceFailed}, counts)
		return err
	}
	games := make([]UserGame, 0, len(rows))
	for _, row := range rows {
		games = append(games, UserGame{AppID: row.SteamAppID, Name: row.Name, PlaytimeMinutes: row.PlaytimeMinutes, RecentMinutes: row.RecentlyPlayedMinutes, LastPlayedAt: row.LastPlayedAt.Time})
	}
	targets := SelectAchievementTargets(games, AchievementTargetLimit)
	counts.total = len(targets)
	availabilityOutcome := steam.Available
	diagnostic := steamDiagnostic{}
	for _, appID := range targets {
		summary, state, sourceErr := s.client.Achievements(ctx, account.SteamID, appID)
		if err := s.recordState(ctx, userID, "achievements", strconv.FormatInt(appID, 10), state, sourceErr); err != nil {
			counts.failed++
			finish(string(steam.RequestFailed), steamDiagnostic{reason: observability.ReasonPersistenceFailed}, counts)
			return err
		}
		if state == steam.NotModified && errors.Is(sourceErr, steam.ErrNotModified) {
			counts.available++
			continue
		}
		if state != steam.Available || sourceErr != nil {
			if state == steam.Unavailable {
				counts.unavailable++
				if availabilityOutcome == steam.Available {
					availabilityOutcome = steam.Unavailable
				}
			} else {
				counts.failed++
				availabilityOutcome = steam.RequestFailed
			}
			candidateDiagnostic := diagnoseSteamFailure(state, sourceErr)
			if state != steam.Unavailable || diagnostic.reason == "" {
				diagnostic = candidateDiagnostic
			}
			continue
		}
		game, err := s.store.Queries.GetGameByAppID(ctx, appID)
		if err != nil {
			counts.failed++
			finish(string(steam.RequestFailed), steamDiagnostic{reason: observability.ReasonPersistenceFailed}, counts)
			return err
		}
		now := s.now().UTC()
		if achievementMetadataMissing(summary) {
			schema, schemaState, schemaErr := s.client.AchievementSchema(ctx, appID)
			if err := s.recordState(ctx, userID, "achievement_schema", strconv.FormatInt(appID, 10), schemaState, schemaErr); err != nil {
				counts.failed++
				finish(string(steam.RequestFailed), steamDiagnostic{reason: observability.ReasonPersistenceFailed}, counts)
				return err
			}
			if schemaState == steam.Available && schemaErr == nil {
				summary = mergeAchievementSchema(summary, schema)
			}
		}
		if s.achievementSnapshots != nil {
			err = s.achievementSnapshots.RecordAchievementSnapshot(ctx, userID, uuid.UUID(game.ID.Bytes), summary, now)
		} else {
			raw, _ := json.Marshal(summary)
			err = s.store.Queries.UpsertAchievementSummary(ctx, dbgen.UpsertAchievementSummaryParams{UserID: uuidPG(userID), GameID: game.ID, Achieved: int32(summary.Unlocked), Total: int32(summary.Total), Source: "steam", LastSuccessfulFetchAt: timestamptz(now), RawSnapshot: raw})
		}
		if err != nil {
			counts.failed++
			finish(string(steam.RequestFailed), steamDiagnostic{reason: observability.ReasonPersistenceFailed}, counts)
			return err
		}
		counts.available++
	}
	finish(string(availabilityOutcome), diagnostic, counts)
	return nil
}

type sourceCounts struct {
	total       int
	available   int
	unavailable int
	failed      int
}

func itemCounts(total int, state steam.Availability, err error) sourceCounts {
	counts := sourceCounts{total: total}
	if state == steam.Available && err == nil {
		counts.available = total
	} else if state == steam.Unavailable {
		counts.unavailable = total
	} else if err != nil || state == steam.RequestFailed {
		counts.failed = total
	}
	return counts
}

func (s *Service) beginSource(ctx context.Context, source string) func(string, steamDiagnostic, sourceCounts) {
	started := time.Now()
	correlation := observability.CorrelationFromContext(ctx)
	s.logger.SyncSource(ctx, observability.SyncSourceEvent{Stage: observability.SourceStarted, Correlation: correlation, Source: source})
	return func(sourceAvailability string, diagnostic steamDiagnostic, counts sourceCounts) {
		s.logger.SyncSource(ctx, observability.SyncSourceEvent{
			Stage: observability.SourceFinished, Correlation: correlation, Source: source,
			Availability: sourceAvailability, Reason: diagnostic.reason, Duration: time.Since(started),
			TotalCount: counts.total, AvailableCount: counts.available,
			UnavailableCount: counts.unavailable, FailedCount: counts.failed,
			UpstreamStatus: diagnostic.upstreamStatus,
		})
	}
}

type steamDiagnostic struct {
	reason         observability.Reason
	upstreamStatus int
}

func diagnoseSteamFailure(state steam.Availability, err error) steamDiagnostic {
	if err == nil {
		switch state {
		case steam.Available, steam.NotModified:
			return steamDiagnostic{}
		case steam.Unavailable:
			return steamDiagnostic{reason: observability.ReasonUpstreamUnavailable}
		default:
			return steamDiagnostic{reason: observability.ReasonInternalError}
		}
	}
	if errors.Is(err, steam.ErrNotModified) && state == steam.NotModified {
		return steamDiagnostic{}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return steamDiagnostic{reason: observability.ReasonTimeout}
	}
	if errors.Is(err, context.Canceled) {
		return steamDiagnostic{reason: observability.ReasonInternalError}
	}
	var requestErr *steam.RequestError
	if errors.As(err, &requestErr) && requestErr != nil {
		diagnostic := steamDiagnostic{}
		if requestErr.StatusCode >= 400 && requestErr.StatusCode <= 599 {
			diagnostic.upstreamStatus = requestErr.StatusCode
		}
		switch requestErr.StatusCode {
		case http.StatusTooManyRequests:
			diagnostic.reason = observability.ReasonRateLimited
		case http.StatusRequestTimeout, http.StatusGatewayTimeout:
			diagnostic.reason = observability.ReasonTimeout
		default:
			switch {
			case requestErr.StatusCode >= 400 && requestErr.StatusCode <= 499:
				diagnostic.reason = observability.ReasonUpstreamRejected
			case requestErr.StatusCode >= 500 && requestErr.StatusCode <= 599:
				diagnostic.reason = observability.ReasonUpstreamUnavailable
			}
		}
		if diagnostic.reason != "" {
			return diagnostic
		}
		if requestErr.StatusCode == 0 && requestErr.Temporary {
			return steamDiagnostic{reason: observability.ReasonUpstreamUnavailable}
		}
	}
	if errors.Is(err, steam.ErrInvalidResponse) || errors.Is(err, steam.ErrResponseTooLarge) {
		return steamDiagnostic{reason: observability.ReasonMalformedResponse}
	}
	if errors.Is(err, steam.ErrUnavailable) || state == steam.Unavailable {
		return steamDiagnostic{reason: observability.ReasonUpstreamUnavailable}
	}
	return steamDiagnostic{reason: observability.ReasonInternalError}
}

func (s *Service) GetStatus(ctx context.Context, userID, jobID uuid.UUID) (Status, error) {
	job, err := s.store.Queries.GetSyncJob(ctx, uuidPG(jobID))
	if err != nil {
		return Status{}, err
	}
	payload, err := decodePayload(job.Payload)
	if err != nil || payload.UserID != userID {
		return Status{}, pgx.ErrNoRows
	}
	children, err := s.store.Queries.GetJobChildren(ctx, uuidPG(jobID))
	if err != nil {
		return Status{}, err
	}
	stages := []StageStatus{{Kind: job.Kind, Stage: job.Stage, Status: job.Status, Error: safeError(job.LastError)}}
	for _, child := range children {
		stages = append(stages, StageStatus{Kind: child.Kind, Stage: child.Stage, Status: child.Status, Error: safeError(child.LastError)})
	}
	sourceRows, sourceErr := s.store.Pool.Query(ctx, `SELECT source,resource_key,availability,last_successful_fetch_at,error FROM user_source_states WHERE user_id=$1 ORDER BY source,resource_key`, userID)
	if sourceErr != nil {
		return Status{}, fmt.Errorf("load synchronization source states: %w", sourceErr)
	}
	defer sourceRows.Close()
	for sourceRows.Next() {
		var source, resource, sourceAvailability string
		var sourceLastSuccess *time.Time
		var sourceError *string
		if err := sourceRows.Scan(&source, &resource, &sourceAvailability, &sourceLastSuccess, &sourceError); err != nil {
			return Status{}, err
		}
		stageName := source
		if resource != "" {
			stageName += ":" + resource
		}
		item := StageStatus{Kind: "steam.source", Stage: stageName, Status: "ready", Availability: sourceAvailability, LastSuccessfulFetchAt: sourceLastSuccess}
		if sourceError != nil {
			item.Status = "degraded"
			item.Error = "Steam source request failed"
		} else if sourceAvailability == string(steam.Unavailable) {
			item.Status = "degraded"
			item.Error = "Steam source unavailable"
		}
		stages = append(stages, item)
	}
	if err := sourceRows.Err(); err != nil {
		return Status{}, err
	}
	catalogRows, catalogErr := s.store.Pool.Query(ctx, `SELECT g.steam_app_id,cfs.availability,cfs.last_successful_fetch_at,cfs.error
		FROM catalog_fetch_states cfs JOIN games g ON g.id=cfs.game_id JOIN catalog_entries ce ON ce.game_id=g.id ORDER BY g.steam_app_id`)
	if catalogErr != nil {
		return Status{}, fmt.Errorf("load catalog source states: %w", catalogErr)
	}
	defer catalogRows.Close()
	for catalogRows.Next() {
		var appID int64
		var catalogAvailability string
		var catalogLastSuccess *time.Time
		var catalogError *string
		if err := catalogRows.Scan(&appID, &catalogAvailability, &catalogLastSuccess, &catalogError); err != nil {
			return Status{}, err
		}
		item := StageStatus{Kind: "steam.catalog", Stage: "catalog:" + strconv.FormatInt(appID, 10), Status: "ready", Availability: catalogAvailability, LastSuccessfulFetchAt: catalogLastSuccess}
		if catalogError != nil || catalogAvailability == string(steam.Unavailable) || catalogAvailability == string(steam.RequestFailed) {
			item.Status = "degraded"
			item.Error = "Steam catalog metadata unavailable"
		}
		stages = append(stages, item)
	}
	if err := catalogRows.Err(); err != nil {
		return Status{}, err
	}
	var last *time.Time
	_ = s.store.Pool.QueryRow(ctx, `SELECT max(last_successful_fetch_at) FROM user_source_states WHERE user_id=$1`, userID).Scan(&last)
	stage := job.Stage
	for _, item := range stages {
		if item.Status == "running" || item.Status == "queued" || item.Status == "retryable" {
			stage = item.Stage
			break
		}
	}
	return Status{ID: jobID, Stage: stage, Status: job.Status, Progress: workflowProgress(job, children), LastSuccessfulFetchAt: last, Error: safeError(job.LastError), Stages: stages}, nil
}

func (s *Service) GetCurrentStatus(ctx context.Context, userID uuid.UUID) (Status, bool, error) {
	if userID == uuid.Nil {
		return Status{}, false, errors.New("user ID is required")
	}
	job, err := s.store.Queries.FindActiveUserSync(ctx, userID.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, fmt.Errorf("find active synchronization: %w", err)
	}
	status, err := s.GetStatus(ctx, userID, uuid.UUID(job.ID.Bytes))
	if errors.Is(err, pgx.ErrNoRows) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, err
	}
	return status, true, nil
}

func workflowProgress(root dbgen.SyncJob, children []dbgen.SyncJob) int {
	if root.Status == "succeeded" {
		return 100
	}

	var catalogCreated, achievementsCreated bool
	var catalogSucceeded, achievementsSucceeded bool
	var profileCreated, profileSucceeded bool
	var recommendationsCreated, recommendationsSucceeded bool
	aiTotal, aiTerminal := 0, 0
	for _, child := range children {
		switch child.Stage {
		case "catalog":
			catalogCreated = true
			catalogSucceeded = child.Status == "succeeded"
		case "achievements":
			achievementsCreated = true
			achievementsSucceeded = child.Status == "succeeded"
		case "ai_features":
			aiTotal++
			if child.Status == "succeeded" || child.Status == "dead" {
				aiTerminal++
			}
		case "profile":
			profileCreated = true
			profileSucceeded = child.Status == "succeeded"
		case "recommendations":
			recommendationsCreated = true
			recommendationsSucceeded = child.Status == "succeeded"
		}
	}

	progress := 0
	if catalogCreated && achievementsCreated {
		progress += 20
	}
	if catalogSucceeded {
		progress += 20
	}
	if achievementsSucceeded {
		progress += 20
	}
	if profileCreated || recommendationsCreated {
		progress += 20
	} else if aiTotal > 0 {
		// AwaitChildren publishes every planned AI job in one transaction before
		// any child can run, so aiTotal is stable while this fraction advances.
		progress += 20 * aiTerminal / aiTotal
	}
	if profileSucceeded || recommendationsCreated {
		progress += 10
	}
	if recommendationsSucceeded {
		progress += 10
	}
	if progress < 0 {
		return 0
	}
	if progress > 100 {
		return 100
	}
	return progress
}

func safeError(value pgtype.Text) string {
	if !value.Valid || value.String == "" {
		return ""
	}
	return "synchronization stage failed"
}
func uuidPG(value uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: value, Valid: true} }
func timestamptz(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
func text(value string) pgtype.Text { return pgtype.Text{String: value, Valid: value != ""} }
func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}
