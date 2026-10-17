package sync

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
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

const PayloadVersion = 1

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

type Service struct {
	store   *store.Store
	queue   *jobs.Queue
	client  steam.Client
	catalog *catalog.Service
	now     func() time.Time
}

func NewService(database *store.Store, queue *jobs.Queue, client steam.Client, catalogService *catalog.Service) (*Service, error) {
	if database == nil || queue == nil || client == nil || catalogService == nil {
		return nil, errors.New("sync store, queue, Steam client, and catalog service are required")
	}
	return &Service{store: database, queue: queue, client: client, catalog: catalogService, now: time.Now}, nil
}

func (s *Service) Start(ctx context.Context, userID uuid.UUID) (uuid.UUID, error) {
	if userID == uuid.Nil {
		return uuid.Nil, errors.New("user ID is required")
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
		for _, child := range childJobs {
			if child.Required && child.Status != "succeeded" {
				return errors.New("required synchronization stage is incomplete")
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
	owned, ownedAvailability, ownedErr := s.client.OwnedGames(ctx, steamID)
	if err := s.recordState(ctx, userID, "owned_games", "", ownedAvailability, ownedErr); err != nil {
		return err
	}
	if ownedAvailability == steam.Available && ownedErr == nil {
		if err := s.saveOwned(ctx, userID, owned); err != nil {
			return err
		}
		snapshot, _ := json.Marshal(owned)
		if _, err := s.store.Pool.Exec(ctx, `UPDATE steam_accounts SET availability='available',last_successful_fetch_at=$2,raw_snapshot=$3 WHERE user_id=$1`, userID, s.now().UTC(), snapshot); err != nil {
			return fmt.Errorf("save Steam account snapshot: %w", err)
		}
	} else {
		if _, err := s.store.Pool.Exec(ctx, `UPDATE steam_accounts SET availability=$2 WHERE user_id=$1`, userID, availability(ownedAvailability, ownedErr)); err != nil {
			return fmt.Errorf("mark Steam account availability: %w", err)
		}
	}

	recent, recentAvailability, recentErr := s.client.RecentlyPlayed(ctx, steamID)
	if err := s.recordState(ctx, userID, "recently_played", "", recentAvailability, recentErr); err != nil {
		return err
	}
	if recentAvailability == steam.Available && recentErr == nil {
		if err := s.saveRecent(ctx, userID, recent); err != nil {
			return err
		}
	}

	wishlist, wishlistAvailability, wishlistErr := s.client.Wishlist(ctx, steamID)
	if err := s.recordState(ctx, userID, "wishlist", "", wishlistAvailability, wishlistErr); err != nil {
		return err
	}
	if wishlistAvailability == steam.Available && wishlistErr == nil {
		if err := s.saveWishlist(ctx, userID, wishlist); err != nil {
			return err
		}
	}
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
	account, err := s.store.Queries.GetSteamAccountForUser(ctx, uuidPG(userID))
	if err != nil {
		return err
	}
	rows, err := s.store.Queries.ListUserGamesForAchievements(ctx, uuidPG(userID))
	if err != nil {
		return err
	}
	games := make([]UserGame, 0, len(rows))
	for _, row := range rows {
		games = append(games, UserGame{AppID: row.SteamAppID, Name: row.Name, PlaytimeMinutes: row.PlaytimeMinutes, RecentMinutes: row.RecentlyPlayedMinutes, LastPlayedAt: row.LastPlayedAt.Time})
	}
	for _, appID := range SelectAchievementTargets(games, AchievementTargetLimit) {
		summary, state, sourceErr := s.client.Achievements(ctx, account.SteamID, appID)
		if err := s.recordState(ctx, userID, "achievements", strconv.FormatInt(appID, 10), state, sourceErr); err != nil {
			return err
		}
		if state != steam.Available || sourceErr != nil {
			continue
		}
		game, err := s.store.Queries.GetGameByAppID(ctx, appID)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(summary)
		now := s.now().UTC()
		if err := s.store.Queries.UpsertAchievementSummary(ctx, dbgen.UpsertAchievementSummaryParams{UserID: uuidPG(userID), GameID: game.ID, Achieved: int32(summary.Unlocked), Total: int32(summary.Total), Source: "steam", LastSuccessfulFetchAt: timestamptz(now), RawSnapshot: raw}); err != nil {
			return err
		}
	}
	return nil
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
	completed, total := 0, 1+len(children)
	if job.Status == "succeeded" {
		completed++
	}
	for _, child := range children {
		stages = append(stages, StageStatus{Kind: child.Kind, Stage: child.Stage, Status: child.Status, Error: safeError(child.LastError)})
		if child.Status == "succeeded" {
			completed++
		}
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
	return Status{ID: jobID, Stage: stage, Status: job.Status, Progress: completed * 100 / total, LastSuccessfulFetchAt: last, Error: safeError(job.LastError), Stages: stages}, nil
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
