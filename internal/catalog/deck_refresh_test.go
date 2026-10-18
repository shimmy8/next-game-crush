package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
)

func TestDeckRefreshEnqueueIdempotent(t *testing.T) {
	db := openCatalogTestStore(t)
	client := &discoveryMetadata{details: modeDetails(), deckStatus: steam.DeckVerified, deckAvailability: steam.Available}
	game := modeGame(t, db, client.details, "test", time.Now())
	service, err := NewService(db, client, Seed{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	target := DeckRefreshTarget{GameID: uuid.UUID(game.ID.Bytes), SteamAppID: game.SteamAppID, RankingVersion: 7}
	for range 2 {
		if err := service.EnqueueDeckRefresh(context.Background(), target); err != nil {
			t.Fatal(err)
		}
	}
	var count, attempts int
	var lane, key, kind string
	var raw []byte
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM sync_jobs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(context.Background(), `SELECT lane,idempotency_key,kind,payload,max_attempts FROM sync_jobs`).Scan(&lane, &key, &kind, &raw, &attempts); err != nil {
		t.Fatal(err)
	}
	wantKey := fmt.Sprintf("catalog.deck_refresh:v1:%s:ranking:7", target.GameID)
	var payload DeckRefreshPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if count != 1 || lane != string(jobs.LaneCatalog) || key != wantKey || kind != DeckRefreshJobKind || attempts != 4 || payload.Version != 1 || payload.Target != target || client.deckCalls != 0 {
		t.Fatalf("job count=%d lane=%s key=%s kind=%s attempts=%d payload=%+v calls=%d", count, lane, key, kind, attempts, payload, client.deckCalls)
	}
}

func TestCatalogRefreshDoesNotFetchDeckCompatibility(t *testing.T) {
	db := openCatalogTestStore(t)
	client := &discoveryMetadata{details: modeDetails(), deckStatus: steam.DeckVerified, deckAvailability: steam.Available}
	modeGame(t, db, client.details, "test", time.Now())
	service, err := NewService(db, client, Seed{Version: 1, Entries: []SeedEntry{{AppID: 42, ReleaseWindow: "Coming Soon", SourceURL: "https://store.steampowered.com/app/42/", ObservedDate: "2026-10-18"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RefreshTargets(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.RefreshGame(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if err := service.RefreshKnown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.deckCalls != 0 {
		t.Fatalf("ordinary refresh made %d Deck calls", client.deckCalls)
	}
}

func TestDeckRefreshFailurePreservesLastSuccess(t *testing.T) {
	for _, test := range []struct {
		name         string
		status       steam.DeckCompatibility
		availability steam.Availability
		err          error
		want         steam.Availability
	}{
		{"unavailable", steam.DeckUnsupported, steam.Unavailable, steam.ErrUnavailable, steam.Unavailable},
		{"malformed", "future", steam.Available, nil, steam.RequestFailed},
		{"failed", steam.DeckUnknown, steam.RequestFailed, errors.New("private upstream detail"), steam.RequestFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openCatalogTestStore(t)
			now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
			client := &discoveryMetadata{details: modeDetails(), deckStatus: steam.DeckVerified, deckAvailability: steam.Available}
			game := modeGame(t, db, client.details, "test", now)
			service, err := NewService(db, client, Seed{Version: 1})
			if err != nil {
				t.Fatal(err)
			}
			service.now = func() time.Time { return now }
			target := DeckRefreshTarget{GameID: uuid.UUID(game.ID.Bytes), SteamAppID: game.SteamAppID, RankingVersion: 1}
			payload, _ := json.Marshal(DeckRefreshPayload{Version: 1, Target: target})
			lease := jobs.Lease{Lane: jobs.LaneCatalog, Payload: payload}
			if err := service.handleDeckRefresh(context.Background(), lease); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Hour)
			client.deckStatus, client.deckAvailability, client.deckErr = test.status, test.availability, test.err
			if err := service.handleDeckRefresh(context.Background(), lease); err != nil {
				t.Fatal(err)
			}
			var status, availability string
			var attempt, success time.Time
			if err := db.Pool.QueryRow(context.Background(), `SELECT deck_status,deck_availability,deck_last_attempt_at,deck_last_successful_fetch_at FROM game_compatibility WHERE game_id=$1`, game.ID).Scan(&status, &availability, &attempt, &success); err != nil {
				t.Fatal(err)
			}
			if status != string(steam.DeckVerified) || availability != string(test.want) || !attempt.Equal(now) || !success.Equal(now.Add(-time.Hour)) || client.deckCalls != 2 {
				t.Fatalf("state=%s,%s attempt=%v success=%v calls=%d", status, availability, attempt, success, client.deckCalls)
			}
		})
	}
}

func TestDeckRefreshWithoutPlatformsPersistsObservation(t *testing.T) {
	for _, available := range []bool{true, false} {
		t.Run(fmt.Sprintf("success=%v", available), func(t *testing.T) {
			db := openCatalogTestStore(t)
			ctx := context.Background()
			now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
			client := &discoveryMetadata{details: modeDetails(), deckStatus: steam.DeckVerified, deckAvailability: steam.Available}
			if !available {
				client.deckAvailability, client.deckErr = steam.Unavailable, steam.ErrUnavailable
			}
			game := modeGame(t, db, client.details, "test", now)
			service, err := NewService(db, client, Seed{Version: 1})
			if err != nil {
				t.Fatal(err)
			}
			service.now = func() time.Time { return now }
			payload, _ := json.Marshal(DeckRefreshPayload{Version: 1, Target: DeckRefreshTarget{GameID: uuid.UUID(game.ID.Bytes), SteamAppID: game.SteamAppID, RankingVersion: 1}})
			if err := service.handleDeckRefresh(ctx, jobs.Lease{Lane: jobs.LaneCatalog, Payload: payload}); err != nil {
				t.Fatal(err)
			}
			var windows, mac, linux pgtype.Bool
			var platformAt, attemptAt, successAt pgtype.Timestamptz
			var status, availability string
			if err := db.Pool.QueryRow(ctx, `SELECT windows,mac,linux,platforms_last_successful_fetch_at,deck_status,deck_availability,deck_last_attempt_at,deck_last_successful_fetch_at FROM game_compatibility WHERE game_id=$1`, game.ID).Scan(&windows, &mac, &linux, &platformAt, &status, &availability, &attemptAt, &successAt); err != nil {
				t.Fatal(err)
			}
			if windows.Valid || mac.Valid || linux.Valid || platformAt.Valid {
				t.Fatalf("Deck invented platform observation: %v %v %v %v", windows, mac, linux, platformAt)
			}
			if !attemptAt.Valid || !attemptAt.Time.Equal(now) || successAt.Valid != available || availability != string(client.deckAvailability) {
				t.Fatalf("Deck availability=%s attempt=%v success=%v", availability, attemptAt, successAt)
			}
			if available && (status != string(steam.DeckVerified) || !successAt.Time.Equal(now)) {
				t.Fatalf("Deck success=%s %v", status, successAt)
			}
			if !available && status != string(steam.DeckUnknown) {
				t.Fatalf("failed observation invented status %s", status)
			}
			// A later canonical platform observation must fill only its own fields.
			if err := service.RefreshGame(ctx, game.SteamAppID); err != nil {
				t.Fatal(err)
			}
			var keptStatus, keptAvailability string
			if err := db.Pool.QueryRow(ctx, `SELECT windows,platforms_last_successful_fetch_at,deck_status,deck_availability FROM game_compatibility WHERE game_id=$1`, game.ID).Scan(&windows, &platformAt, &keptStatus, &keptAvailability); err != nil {
				t.Fatal(err)
			}
			if !windows.Valid || !platformAt.Valid || keptStatus != status || keptAvailability != availability {
				t.Fatal("platform refresh did not preserve independent Deck state")
			}
		})
	}
}
