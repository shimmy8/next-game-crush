package sync

import (
	"context"
	"testing"
	"time"

	"next-game-crush/internal/completion"
	"next-game-crush/internal/steam"
)

type snapshotSteam struct {
	*fakeSteam
	summary     steam.AchievementSummary
	playerState steam.Availability
	playerErr   error
	schema      steam.AchievementSchema
	schemaState steam.Availability
	schemaErr   error
}

func (f *snapshotSteam) Achievements(context.Context, string, int64) (steam.AchievementSummary, steam.Availability, error) {
	return f.summary, f.playerState, f.playerErr
}

func (f *snapshotSteam) AchievementSchema(context.Context, int64) (steam.AchievementSchema, steam.Availability, error) {
	return f.schema, f.schemaState, f.schemaErr
}

func TestAchievementSyncSchemaFallbackFailurePreservesPlayerSnapshotAndDefinitions(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	if err := service.importSteam(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	repository, err := completion.NewSQLRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	service.SetAchievementSnapshotRecorder(repository)
	client := &snapshotSteam{fakeSteam: fake, playerState: steam.Available, schemaState: steam.Available,
		summary: steam.AchievementSummary{AppID: 10, GameName: "Owned", Achievements: []steam.Achievement{{APIName: "finish", Name: "The End", Description: "Finish the game"}}}}
	service.client = client
	if err := service.syncAchievements(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	client.summary = steam.AchievementSummary{AppID: 10, GameName: "Owned", Achievements: []steam.Achievement{{APIName: "finish"}}}
	client.schema = steam.AchievementSchema{AppID: 10, Achievements: []steam.AchievementDefinition{{APIName: "finish", Name: "Schema name"}}}
	if err := service.syncAchievements(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var beforeName, beforeDescription, beforeHash, metadataSource string
	if err := db.Pool.QueryRow(context.Background(), `SELECT display_name,description,definition_content_hash,metadata_source FROM game_achievement_definitions d JOIN games g ON g.id=d.game_id WHERE g.steam_app_id=10 AND d.api_name='finish'`).Scan(&beforeName, &beforeDescription, &beforeHash, &metadataSource); err != nil {
		t.Fatal(err)
	}
	if beforeName != "Schema name" || beforeDescription != "Finish the game" || metadataSource != "game_schema" {
		t.Fatalf("schema fallback provenance = %q/%q/%q", beforeName, beforeDescription, metadataSource)
	}
	client.summary = steam.AchievementSummary{AppID: 10, GameName: "Owned", Achievements: []steam.Achievement{{APIName: "finish", Name: "Schema name", Achieved: true, UnlockedAt: time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)}}}
	client.schemaState, client.schemaErr = steam.RequestFailed, steam.ErrInvalidResponse
	if err := service.syncAchievements(context.Background(), userID); err != nil {
		t.Fatalf("schema fallback failure aborted player snapshot: %v", err)
	}
	var afterName, afterDescription, afterHash string
	if err := db.Pool.QueryRow(context.Background(), `SELECT display_name,description,definition_content_hash FROM game_achievement_definitions d JOIN games g ON g.id=d.game_id WHERE g.steam_app_id=10 AND d.api_name='finish'`).Scan(&afterName, &afterDescription, &afterHash); err != nil {
		t.Fatal(err)
	}
	if afterName != beforeName || afterDescription != beforeDescription || afterHash != beforeHash {
		t.Fatalf("definition overwritten after fallback failure: before=%q/%q/%q after=%q/%q/%q", beforeName, beforeDescription, beforeHash, afterName, afterDescription, afterHash)
	}
	var achieved, total int
	var current, ever bool
	if err := db.Pool.QueryRow(context.Background(), `SELECT s.achieved,s.total,u.achieved_in_latest,u.ever_achieved FROM achievement_summaries s JOIN user_achievement_unlocks u USING(user_id,game_id) JOIN games g ON g.id=s.game_id WHERE s.user_id=$1 AND g.steam_app_id=10 AND u.api_name='finish'`, userID).Scan(&achieved, &total, &current, &ever); err != nil {
		t.Fatal(err)
	}
	if achieved != 1 || total != 1 || !current || !ever {
		t.Fatalf("player snapshot not committed: achieved=%d total=%d current=%v ever=%v", achieved, total, current, ever)
	}
	states := map[string]string{}
	rows, err := db.Pool.Query(context.Background(), `SELECT source,availability FROM user_source_states WHERE user_id=$1 AND resource_key='10' AND source IN ('achievements','achievement_schema')`, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var source, state string
		if err := rows.Scan(&source, &state); err != nil {
			t.Fatal(err)
		}
		states[source] = state
	}
	if states["achievements"] != string(steam.Available) || states["achievement_schema"] != string(steam.RequestFailed) {
		t.Fatalf("independent source states = %#v", states)
	}
}

func TestAchievementSyncUnavailablePreservesDetailedSnapshot(t *testing.T) {
	tests := []struct {
		name  string
		state steam.Availability
		err   error
	}{
		{name: "private", state: steam.Unavailable},
		{name: "request failed", state: steam.RequestFailed, err: steam.ErrInvalidResponse},
		{name: "not modified", state: steam.NotModified, err: steam.ErrNotModified},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, fake, db, userID := newTestService(t)
			if err := service.importSteam(context.Background(), userID); err != nil {
				t.Fatal(err)
			}
			repository, err := completion.NewSQLRepository(db)
			if err != nil {
				t.Fatal(err)
			}
			service.SetAchievementSnapshotRecorder(repository)
			client := &snapshotSteam{fakeSteam: fake, playerState: steam.Available, schemaState: steam.Available,
				summary: steam.AchievementSummary{AppID: 10, GameName: "Owned", Achievements: []steam.Achievement{{APIName: "finish", Name: "The End", Description: "Finish the game", Achieved: true}}}}
			service.client = client
			if err := service.syncAchievements(context.Background(), userID); err != nil {
				t.Fatal(err)
			}
			readSnapshot := func() string {
				t.Helper()
				var snapshot string
				if err := db.Pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
					'summary',(SELECT to_jsonb(s) FROM achievement_summaries s JOIN games g ON g.id=s.game_id WHERE s.user_id=$1 AND g.steam_app_id=10),
					'definitions',(SELECT jsonb_agg(to_jsonb(d) ORDER BY d.api_name) FROM game_achievement_definitions d JOIN games g ON g.id=d.game_id WHERE g.steam_app_id=10),
					'unlocks',(SELECT jsonb_agg(to_jsonb(u) ORDER BY u.api_name) FROM user_achievement_unlocks u JOIN games g ON g.id=u.game_id WHERE u.user_id=$1 AND g.steam_app_id=10),
					'evidence',(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM user_game_completion_evidence e JOIN games g ON g.id=e.game_id WHERE e.user_id=$1 AND g.steam_app_id=10)
				)::text`, userID).Scan(&snapshot); err != nil {
					t.Fatal(err)
				}
				return snapshot
			}
			before := readSnapshot()
			client.summary = steam.AchievementSummary{AppID: 10, Achievements: []steam.Achievement{{APIName: "other", Name: "Other", Description: "Other"}}}
			client.playerState, client.playerErr = test.state, test.err
			if err := service.syncAchievements(context.Background(), userID); err != nil {
				t.Fatal(err)
			}
			if after := readSnapshot(); after != before {
				t.Fatalf("unavailable player response changed detailed snapshot:\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}
