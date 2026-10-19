package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

type Seed struct {
	Version      int         `json:"version"`
	ObservedDate string      `json:"observed_date"`
	Entries      []SeedEntry `json:"entries"`
}

type SeedEntry struct {
	AppID         int64  `json:"app_id"`
	ReleaseWindow string `json:"release_window"`
	SourceURL     string `json:"source_url"`
	ObservedDate  string `json:"observed_date"`
}

func LoadSeed(path string) (Seed, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Seed{}, fmt.Errorf("read catalog seed: %w", err)
	}
	var seed Seed
	if err := json.Unmarshal(data, &seed); err != nil {
		return Seed{}, fmt.Errorf("decode catalog seed: %w", err)
	}
	if seed.Version != 1 || len(seed.Entries) == 0 {
		return Seed{}, errors.New("catalog seed must use schema version 1 and contain entries")
	}
	if _, err := time.Parse(time.DateOnly, seed.ObservedDate); err != nil {
		return Seed{}, errors.New("catalog seed observed_date must be YYYY-MM-DD")
	}
	seen := map[int64]struct{}{}
	for _, entry := range seed.Entries {
		if entry.AppID <= 0 || strings.TrimSpace(entry.ReleaseWindow) == "" || strings.TrimSpace(entry.SourceURL) == "" || entry.ObservedDate != seed.ObservedDate {
			return Seed{}, errors.New("catalog seed contains an invalid entry")
		}
		if _, duplicate := seen[entry.AppID]; duplicate {
			return Seed{}, fmt.Errorf("catalog seed contains duplicate AppID %d", entry.AppID)
		}
		seen[entry.AppID] = struct{}{}
	}
	return seed, nil
}

type Service struct {
	store      *store.Store
	client     steam.Client
	deckClient steam.Client
	seed       Seed
	now        func() time.Time
}

// RefreshTarget is one independently leasable Store metadata refresh.
type RefreshTarget struct {
	AppID    int64
	Priority int
}

func NewService(database *store.Store, client steam.Client, seed Seed) (*Service, error) {
	if database == nil || client == nil {
		return nil, errors.New("catalog store and Steam client are required")
	}
	if seed.Version != 1 {
		return nil, errors.New("catalog seed schema version 1 is required")
	}
	return &Service{store: database, client: client, deckClient: client, seed: seed, now: time.Now}, nil
}

// SetDeckRefreshClient isolates detached presentation refreshes on the
// catalog transport without changing interactive bulk metadata behavior.
func (s *Service) SetDeckRefreshClient(client steam.Client) {
	if client != nil {
		s.deckClient = client
	}
}

func (s *Service) RefreshKnown(ctx context.Context) error {
	targets, err := s.RefreshTargets(ctx)
	if err != nil {
		return err
	}
	for _, target := range targets {
		if err := s.RefreshGame(ctx, target.AppID); err != nil {
			return err
		}
	}
	return nil
}

