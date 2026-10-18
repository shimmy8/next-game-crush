package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/steam"
	dbgen "next-game-crush/internal/store/sqlc"
)

// The caller owns the metadata transaction, so a failed projection cannot
// publish a snapshot whose canonical categories were not saved.
func replaceSteamModes(ctx context.Context, q *dbgen.Queries, gameID uuid.UUID, categories []steam.Category, sourceContentHash string, fetchedAt time.Time) error {
	if gameID == uuid.Nil || strings.TrimSpace(sourceContentHash) == "" || fetchedAt.IsZero() {
		return errors.New("successful Steam mode source is required")
	}
	id := uuidPG(gameID)
	if err := q.DeleteSteamGameModes(ctx, id); err != nil {
		return fmt.Errorf("replace Steam modes: %w", err)
	}
	for _, mode := range steam.CanonicalModes(categories) {
		if err := q.InsertSteamGameMode(ctx, dbgen.InsertSteamGameModeParams{GameID: id, Mode: string(mode), SourceContentHash: sourceContentHash, LastSuccessfulFetchAt: timestamptz(fetchedAt)}); err != nil {
			return fmt.Errorf("save Steam mode: %w", err)
		}
	}
	return nil
}

func (s *Service) backfillStoredSteamModes(ctx context.Context) error {
	rows, err := s.store.Queries.ListStoredSteamCategorySnapshots(ctx)
	if err != nil {
		return fmt.Errorf("list stored Steam category snapshots: %w", err)
	}
	for _, row := range rows {
		if err := s.backfillStoredSteamGameModes(ctx, row); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) backfillStoredSteamGameModes(ctx context.Context, row dbgen.ListStoredSteamCategorySnapshotsRow) error {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin Steam mode backfill: %w", err)
	}
	defer tx.Rollback(ctx)
	// Serialize with successful metadata writers, then reread their current
	// snapshot. An older list result must never replace a newer projection.
	if _, err := tx.Exec(ctx, `SELECT id FROM games WHERE id=$1 FOR UPDATE`, row.GameID); err != nil {
		return fmt.Errorf("lock Steam mode source: %w", err)
	}
	q := s.store.Queries.WithTx(tx)
	game, err := q.GetGameByAppID(ctx, row.SteamAppID)
	if err != nil {
		return fmt.Errorf("read Steam mode source: %w", err)
	}
	details, ok := validatedStoredSteamDetails(game)
	if !ok {
		return nil
	}
	modes, err := q.ListGameModes(ctx, []pgtype.UUID{game.ID})
	if err != nil {
		return fmt.Errorf("read stored Steam modes: %w", err)
	}
	unchanged := len(modes) > 0
	for _, mode := range modes {
		unchanged = unchanged && mode.SourceContentHash == game.ContentHash.String
	}
	if unchanged {
		return nil
	}
	if err := replaceSteamModes(ctx, q, uuid.UUID(game.ID.Bytes), details.Categories, game.ContentHash.String, game.LastSuccessfulFetchAt.Time); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Steam mode backfill: %w", err)
	}
	return nil
}

// Only a complete normalized snapshot produced by our successful Steam writer
// is trusted. Re-encoding its typed value reproduces the original content hash
// despite PostgreSQL JSONB key ordering, and rejects partial/foreign payloads.
func validatedStoredSteamDetails(game dbgen.Game) (steam.GameDetails, bool) {
	var details steam.GameDetails
	if game.Source != "steam_store" || !game.ContentHash.Valid || !game.LastSuccessfulFetchAt.Valid || game.LastSuccessfulFetchAt.Time.IsZero() {
		return details, false
	}
	decoder := json.NewDecoder(bytes.NewReader(game.SourceSnapshot))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&details); err != nil {
		return details, false
	}
	if details.AppID <= 0 || details.AppID != game.SteamAppID || strings.TrimSpace(details.Type) == "" || strings.TrimSpace(details.Name) == "" || (strings.TrimSpace(details.ReleaseDate.Raw) == "" && !details.ReleaseDate.ComingSoon) {
		return details, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(game.SourceSnapshot, &fields); err != nil {
		return details, false
	}
	encoded, err := json.Marshal(details)
	if _, hasPlatforms := fields["Platforms"]; !hasPlatforms {
		// Platforms was added after the original normalized snapshot schema.
		// Shadow the promoted field to reproduce that successful writer's hash.
		encoded, err = json.Marshal(struct {
			steam.GameDetails
			Platforms *steam.Platforms `json:"Platforms,omitempty"`
		}{GameDetails: details})
	}
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(encoded)) != game.ContentHash.String {
		return details, false
	}
	return details, true
}
