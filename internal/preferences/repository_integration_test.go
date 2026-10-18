package preferences

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

func openPreferenceStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "preferences_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	admin.Close(ctx)
	t.Cleanup(func() {
		admin, err := pgx.Connect(context.Background(), baseURL)
		if err == nil {
			defer admin.Close(context.Background())
			_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		}
	})
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	db, err := store.Open(ctx, fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", baseURL, separator, schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func preferenceFixture(t *testing.T, db *store.Store) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	user, game, profile, recommendation, feedback := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id)VALUES($1)`, []any{user}},
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at)VALUES($1,$2,'Fixture','catalog_seed',now())`, []any{game, time.Now().UnixNano()}},
		{`INSERT INTO preference_profiles(id,user_id,version)VALUES($1,$2,1)`, []any{profile, user}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,score)VALUES($1,$2,$3,$4,'fixture',15)`, []any{recommendation, user, game, profile}},
		{`INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key)VALUES($1,$2,$3,'dislike',$4)`, []any{feedback, recommendation, user, feedback.String()}},
	} {
		if _, err := db.Pool.Exec(context.Background(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return user, game, feedback
}

func TestAdjustmentConstraintsAndScope(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user, game, feedback := preferenceFixture(t, db)
	other, otherGame, otherFeedback := preferenceFixture(t, db)
	insert := `INSERT INTO preference_adjustments(id,user_id,source_feedback_id,source_game_id,dimension,target,direction,confidence,strength) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`
	for _, tt := range []struct {
		name, dimension, target, direction string
		confidence, strength               float64
	}{
		{"dimension", "genre", "co-op", "avoid", .9, 1},
		{"target", "mode", "invented", "avoid", .9, 1},
		{"direction", "mode", "co-op", "prefer", .9, 1},
		{"negative-confidence", "mode", "co-op", "avoid", -.1, 1},
		{"excess-confidence", "mode", "co-op", "avoid", 1.1, 1},
		{"negative-strength", "mode", "co-op", "avoid", .9, -.1},
		{"excess-strength", "mode", "co-op", "avoid", .9, 1.1},
		{"nan-confidence", "mode", "co-op", "avoid", math.NaN(), 1},
		{"infinite-confidence", "mode", "co-op", "avoid", math.Inf(1), 1},
		{"nan-strength", "mode", "co-op", "avoid", .9, math.NaN()},
		{"infinite-strength", "mode", "co-op", "avoid", .9, math.Inf(1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := db.Pool.Exec(ctx, insert, uuid.New(), user, feedback, game, tt.dimension, tt.target, tt.direction, tt.confidence, tt.strength); err == nil {
				t.Fatal("invalid adjustment accepted")
			}
		})
	}
	if _, err := db.Pool.Exec(ctx, insert, uuid.New(), other, feedback, game, "mode", "co-op", "avoid", .9, 1); err == nil {
		t.Fatal("foreign source feedback accepted")
	}
	if _, err := db.Pool.Exec(ctx, insert, uuid.New(), user, feedback, game, "mode", "co-op", "avoid", .9, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, insert, uuid.New(), user, feedback, game, "mode", "co-op", "avoid", .9, 1); err == nil {
		t.Fatal("duplicate feedback accepted")
	}
	if _, err := db.Pool.Exec(ctx, insert, uuid.New(), other, otherFeedback, otherGame, "mode", "co-op", "avoid", .9, 1); err != nil {
		t.Fatal(err)
	}
	secondEvent := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key)SELECT $1,recommendation_id,user_id,'dislike',$2 FROM feedback_events WHERE id=$3`, secondEvent, secondEvent.String(), feedback); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, insert, uuid.New(), user, secondEvent, game, "mode", "co-op", "avoid", .9, 1); err != nil {
		t.Fatalf("same target from different feedback rejected: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO game_modes(game_id,mode,source,source_content_hash,last_successful_fetch_at)VALUES($1,'co-op','steam_category','hash',now())`, game); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_classifications(feedback_id,taxonomy,polarity,confidence,actionable,model,input_content_hash,preference_dimension)VALUES($1,'multiplayer','negative',.9,true,'fixture','hash','mode')`, feedback); err == nil {
		t.Fatal("partial preference accepted")
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_classifications(feedback_id,taxonomy,polarity,confidence,actionable,model,input_content_hash,preference_dimension,preference_target,preference_direction)VALUES($1,'single-player','negative',.9,true,'fixture','hash','mode','co-op','avoid')`, feedback); err == nil {
		t.Fatal("contradictory taxonomy accepted")
	}
	if _, err := db.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, user); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM preference_adjustments WHERE user_id=$1`, user).Scan(&count); err != nil || count != 0 {
		t.Fatalf("adjustments after deletion=%d err=%v", count, err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM game_modes WHERE game_id=$1`, game).Scan(&count); err != nil || count != 1 {
		t.Fatalf("shared modes after deletion=%d err=%v", count, err)
	}
}

