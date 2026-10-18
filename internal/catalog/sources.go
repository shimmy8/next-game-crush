package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Bootstrap imports reviewed seed metadata without making upstream requests.
func (s *Service) Bootstrap(ctx context.Context) error {
	observed, _ := time.Parse(time.DateOnly, s.seed.ObservedDate)
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.store.Queries.WithTx(tx)
	for _, entry := range s.seed.Entries {
		snapshot, _ := json.Marshal(entry)
		hash := fmt.Sprintf("%x", sha256.Sum256(snapshot))
		if _, err := tx.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at,availability,last_attempt_at,source_snapshot,content_hash)
   VALUES($1,$2,$3,'catalog_seed',$4,'available',$4,$5,$6)
   ON CONFLICT(steam_app_id) DO UPDATE SET source_snapshot=COALESCE(games.source_snapshot,EXCLUDED.source_snapshot),content_hash=COALESCE(games.content_hash,EXCLUDED.content_hash)`, uuid.New(), entry.AppID, fmt.Sprintf("Steam App %d", entry.AppID), observed, snapshot, hash); err != nil {
			return fmt.Errorf("import catalog seed: %w", err)
		}
		game, err := q.GetGameByAppID(ctx, entry.AppID)
		if err != nil {
			return err
		}
		if _, err := setSharedSource(ctx, tx, uuid.UUID(game.ID.Bytes), "seed", !snapshotIneligible(game.SourceSnapshot, game.SteamAppID), observed); err != nil {
			return err
		}
		var hasObservation bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM game_release_dates WHERE game_id=$1)`, game.ID).Scan(&hasObservation); err != nil {
			return err
		}
		if !hasObservation {
			if err := observeReleaseWithQueries(ctx, q, game.ID, ParseReleaseDate(entry.ReleaseWindow, observed), "catalog_seed:"+entry.SourceURL, observed); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// CleanupLegacyWishlistProjection is safe only with the independent wishlist union installed.
func (s *Service) CleanupLegacyWishlistProjection(ctx context.Context) error {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM catalog_entries ce WHERE origin='wishlist'
  AND NOT EXISTS(SELECT 1 FROM catalog_entry_sources src WHERE src.game_id=ce.game_id AND src.active)`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// setSharedSource serializes provenance and projection updates on the game row.
// Historical source rows remain even when their active membership is removed.
func setSharedSource(ctx context.Context, tx pgx.Tx, gameID uuid.UUID, source string, active bool, seenAt time.Time) (bool, error) {
	if source != "seed" && source != "steam_discovery" {
		return false, errors.New("invalid shared catalog source")
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM games WHERE id=$1 FOR UPDATE`, gameID).Scan(&id); err != nil {
		return false, err
	}
	var before bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_entries WHERE game_id=$1)`, gameID).Scan(&before); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO catalog_entry_sources(game_id,source,first_seen_at,last_seen_at,active) VALUES($1,$2,$3,$3,$4)
  ON CONFLICT(game_id,source) DO UPDATE SET last_seen_at=GREATEST(catalog_entry_sources.last_seen_at,EXCLUDED.last_seen_at),active=EXCLUDED.active`, gameID, source, seenAt, active); err != nil {
		return false, err
	}
	var after bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_entry_sources WHERE game_id=$1 AND active)`, gameID).Scan(&after); err != nil {
		return false, err
	}
	if after {
		_, err := tx.Exec(ctx, `INSERT INTO catalog_entries(game_id,added_at) VALUES($1,$2) ON CONFLICT(game_id) DO UPDATE SET origin=NULL`, gameID, seenAt)
		if err != nil {
			return false, err
		}
	} else {
		if _, err := tx.Exec(ctx, `DELETE FROM catalog_entries WHERE game_id=$1`, gameID); err != nil {
			return false, err
		}
	}
	return before != after, nil
}

func snapshotIneligible(snapshot []byte, appID int64) bool {
	var details struct {
		AppID       int64
		Type        string
		Name        string
		ReleaseDate struct {
			ComingSoon *bool
			Raw        *string
		}
	}
	if json.Unmarshal(snapshot, &details) != nil || details.AppID != appID || strings.TrimSpace(details.Name) == "" || strings.TrimSpace(details.Type) == "" || details.ReleaseDate.ComingSoon == nil || details.ReleaseDate.Raw == nil {
		return false
	}
	if !*details.ReleaseDate.ComingSoon && strings.TrimSpace(*details.ReleaseDate.Raw) == "" {
		return false
	}
	return !*details.ReleaseDate.ComingSoon || !strings.EqualFold(strings.TrimSpace(details.Type), "game")
}
