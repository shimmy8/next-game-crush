package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

type modeSteamClient struct {
	steam.Client
	details steam.GameDetails
	err     error
}

func (c *modeSteamClient) GameDetails(context.Context, int64) (steam.GameDetails, error) {
	return c.details, c.err
}

func (c *modeSteamClient) DeckCompatibility(context.Context, int64) (steam.DeckCompatibility, steam.Availability, error) {
	return steam.DeckUnknown, steam.Available, nil
}

func modeDetails() steam.GameDetails {
	return steam.GameDetails{AppID: 42, Type: "game", Name: "Co-op title alone is not evidence", ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "2027"}, Categories: []steam.Category{{ID: 9, Description: "Co-op"}}}
}

func modeGame(t *testing.T, s *store.Store, details steam.GameDetails, source string, at time.Time) dbgen.Game {
	t.Helper()
	data, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	game, err := s.Queries.UpsertCatalogGame(context.Background(), dbgen.UpsertCatalogGameParams{ID: uuidPG(uuid.New()), SteamAppID: details.AppID, Name: details.Name, Source: source, LastSuccessfulFetchAt: timestamptz(at), SourceSnapshot: data, ContentHash: text(fmt.Sprintf("%x", sha256.Sum256(data)))})
	if err != nil {
		t.Fatal(err)
	}
	return game
}