func TestAdjustmentQueriesPreserveDisabledStateAndOwnership(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user, game, event := preferenceFixture(t, db)
	other, _, _ := preferenceFixture(t, db)
	repository, err := NewSQLRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	args := dbgen.UpsertPreferenceAdjustmentFromFeedbackParams{ID: uuidToPG(uuid.New()), UserID: uuidToPG(user), SourceFeedbackID: uuidToPG(event), Dimension: "mode", Target: "co-op", Direction: "avoid", Confidence: .92, Strength: 1}
	foreign := args
	foreign.UserID = uuidToPG(other)
	if _, err := db.Queries.UpsertPreferenceAdjustmentFromFeedback(ctx, foreign); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign insert error=%v", err)
	}
	invalidOwnerEvent := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key)SELECT $1,recommendation_id,$2,'dislike',$3 FROM feedback_events WHERE id=$4`, invalidOwnerEvent, other, invalidOwnerEvent.String(), event); err != nil {
		t.Fatal(err)
	}
	mismatched := args
	mismatched.UserID = uuidToPG(other)
	mismatched.SourceFeedbackID = uuidToPG(invalidOwnerEvent)
	if _, err := db.Queries.UpsertPreferenceAdjustmentFromFeedback(ctx, mismatched); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("recommendation ownership mismatch accepted: %v", err)
	}
	likeEvent := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key)SELECT $1,recommendation_id,user_id,'like',$2 FROM feedback_events WHERE id=$3`, likeEvent, likeEvent.String(), event); err != nil {
		t.Fatal(err)
	}
	like := args
	like.SourceFeedbackID = uuidToPG(likeEvent)
	if _, err := db.Queries.UpsertPreferenceAdjustmentFromFeedback(ctx, like); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("non-dislike created adjustment: %v", err)
	}
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	q := db.Queries.WithTx(tx)
	if _, err = q.UpsertPreferenceAdjustmentFromFeedback(ctx, args); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.ForFeedback(ctx, user, event); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("rolled-back insert visible: %v", err)
	}
	first, err := db.Queries.UpsertPreferenceAdjustmentFromFeedback(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if uuid.UUID(first.SourceGameID.Bytes) != game {
		t.Fatal("source game not derived from event")
	}
	if _, err = repository.ForFeedback(ctx, other, event); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign read error=%v", err)
	}
	if rows, err := repository.List(ctx, other, true); err != nil || len(rows) != 0 {
		t.Fatalf("foreign list len=%d err=%v", len(rows), err)
	}
	for _, statement := range []string{
		`UPDATE preference_adjustments SET active=false`,
		`UPDATE preference_adjustments SET disabled_at=now()`,
	} {
		if _, err = db.Pool.Exec(ctx, statement); err == nil {
			t.Fatal("incomplete disable accepted")
		}
	}
	job := uuid.New()
	if _, err = db.Pool.Exec(ctx, `INSERT INTO sync_jobs(id,kind,idempotency_key,user_id)VALUES($1,'fixture',$2,$3)`, job, job.String(), user); err != nil {
		t.Fatal(err)
	}
	disabledAt := time.Now().UTC().Truncate(time.Microsecond)
	tx, err = db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q = db.Queries.WithTx(tx)
	if _, err = q.LockPreferenceAdjustment(ctx, dbgen.LockPreferenceAdjustmentParams{UserID: uuidToPG(user), ID: first.ID}); err != nil {
		t.Fatal(err)
	}
	disable := dbgen.MarkPreferenceAdjustmentDisabledParams{UserID: uuidToPG(user), ID: first.ID, DisableRebuildJobID: uuidToPG(job), DisablePreviousRankingVersion: pgtype.Int8{Int64: 7, Valid: true}, DisabledAt: pgtype.Timestamptz{Time: disabledAt, Valid: true}}
	if count, err := q.MarkPreferenceAdjustmentDisabled(ctx, disable); err != nil || count != 1 {
		t.Fatalf("disable count=%d err=%v", count, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	args.ID = uuidToPG(uuid.New())
	args.Confidence = .7
	replayed, err := db.Queries.UpsertPreferenceAdjustmentFromFeedback(ctx, args)
	if err != nil || replayed.ID != first.ID || replayed.Active || replayed.Confidence != .92 {
		t.Fatalf("replay changed immutable adjustment: active=%v confidence=%v err=%v", replayed.Active, replayed.Confidence, err)
	}
	if rows, err := repository.List(ctx, user, false); err != nil || len(rows) != 0 {
		t.Fatalf("active list=%v err=%v", rows, err)
	}
	rows, err := repository.List(ctx, user, true)
	if err != nil || len(rows) != 1 {
		t.Fatalf("diagnostic list len=%d err=%v", len(rows), err)
	}
	row := rows[0]
	if row.DisabledAt == nil || !row.DisabledAt.Equal(disabledAt) || row.DisableRebuildJobID == nil || *row.DisableRebuildJobID != job || row.DisablePreviousRankingVersion == nil || *row.DisablePreviousRankingVersion != 7 {
		t.Fatal("disable metadata lost at domain boundary")
	}
	if _, err = db.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, user); err != nil {
		t.Fatalf("disabled adjustment blocked lifecycle cascade: %v", err)
	}
}

