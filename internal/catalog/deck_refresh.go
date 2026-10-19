package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/jobs"
)

const DeckRefreshJobKind = "catalog.deck_refresh"

// DeckRefreshTarget identifies a game selected by an immutable published run.
// Recommendation publication owns selection; catalog owns the detached refresh.
type DeckRefreshTarget struct {
	GameID         uuid.UUID `json:"game_id"`
	SteamAppID     int64     `json:"steam_app_id"`
	RankingVersion int64     `json:"ranking_version"`
}

type DeckRefreshPayload struct {
	Version int               `json:"version"`
	Target  DeckRefreshTarget `json:"target"`
}

func (target DeckRefreshTarget) valid() bool {
	return target.GameID != uuid.Nil && target.SteamAppID > 0 && target.RankingVersion > 0
}

// EnqueueDeckRefresh is idempotent for each published game/version, including
// a replay after only part of a ranking's targets reached the queue.
func (s *Service) EnqueueDeckRefresh(ctx context.Context, target DeckRefreshTarget) error {
	if !target.valid() {
		return errors.New("invalid published Deck refresh target")
	}
	payload, err := json.Marshal(DeckRefreshPayload{Version: 1, Target: target})
	if err != nil {
		return fmt.Errorf("encode Deck refresh payload: %w", err)
	}
	_, err = jobs.NewQueue(s.store).Enqueue(ctx, jobs.Spec{
		Kind: DeckRefreshJobKind, Stage: "catalog_deck_refresh", Lane: jobs.LaneCatalog,
		IdempotencyKey: fmt.Sprintf("catalog.deck_refresh:v1:%s:ranking:%d", target.GameID, target.RankingVersion),
		Payload:        payload, MaxAttempts: 4, Priority: 80, Optional: true,
	})
	return err
}

// RegisterDeckRefreshHandlers is activated with the catalog worker at cutover.
func (s *Service) RegisterDeckRefreshHandlers(worker *jobs.Worker) error {
	if worker == nil {
		return errors.New("Deck refresh worker is required")
	}
	return worker.Register(DeckRefreshJobKind, s.handleDeckRefresh)
}

func (s *Service) handleDeckRefresh(ctx context.Context, lease jobs.Lease) error {
	var payload DeckRefreshPayload
	if lease.Lane != jobs.LaneCatalog || strictDiscoveryPayload(lease.Payload, &payload) != nil || payload.Version != 1 || !payload.Target.valid() {
		return errors.New("unsupported Deck refresh payload schema")
	}
	game, err := s.store.Queries.GetGameByAppID(ctx, payload.Target.SteamAppID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load Deck refresh game: %w", err)
	}
	if uuid.UUID(game.ID.Bytes) != payload.Target.GameID {
		return errors.New("Deck refresh game identity mismatch")
	}
	observation := fetchDeckCompatibility(ctx, s.deckClient, payload.Target.SteamAppID)
	return recordDeckCompatibility(ctx, s.store.Queries, game.ID, observation, s.now().UTC())
}