func readModes(t *testing.T, s *store.Store, gameID pgtype.UUID) []dbgen.GameMode {
	t.Helper()
	rows, err := s.Queries.ListGameModes(context.Background(), []pgtype.UUID{gameID})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestSteamModesProjectionIsAtomicAndPreservesLastSuccess(t *testing.T) {
	s := openCatalogTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	client := &modeSteamClient{details: modeDetails()}
	svc, err := NewService(s, client, Seed{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return at }
	game := modeGame(t, s, client.details, "catalog_seed", at.Add(-time.Hour))
	if err := svc.refreshOne(ctx, game); err != nil {
		t.Fatal(err)
	}
	first := readModes(t, s, game.ID)
	if len(first) != 1 || first[0].Mode != "co-op" || !first[0].LastSuccessfulFetchAt.Time.Equal(at) {
		t.Fatalf("initial projection = %+v", first)
	}
	at = at.Add(time.Hour)
	client.details.Categories = []steam.Category{{ID: 2, Description: "Single-player"}}
	if err := svc.refreshOne(ctx, game); err != nil {
		t.Fatal(err)
	}
	last := readModes(t, s, game.ID)
	if len(last) != 1 || last[0].Mode != "single-player" || last[0].SourceContentHash == first[0].SourceContentHash || !last[0].LastSuccessfulFetchAt.Time.Equal(at) {
		t.Fatalf("replacement = %+v", last)
	}
	saved, err := s.Queries.GetGameByAppID(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []error{steam.ErrUnavailable, errors.New("malformed Steam response"), errors.New("request failed"), steam.ErrNotModified} {
		client.err = failure
		if err := svc.refreshOne(ctx, game); err != nil {
			t.Fatal(err)
		}
		if got := readModes(t, s, game.ID); !reflect.DeepEqual(got, last) {
			t.Fatalf("failure replaced modes: %+v", got)
		}
	}
	client.err = nil
	client.details.Categories = []steam.Category{{ID: 9, Description: "Co-op"}}
	// Fail after game metadata has been written: all successful projections must roll back.
	if _, err := s.Pool.Exec(ctx, `CREATE FUNCTION reject_mode_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected projection failure'; END $$; CREATE TRIGGER reject_mode_insert BEFORE INSERT ON game_modes FOR EACH ROW EXECUTE FUNCTION reject_mode_insert()`); err != nil {
		t.Fatal(err)
	}
	if err := svc.refreshOne(ctx, game); err == nil {
		t.Fatal("expected projection transaction failure")
	}
	if got := readModes(t, s, game.ID); !reflect.DeepEqual(got, last) {
		t.Fatal("projection did not roll back")
	}
	after, err := s.Queries.GetGameByAppID(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.SourceSnapshot, saved.SourceSnapshot) || after.ContentHash != saved.ContentHash || after.LastSuccessfulFetchAt != saved.LastSuccessfulFetchAt {
		t.Fatal("metadata did not roll back with modes")
	}
	if _, err := s.Pool.Exec(ctx, `DROP TRIGGER reject_mode_insert ON game_modes`); err != nil {
		t.Fatal(err)
	}
	for _, categories := range [][]steam.Category{nil, {{ID: 999, Description: "Unknown mode"}}} {
		client.details.Categories = categories
		if err := svc.refreshOne(ctx, game); err != nil {
			t.Fatal(err)
		}
		if got := readModes(t, s, game.ID); len(got) != 0 {
			t.Fatalf("empty/unknown categories = %+v", got)
		}
	}
}

func TestRefreshGameMarksCanonicalChangesDirtyButNotNoop(t *testing.T) {
	s := openCatalogTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	client := &modeSteamClient{details: modeDetails()}
	client.details.Categories = append(client.details.Categories, steam.Category{ID: 36, Description: "Online PvP"})
	client.details.Developers = []string{"Studio B", "Studio A"}
	svc, err := NewService(s, client, Seed{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return at }
	game := modeGame(t, s, client.details, "catalog_seed", at.Add(-time.Hour))
	userID := uuid.New()
	if _, err := s.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, uuid.New(), userID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setSharedSource(ctx, tx, uuid.UUID(game.ID.Bytes), "seed", true, at); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.RefreshGame(ctx, 42); err != nil {
		t.Fatal(err)
	}
	revision := func() int64 {
		t.Helper()
		var got int64
		if err := s.Pool.QueryRow(ctx, `SELECT dirty_revision FROM catalog_rebuild_state WHERE user_id=$1`, userID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := revision()
	if err := svc.RefreshGame(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if unchanged := revision(); unchanged != first {
		t.Fatalf("no-op dirty revision=%d, want %d", unchanged, first)
	}
	client.details.HeaderImage = "https://cdn.example.invalid/presentation-only.jpg"
	client.details.Categories[0], client.details.Categories[1] = client.details.Categories[1], client.details.Categories[0]
	client.details.Developers[0], client.details.Developers[1] = client.details.Developers[1], client.details.Developers[0]
	if err := svc.RefreshGame(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if presentationOnly := revision(); presentationOnly != first {
		t.Fatalf("presentation-only refresh dirty revision=%d, want %d", presentationOnly, first)
	}
	changes := []func(){
		func() { client.details.Categories = []steam.Category{{ID: 2, Description: "Single-player"}} },
		func() { client.details.Name = "Changed canonical name" },
		func() { client.details.ReleaseDate.Raw = "Q2 2027" },
		func() { client.details.Platforms.Linux = !client.details.Platforms.Linux },
		// Candidate removal must still dirty the ranking for candidateBefore.
		func() { client.details.ReleaseDate.ComingSoon = false },
	}
	for index, change := range changes {
		change()
		at = at.Add(time.Hour)
		if err := svc.RefreshGame(ctx, 42); err != nil {
			t.Fatal(err)
		}
		if got, want := revision(), first+int64(index)+1; got != want {
			t.Fatalf("change %d dirty revision=%d, want %d", index, got, want)
		}
	}
}

func TestRefreshGameRollsBackCanonicalChangesWhenDirtyMarkFails(t *testing.T) {
	s := openCatalogTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	client := &modeSteamClient{details: modeDetails()}
	svc, err := NewService(s, client, Seed{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return at }
	game := modeGame(t, s, client.details, "catalog_seed", at.Add(-time.Hour))
	userID := uuid.New()
	if _, err := s.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version) VALUES($1,$2,1)`, uuid.New(), userID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setSharedSource(ctx, tx, uuid.UUID(game.ID.Bytes), "seed", true, at); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.RefreshGame(ctx, 42); err != nil {
		t.Fatal(err)
	}
	before, err := s.Queries.GetGameByAppID(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := s.Pool.QueryRow(ctx, `SELECT dirty_revision FROM catalog_rebuild_state WHERE user_id=$1`, userID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `CREATE FUNCTION reject_catalog_dirty() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected dirty failure'; END $$; CREATE TRIGGER reject_catalog_dirty BEFORE UPDATE ON catalog_rebuild_state FOR EACH ROW EXECUTE FUNCTION reject_catalog_dirty()`); err != nil {
		t.Fatal(err)
	}
	client.details.Name = "must roll back"
	at = at.Add(time.Hour)
	if err := svc.RefreshGame(ctx, 42); err == nil {
		t.Fatal("expected dirty mark failure")
	}
	after, err := s.Queries.GetGameByAppID(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != before.Name || !reflect.DeepEqual(after.SourceSnapshot, before.SourceSnapshot) || after.ContentHash != before.ContentHash {
		t.Fatal("canonical metadata committed despite dirty mark failure")
	}
	var afterRevision int64
	if err := s.Pool.QueryRow(ctx, `SELECT dirty_revision FROM catalog_rebuild_state WHERE user_id=$1`, userID).Scan(&afterRevision); err != nil {
		t.Fatal(err)
	}
	if afterRevision != revision {
		t.Fatalf("dirty revision=%d after rollback, want %d", afterRevision, revision)
	}
}

func TestSteamModeBackfillUsesOnlyValidatedStoredCategories(t *testing.T) {
	s := openCatalogTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	valid := modeGame(t, s, modeDetails(), "steam_store", at)
	details := modeDetails()
	details.AppID = 43
	seed := modeGame(t, s, details, "catalog_seed", at)
	details.AppID = 44
	invalid := modeGame(t, s, details, "steam_store", at)
	if _, err := s.Pool.Exec(ctx, `UPDATE games SET source_snapshot='{"AppID":44,"Name":"co-op"}' WHERE id=$1`, invalid.ID); err != nil {
		t.Fatal(err)
	}
	details.AppID = 45
	details.Categories = nil
	titleOnly := modeGame(t, s, details, "steam_store", at)
	details.AppID = 46
	details.Categories = []steam.Category{{ID: 1, Description: "Multi-player"}}
	broad := modeGame(t, s, details, "steam_store", at)
	details.AppID = 47
	details.Categories = []steam.Category{{ID: 9, Description: "Co-op"}}
	legacy := modeGame(t, s, details, "steam_store", at)
	legacySnapshot := []byte(`{"AppID":47,"Type":"game","Name":"Co-op title alone is not evidence","RequiredAge":0,"Free":false,"ShortDescription":"","HeaderImage":"","Developers":null,"Publishers":null,"Categories":[{"ID":9,"Description":"Co-op"}],"Genres":null,"ReleaseDate":{"ComingSoon":true,"Raw":"2027"}}`)
	if _, err := s.Pool.Exec(ctx, `UPDATE games SET source_snapshot=$2,content_hash=$3 WHERE id=$1`, legacy.ID, legacySnapshot, fmt.Sprintf("%x", sha256.Sum256(legacySnapshot))); err != nil {
		t.Fatal(err)
	}
	// A failed latest attempt still retains a trusted successful snapshot.
	if _, err := s.Pool.Exec(ctx, `UPDATE games SET availability='request_failed' WHERE id=$1`, valid.ID); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(s, &modeSteamClient{err: steam.ErrUnavailable}, Seed{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RefreshKnown(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readModes(t, s, valid.ID); len(got) != 1 || got[0].Mode != "co-op" || !got[0].LastSuccessfulFetchAt.Time.Equal(at) {
		t.Fatalf("trusted snapshot = %+v", got)
	}
	for _, game := range []dbgen.Game{seed, invalid, titleOnly} {
		if got := readModes(t, s, game.ID); len(got) != 0 {
			t.Fatalf("untrusted evidence created modes: %+v", got)
		}
	}
	if got := readModes(t, s, broad.ID); len(got) != 1 || got[0].Mode != "multiplayer" {
		t.Fatalf("generic multiplayer inferred a subtype: %+v", got)
	}
	if got := readModes(t, s, legacy.ID); len(got) != 1 || got[0].Mode != "co-op" {
		t.Fatalf("legacy successful snapshot was skipped: %+v", got)
	}
	// Replaying does not rewrite rows whose successful snapshot hash is unchanged.
	if _, err := s.Pool.Exec(ctx, `CREATE FUNCTION reject_mode_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'unchanged projection rewritten'; END $$; CREATE TRIGGER reject_mode_delete BEFORE DELETE ON game_modes FOR EACH ROW EXECUTE FUNCTION reject_mode_delete()`); err != nil {
		t.Fatal(err)
	}
	if err := svc.RefreshKnown(ctx); err != nil {
		t.Fatal(err)
	}
}