// RefreshTargets prepares the local seed and returns the deduplicated known
// catalog union. A target in any user's latest published ranking is foreground
// presentation data and receives the higher refresh priority.
func (s *Service) RefreshTargets(ctx context.Context) ([]RefreshTarget, error) {
	if err := s.importSeed(ctx); err != nil {
		return nil, err
	}
	if err := s.backfillStoredSteamModes(ctx); err != nil {
		return nil, err
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT g.steam_app_id,
		EXISTS(SELECT 1 FROM recommendations r
		  WHERE r.game_id=g.id AND r.ranking_version=(
		    SELECT max(rr.ranking_version) FROM recommendation_runs rr
		    WHERE rr.user_id=r.user_id AND rr.published
		  )) AS displayed,
		EXISTS(SELECT 1 FROM wishlist_entries w WHERE w.game_id=g.id) AS wishlisted
		FROM games g
		WHERE EXISTS(SELECT 1 FROM catalog_entry_sources src WHERE src.game_id=g.id AND src.active)
		   OR EXISTS(SELECT 1 FROM wishlist_entries w WHERE w.game_id=g.id)
		ORDER BY g.steam_app_id`)
	if err != nil {
		return nil, fmt.Errorf("list catalog refresh targets: %w", err)
	}
	defer rows.Close()
	result := []RefreshTarget{}
	for rows.Next() {
		var target RefreshTarget
		var displayed, wishlisted bool
		if err := rows.Scan(&target.AppID, &displayed, &wishlisted); err != nil {
			return nil, err
		}
		target.Priority = 50
		if displayed {
			target.Priority = 80
		}
		if wishlisted {
			target.Priority = 100
		}
		result = append(result, target)
	}
	return result, rows.Err()
}

// RefreshGame refreshes exactly one already-known AppID. It never creates a
// new catalog member; discovery qualification owns admission.
func (s *Service) RefreshGame(ctx context.Context, appID int64) error {
	if appID <= 0 {
		return errors.New("catalog AppID must be positive")
	}
	game, err := s.store.Queries.GetGameByAppID(ctx, appID)
	if err != nil {
		return fmt.Errorf("load catalog AppID %d: %w", appID, err)
	}
	return s.refreshOne(ctx, game)
}

func (s *Service) importSeed(ctx context.Context) error {
	return s.Bootstrap(ctx)
}

func (s *Service) refreshOne(ctx context.Context, game dbgen.Game) error {
	now := s.now().UTC()
	details, err := s.client.GameDetails(ctx, game.SteamAppID)
	if err != nil {
		availability := string(steam.RequestFailed)
		if errors.Is(err, steam.ErrUnavailable) {
			availability = string(steam.Unavailable)
		} else if errors.Is(err, steam.ErrNotModified) {
			availability = string(steam.NotModified)
		}
		safe := "Steam catalog request failed"
		if availability == string(steam.Unavailable) {
			safe = "Steam catalog entry unavailable"
		}
		if _, dbErr := s.store.Pool.Exec(ctx, `UPDATE games SET availability=$2,last_attempt_at=$3,updated_at=now() WHERE id=$1`, game.ID, availability, now); dbErr != nil {
			return fmt.Errorf("mark catalog availability: %w", dbErr)
		}
		lastSuccess := pgtype.Timestamptz{}
		if availability == string(steam.NotModified) {
			lastSuccess = timestamptz(now)
			safe = ""
		}
		if dbErr := s.store.Queries.UpsertCatalogFetchState(ctx, dbgen.UpsertCatalogFetchStateParams{GameID: game.ID, Availability: availability, LastAttemptAt: timestamptz(now), LastSuccessfulFetchAt: lastSuccess, Error: text(safe)}); dbErr != nil {
			return fmt.Errorf("record catalog failure: %w", dbErr)
		}
		return nil
	}
	snapshot, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode catalog snapshot: %w", err)
	}
	contentHash := fmt.Sprintf("%x", sha256.Sum256(snapshot))
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin catalog metadata refresh: %w", err)
	}
	defer tx.Rollback(ctx)
	var lockedGameID pgtype.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM games WHERE id=$1 FOR UPDATE`, game.ID).Scan(&lockedGameID); err != nil {
		return fmt.Errorf("lock catalog AppID %d: %w", game.SteamAppID, err)
	}
	beforeEvidence, candidateBefore, err := catalogRankingEvidence(ctx, tx, lockedGameID)
	if err != nil {
		return fmt.Errorf("read catalog ranking evidence: %w", err)
	}
	q := s.store.Queries.WithTx(tx)
	updated, err := q.UpsertCatalogGame(ctx, dbgen.UpsertCatalogGameParams{ID: game.ID, SteamAppID: details.AppID, Name: details.Name, Source: "steam_store", LastSuccessfulFetchAt: timestamptz(now), SourceSnapshot: snapshot, ContentHash: text(contentHash)})
	if err != nil {
		return fmt.Errorf("save catalog AppID %d: %w", game.SteamAppID, err)
	}
	if err := replaceSteamModes(ctx, q, uuid.UUID(updated.ID.Bytes), details.Categories, contentHash, now); err != nil {
		return err
	}
	if err := q.UpsertCatalogFetchState(ctx, dbgen.UpsertCatalogFetchStateParams{GameID: updated.ID, Availability: string(steam.Available), LastAttemptAt: timestamptz(now), LastSuccessfulFetchAt: timestamptz(now)}); err != nil {
		return fmt.Errorf("record catalog success: %w", err)
	}
	if !details.ReleaseDate.ComingSoon || !strings.EqualFold(strings.TrimSpace(details.Type), "game") {
		if _, err := tx.Exec(ctx, `UPDATE catalog_entry_sources SET active=false WHERE game_id=$1`, updated.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM catalog_entries WHERE game_id=$1`, updated.ID); err != nil {
			return err
		}
	}
	window := ParseReleaseDate(details.ReleaseDate.Raw, now)
	if details.ReleaseDate.ComingSoon && window.Precision == PrecisionUnknown {
		window.Precision = PrecisionComingSoon
	}
	if err := observeReleaseWithQueries(ctx, q, updated.ID, window, "steam_store", now); err != nil {
		return err
	}
	if err := q.UpsertGamePlatforms(ctx, dbgen.UpsertGamePlatformsParams{GameID: updated.ID, Windows: details.Platforms.Windows, Mac: details.Platforms.Mac, Linux: details.Platforms.Linux, PlatformsLastSuccessfulFetchAt: timestamptz(now)}); err != nil {
		return fmt.Errorf("save catalog platforms: %w", err)
	}
	afterEvidence, candidateAfter, err := catalogRankingEvidence(ctx, tx, updated.ID)
	if err != nil {
		return fmt.Errorf("read refreshed catalog ranking evidence: %w", err)
	}
	if (candidateBefore || candidateAfter) && (!bytes.Equal(beforeEvidence, afterEvidence) || candidateBefore != candidateAfter) {
		if err := NewDiscoveryRepository(s.store).MarkCandidateSetDirtyAt(ctx, tx, EligibilityChange{GameID: uuid.UUID(updated.ID.Bytes), Kind: EligibilityChangeSemanticContent}, now); err != nil {
			return fmt.Errorf("mark refreshed catalog candidates changed: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit catalog metadata refresh: %w", err)
	}
	return nil
}

// catalogRankingEvidence returns only canonical fields consumed by ranking.
// Fetch timestamps and availability are deliberately excluded so an identical
// refresh does not create an unbounded rebuild loop.
func catalogRankingEvidence(ctx context.Context, tx pgx.Tx, gameID pgtype.UUID) ([]byte, bool, error) {
	var snapshot, modes, release, platforms []byte
	var candidate bool
	err := tx.QueryRow(ctx, `SELECT COALESCE(g.source_snapshot,'{}'::jsonb),
		COALESCE((SELECT jsonb_agg(gm.mode ORDER BY gm.mode) FROM game_modes gm WHERE gm.game_id=g.id),'[]'::jsonb),
		COALESCE((SELECT jsonb_build_object('precision',r.precision,'date',r.release_date,'raw',r.raw_text)
			FROM game_release_dates r WHERE r.game_id=g.id ORDER BY r.observed_at DESC,r.id DESC LIMIT 1),'null'::jsonb),
		COALESCE((SELECT jsonb_build_object('windows',c.windows,'mac',c.mac,'linux',c.linux)
			FROM game_compatibility c WHERE c.game_id=g.id),'null'::jsonb),
		EXISTS(SELECT 1 FROM catalog_entries ce WHERE ce.game_id=g.id)
		OR EXISTS(SELECT 1 FROM wishlist_entries w WHERE w.game_id=g.id)
	FROM games g WHERE g.id=$1`, gameID).Scan(&snapshot, &modes, &release, &platforms, &candidate)
	if err != nil {
		return nil, false, err
	}
	var details steam.GameDetails
	if err := json.Unmarshal(snapshot, &details); err != nil {
		return nil, false, fmt.Errorf("decode stored catalog snapshot: %w", err)
	}
	evidence, err := json.Marshal(struct {
		MetadataHash string          `json:"metadata_hash"`
		Modes        json.RawMessage `json:"modes"`
		Release      json.RawMessage `json:"release"`
		Platforms    json.RawMessage `json:"platforms"`
	}{steam.GameContentHash(details), modes, release, platforms})
	return evidence, candidate, err
}

type deckCompatibilityObservation struct {
	status       steam.DeckCompatibility
	availability steam.Availability
	err          error
}

func fetchDeckCompatibility(ctx context.Context, client steam.Client, appID int64) deckCompatibilityObservation {
	status, availability, err := client.DeckCompatibility(ctx, appID)
	return deckCompatibilityObservation{status: status, availability: availability, err: err}
}

func recordDeckCompatibility(ctx context.Context, q *dbgen.Queries, gameID pgtype.UUID, observation deckCompatibilityObservation, observedAt time.Time) error {
	now := timestamptz(observedAt)
	validStatus := observation.status == steam.DeckVerified || observation.status == steam.DeckPlayable || observation.status == steam.DeckUnsupported || observation.status == steam.DeckUnknown
	if observation.err == nil && observation.availability == steam.Available && validStatus {
		if err := q.RecordDeckCompatibilitySuccess(ctx, dbgen.RecordDeckCompatibilitySuccessParams{GameID: gameID, DeckStatus: string(observation.status), DeckAvailability: string(steam.Available), DeckLastAttemptAt: now, DeckLastSuccessfulFetchAt: now}); err != nil {
			return fmt.Errorf("record Deck compatibility success: %w", err)
		}
		return nil
	}
	// Keep only fixed availability categories; upstream errors are never persisted.
	availability := observation.availability
	switch availability {
	case steam.Unavailable, steam.NotModified:
	default:
		availability = steam.RequestFailed
	}
	if err := q.RecordDeckCompatibilityFailure(ctx, dbgen.RecordDeckCompatibilityFailureParams{GameID: gameID, DeckAvailability: string(availability), DeckLastAttemptAt: now}); err != nil {
		return fmt.Errorf("record Deck compatibility failure: %w", err)
	}
	return nil
}

func (s *Service) observeRelease(ctx context.Context, gameID pgtype.UUID, window ReleaseWindow, source string, observed time.Time) error {
	return observeReleaseWithQueries(ctx, s.store.Queries, gameID, window, source, observed)
}

func observeReleaseWithQueries(ctx context.Context, q *dbgen.Queries, gameID pgtype.UUID, window ReleaseWindow, source string, observed time.Time) error {
	date := pgtype.Date{}
	if window.Date != nil {
		date = pgtype.Date{Time: *window.Date, Valid: true}
	}
	_, err := q.InsertReleaseObservationIfChanged(ctx, dbgen.InsertReleaseObservationIfChangedParams{TargetGameID: gameID, ID: uuidPG(uuid.New()), Precision: string(window.Precision), ReleaseDate: date, RawText: text(window.Raw), ObservedAt: timestamptz(observed), Source: source, LastSuccessfulFetchAt: timestamptz(observed)})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("record release observation: %w", err)
	}
	return nil
}

func uuidPG(value uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: value, Valid: true} }
func timestamptz(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
func text(value string) pgtype.Text { return pgtype.Text{String: value, Valid: value != ""} }
