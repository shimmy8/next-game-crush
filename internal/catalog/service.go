package catalog

import (
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
	store  *store.Store
	client steam.Client
	seed   Seed
	now    func() time.Time
}

func NewService(database *store.Store, client steam.Client, seed Seed) (*Service, error) {
	if database == nil || client == nil {
		return nil, errors.New("catalog store and Steam client are required")
	}
	if seed.Version != 1 {
		return nil, errors.New("catalog seed schema version 1 is required")
	}
	return &Service{store: database, client: client, seed: seed, now: time.Now}, nil
}

func (s *Service) RefreshKnown(ctx context.Context) error {
	if err := s.importSeed(ctx); err != nil {
		return err
	}
	games, err := s.store.Queries.ListCatalogGames(ctx)
	if err != nil {
		return fmt.Errorf("list catalog games: %w", err)
	}
	for _, game := range games {
		if err := s.refreshOne(ctx, game); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) importSeed(ctx context.Context) error {
	observed, _ := time.Parse(time.DateOnly, s.seed.ObservedDate)
	for _, entry := range s.seed.Entries {
		id := uuid.New()
		seedSnapshot, _ := json.Marshal(entry)
		seedHash := fmt.Sprintf("%x", sha256.Sum256(seedSnapshot))
		if _, err := s.store.Pool.Exec(ctx, `INSERT INTO games (id, steam_app_id, name, source, last_successful_fetch_at, availability, last_attempt_at, source_snapshot, content_hash)
			VALUES ($1,$2,$3,$4,$5,'available',$5,$6,$7)
			ON CONFLICT (steam_app_id) DO UPDATE SET source_snapshot=COALESCE(games.source_snapshot,EXCLUDED.source_snapshot),content_hash=COALESCE(games.content_hash,EXCLUDED.content_hash)`, id, entry.AppID, fmt.Sprintf("Steam App %d", entry.AppID), "catalog_seed", observed, seedSnapshot, seedHash); err != nil {
			return fmt.Errorf("seed catalog AppID %d: %w", entry.AppID, err)
		}
		game, err := s.store.Queries.GetGameByAppID(ctx, entry.AppID)
		if err != nil {
			return fmt.Errorf("read seeded AppID %d: %w", entry.AppID, err)
		}
		if _, err := s.store.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin,added_at) VALUES($1,'seed',$2) ON CONFLICT (game_id) DO NOTHING`, game.ID, observed); err != nil {
			return fmt.Errorf("mark seeded catalog AppID %d: %w", entry.AppID, err)
		}
		var hasObservation bool
		if err := s.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM game_release_dates WHERE game_id=$1)`, game.ID).Scan(&hasObservation); err != nil {
			return fmt.Errorf("inspect seeded release history: %w", err)
		}
		if !hasObservation {
			window := ParseReleaseDate(entry.ReleaseWindow, observed)
			if err := s.observeRelease(ctx, game.ID, window, "catalog_seed:"+entry.SourceURL, observed); err != nil {
				return err
			}
		}
	}
	return nil
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
	updated, err := s.store.Queries.UpsertCatalogGame(ctx, dbgen.UpsertCatalogGameParams{ID: game.ID, SteamAppID: details.AppID, Name: details.Name, Source: "steam_store", LastSuccessfulFetchAt: timestamptz(now), SourceSnapshot: snapshot, ContentHash: text(contentHash)})
	if err != nil {
		return fmt.Errorf("save catalog AppID %d: %w", game.SteamAppID, err)
	}
	if err := s.store.Queries.UpsertCatalogFetchState(ctx, dbgen.UpsertCatalogFetchStateParams{GameID: updated.ID, Availability: string(steam.Available), LastAttemptAt: timestamptz(now), LastSuccessfulFetchAt: timestamptz(now)}); err != nil {
		return fmt.Errorf("record catalog success: %w", err)
	}
	if !details.ReleaseDate.ComingSoon {
		if _, err := s.store.Pool.Exec(ctx, `DELETE FROM catalog_entries WHERE game_id=$1 AND origin='wishlist'`, updated.ID); err != nil {
			return fmt.Errorf("remove released wishlist game from catalog: %w", err)
		}
	}
	window := ParseReleaseDate(details.ReleaseDate.Raw, now)
	if details.ReleaseDate.ComingSoon && window.Precision == PrecisionUnknown {
		window.Precision = PrecisionComingSoon
	}
	return s.observeRelease(ctx, updated.ID, window, "steam_store", now)
}

func (s *Service) observeRelease(ctx context.Context, gameID pgtype.UUID, window ReleaseWindow, source string, observed time.Time) error {
	date := pgtype.Date{}
	if window.Date != nil {
		date = pgtype.Date{Time: *window.Date, Valid: true}
	}
	_, err := s.store.Queries.InsertReleaseObservationIfChanged(ctx, dbgen.InsertReleaseObservationIfChangedParams{TargetGameID: gameID, ID: uuidPG(uuid.New()), Precision: string(window.Precision), ReleaseDate: date, RawText: text(window.Raw), ObservedAt: timestamptz(observed), Source: source, LastSuccessfulFetchAt: timestamptz(observed)})
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
