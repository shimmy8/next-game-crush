package completion

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
)

func TestSaveSteamSnapshotAdvancesGenerationAndPreservesEverUnlocked(t *testing.T) {
	db := openCompletionStore(t)
	ctx := context.Background()
	userID, gameID := uuid.New(), uuid.New()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{userID}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at) VALUES($1,1513,'Snapshot','test',now())`, []any{gameID}},
		{`INSERT INTO user_games(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'steam',now())`, []any{userID, gameID}},
	} {
		if _, err := db.Pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := NewSQLRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	first := steam.AchievementSummary{AppID: 1513, GameName: "Snapshot", Achievements: []steam.Achievement{
		{APIName: "ending_b", Name: "Ending B", Description: "Reach an ending", Achieved: true, UnlockedAt: now.Add(-time.Hour)},
		{APIName: "ending_a", Name: "Ending A", Description: "Finish the game", Achieved: true, UnlockedAt: now.Add(-2 * time.Hour)},
	}}
	change1, err := repository.SaveSteamSnapshot(ctx, userID, gameID, first, now)
	if err != nil {
		t.Fatal(err)
	}
	if !change1.DefinitionsChanged || !change1.HundredPercentObserved || change1.DefinitionSetHash == "" {
		t.Fatalf("first change = %+v", change1)
	}
	first.Achievements[0], first.Achievements[1] = first.Achievements[1], first.Achievements[0]
	change2, err := repository.SaveSteamSnapshot(ctx, userID, gameID, first, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if change2.DefinitionSetHash != change1.DefinitionSetHash || change2.DefinitionsChanged {
		t.Fatalf("reordered definition set changed: first=%+v replay=%+v", change1, change2)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE games SET name='Renamed Snapshot' WHERE id=$1`, gameID); err != nil {
		t.Fatal(err)
	}
	change3, err := repository.SaveSteamSnapshot(ctx, userID, gameID, first, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !change3.DefinitionsChanged || change3.DefinitionSetHash == change2.DefinitionSetHash {
		t.Fatalf("game-name-only classifier context change was not hashed: before=%+v after=%+v", change2, change3)
	}
	second := steam.AchievementSummary{AppID: 1513, GameName: "Snapshot", Achievements: []steam.Achievement{
		{APIName: "ending_b", Name: "Ending B", Description: "Reach an ending", Achieved: false},
	}}
	if _, err := repository.SaveSteamSnapshot(ctx, userID, gameID, second, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Pool.Query(ctx, `SELECT api_name,present_in_latest,achieved_in_latest,ever_achieved,snapshot_generation FROM user_achievement_unlocks WHERE user_id=$1 AND game_id=$2 ORDER BY api_name`, userID, gameID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type unlock struct {
		apiName                 string
		present, achieved, ever bool
		generation              int64
	}
	var got []unlock
	for rows.Next() {
		var row unlock
		if err := rows.Scan(&row.apiName, &row.present, &row.achieved, &row.ever, &row.generation); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []unlock{{apiName: "ending_a", present: false, achieved: false, ever: true, generation: 3}, {apiName: "ending_b", present: true, achieved: false, ever: true, generation: 4}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("unlocks = %+v, want %+v", got, want)
	}
	var generation int64
	var achieved, total int
	var raw string
	if err := db.Pool.QueryRow(ctx, `SELECT snapshot_generation,achieved,total,raw_snapshot::text FROM achievement_summaries WHERE user_id=$1 AND game_id=$2`, userID, gameID).Scan(&generation, &achieved, &total, &raw); err != nil {
		t.Fatal(err)
	}
	if generation != 4 || achieved != 0 || total != 1 || strings.Contains(raw, "ending_a") {
		t.Fatalf("aggregate generation=%d achieved=%d total=%d raw=%s", generation, achieved, total, raw)
	}
	var definitions, evidence int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM game_achievement_definitions WHERE game_id=$1`, gameID).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM user_game_completion_evidence WHERE user_id=$1 AND game_id=$2 AND kind='hundred_percent'`, userID, gameID).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if definitions != 2 || evidence != 1 {
		t.Fatalf("definitions=%d evidence=%d, want 2/1", definitions, evidence)
	}
	if _, err := db.Pool.Exec(ctx, `CREATE FUNCTION reject_snapshot_unlock() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.api_name='z_break' THEN RAISE EXCEPTION 'injected snapshot failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_snapshot_unlock BEFORE INSERT OR UPDATE ON user_achievement_unlocks FOR EACH ROW EXECUTE FUNCTION reject_snapshot_unlock()`); err != nil {
		t.Fatal(err)
	}
	broken := steam.AchievementSummary{AppID: 1513, Achievements: []steam.Achievement{
		{APIName: "a_new", Name: "New", Description: "New definition"},
		{APIName: "z_break", Name: "Break", Description: "Trigger rollback"},
	}}
	if _, err := repository.SaveSteamSnapshot(ctx, userID, gameID, broken, now.Add(4*time.Minute)); err == nil {
		t.Fatal("injected mid-snapshot failure was accepted")
	}
	var generationAfterFailure, newDefinitions int
	if err := db.Pool.QueryRow(ctx, `SELECT snapshot_generation FROM achievement_summaries WHERE user_id=$1 AND game_id=$2`, userID, gameID).Scan(&generationAfterFailure); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM game_achievement_definitions WHERE game_id=$1 AND api_name IN ('a_new','z_break')`, gameID).Scan(&newDefinitions); err != nil {
		t.Fatal(err)
	}
	if generationAfterFailure != 4 || newDefinitions != 0 {
		t.Fatalf("failed transaction leaked generation=%d new_definitions=%d", generationAfterFailure, newDefinitions)
	}
}

func openCompletionStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	conn.Close(ctx)
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	db, err := store.Open(context.Background(), fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", baseURL, separator, schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		cleanup, cleanupErr := pgx.Connect(cleanupCtx, baseURL)
		if cleanupErr == nil {
			_, _ = cleanup.Exec(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(cleanupCtx)
		}
	})
	return db
}