func TestDatabaseModeChecksMatchDomainVocabulary(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	user, game, event := preferenceFixture(t, db)
	for _, mode := range append(Modes(), Mode("unknown")) {
		err := db.Queries.InsertSteamGameMode(ctx, dbgen.InsertSteamGameModeParams{GameID: uuidToPG(game), Mode: string(mode), SourceContentHash: "hash", LastSuccessfulFetchAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}})
		if (err == nil) != ValidMode(mode) {
			t.Fatalf("game_modes vocabulary mismatch mode=%q err=%v", mode, err)
		}
		_, err = db.Pool.Exec(ctx, `INSERT INTO preference_adjustments(id,user_id,source_feedback_id,source_game_id,dimension,target,direction,confidence)VALUES($1,$2,$3,$4,'mode',$5,'avoid',1)`, uuid.New(), user, event, game, string(mode))
		if (err == nil) != ValidMode(mode) {
			t.Fatalf("adjustment vocabulary mismatch mode=%q err=%v", mode, err)
		}
		if _, err := db.Pool.Exec(ctx, `DELETE FROM preference_adjustments`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO game_modes(game_id,mode,source,source_content_hash,last_successful_fetch_at)VALUES($1,'co-op','openai','hash',now())`, game); err == nil {
		t.Fatal("non-Steam mode evidence accepted")
	}
}

func TestClassificationPreferenceTripletConstraints(t *testing.T) {
	db := openPreferenceStore(t)
	ctx := context.Background()
	_, _, event := preferenceFixture(t, db)
	for _, tt := range []struct {
		name                         string
		dimension, target, direction any
		taxonomy, polarity           string
		confidence                   float64
		actionable, valid            bool
	}{
		{"legacy-null", nil, nil, nil, "multiplayer", "negative", .9, true, true},
		{"dimension-only", "mode", nil, nil, "multiplayer", "negative", .9, true, false},
		{"target-only", nil, "co-op", nil, "multiplayer", "negative", .9, true, false},
		{"direction-only", nil, nil, "avoid", "multiplayer", "negative", .9, true, false},
		{"missing-direction", "mode", "co-op", nil, "multiplayer", "negative", .9, true, false},
		{"missing-target", "mode", nil, "avoid", "multiplayer", "negative", .9, true, false},
		{"missing-dimension", nil, "co-op", "avoid", "multiplayer", "negative", .9, true, false},
		{"invalid-dimension", "genre", "co-op", "avoid", "multiplayer", "negative", .9, true, false},
		{"invalid-target", "mode", "unknown", "avoid", "multiplayer", "negative", .9, true, false},
		{"invalid-direction", "mode", "co-op", "prefer", "multiplayer", "negative", .9, true, false},
		{"contradictory-taxonomy", "mode", "co-op", "avoid", "single-player", "negative", .9, true, false},
		{"contradictory-target", "mode", "single-player", "avoid", "multiplayer", "negative", .9, true, false},
		{"other-taxonomy", "mode", "co-op", "avoid", "genre", "negative", .9, true, false},
		{"low-confidence", "mode", "co-op", "avoid", "multiplayer", "negative", .699, true, false},
		{"positive", "mode", "co-op", "avoid", "multiplayer", "positive", .9, true, false},
		{"non-actionable", "mode", "co-op", "avoid", "multiplayer", "negative", .9, false, false},
		{"threshold", "mode", "co-op", "avoid", "multiplayer", "negative", .7, true, true},
		{"single-player", "mode", "single-player", "avoid", "single-player", "negative", .9, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.Pool.Exec(ctx, `INSERT INTO feedback_classifications(feedback_id,taxonomy,polarity,confidence,actionable,model,input_content_hash,preference_dimension,preference_target,preference_direction)VALUES($1,$2,$3,$4,$5,'fixture','hash',$6,$7,$8)`, event, tt.taxonomy, tt.polarity, tt.confidence, tt.actionable, tt.dimension, tt.target, tt.direction)
			if (err == nil) != tt.valid {
				t.Fatalf("accepted=%v want=%v err=%v", err == nil, tt.valid, err)
			}
			if tt.valid {
				if _, err := db.Pool.Exec(ctx, `DELETE FROM feedback_classifications WHERE feedback_id=$1`, event); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	for _, mode := range Modes() {
		taxonomy := "multiplayer"
		if mode == ModeSinglePlayer {
			taxonomy = "single-player"
		}
		args := dbgen.UpsertFeedbackClassificationParams{FeedbackID: uuidToPG(event), Taxonomy: taxonomy, Polarity: "negative", Confidence: .92, Actionable: true, Model: "fixture", InputContentHash: "hash", SchemaVersion: 2, PreferenceDimension: pgtype.Text{String: "mode", Valid: true}, PreferenceTarget: pgtype.Text{String: string(mode), Valid: true}, PreferenceDirection: pgtype.Text{String: "avoid", Valid: true}}
		if err := db.Queries.UpsertFeedbackClassification(ctx, args); err != nil {
			t.Fatal(err)
		}
		stored, err := db.Queries.GetFeedbackClassification(ctx, uuidToPG(event))
		if err != nil || stored.PreferenceTarget.String != string(mode) {
			t.Fatalf("stored canonical mode=%q err=%v", stored.PreferenceTarget.String, err)
		}
		if _, err := db.Pool.Exec(ctx, `DELETE FROM feedback_classifications WHERE feedback_id=$1`, event); err != nil {
			t.Fatal(err)
		}
	}
}
