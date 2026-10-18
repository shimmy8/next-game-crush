package recommendations

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/store"
)

func TestRebuildExplanationMixedOutcomesPublishAtomically(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	owner := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, owner); err != nil {
		t.Fatal(err)
	}
	anchorA := insertRankingGame(t, db, 9300, "SOMA")
	anchorB := insertRankingGame(t, db, 9301, "Into the Breach")
	candidateA := insertRankingGame(t, db, 9302, "Future Horror")
	candidateB := insertRankingGame(t, db, 9303, "Future Tactics")
	features := map[uuid.UUID]string{
		anchorA:    `{"themes":["horror"],"mechanics":["exploration"]}`,
		candidateA: `{"themes":["horror"],"mechanics":["exploration"]}`,
		anchorB:    `{"themes":["strategy"],"mechanics":["turn-based"]}`,
		candidateB: `{"themes":["strategy"],"mechanics":["turn-based"]}`,
	}
	for gameID, value := range features {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_features(game_id,model,input_content_hash,features,source,last_successful_fetch_at) VALUES($1,'text',$2,$3,'test',$4)`, gameID, gameID.String(), value, now); err != nil {
			t.Fatal(err)
		}
	}
	vectorA, vectorB := make([]float32, profile.Dimensions), make([]float32, profile.Dimensions)
	vectorA[0], vectorB[1] = 1, 1
	for gameID, vector := range map[uuid.UUID][]float32{anchorA: vectorA, candidateA: vectorA, anchorB: vectorB, candidateB: vectorB} {
		if err := db.PutGameEmbedding(ctx, store.GameEmbedding{GameID: gameID, Model: "embed", ContentHash: gameID.String(), Vector: vector}); err != nil {
			t.Fatal(err)
		}
	}
	for _, anchor := range []uuid.UUID{anchorA, anchorB} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO user_games(user_id,game_id,playtime_minutes,recently_played_minutes,last_played_at,source,last_successful_fetch_at) VALUES($1,$2,1200,60,$3,'test',$3)`, owner, anchor, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, candidate := range []uuid.UUID{candidateA, candidateB} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, candidate); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,release_date,raw_text,observed_at,source,last_successful_fetch_at) VALUES($1,$2,'day',$3,'future',$4,'test',$4)`, uuid.New(), candidate, now.AddDate(0, 1, 0), now); err != nil {
			t.Fatal(err)
		}
	}
	profileRepo, err := profile.NewSQLRepository(db, "embed")
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.NewService(profileRepo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profiles.Rebuild(ctx, owner); err != nil {
		t.Fatal(err)
	}
	repo, err := NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	repo.now = func() time.Time { return now }
	client := mixedExplanationClient{}
	service, err := NewService(repo, profiles, client, allowAllCurrentEligibility{}, recommendationTestClock)
	if err != nil {
		t.Fatal(err)
	}
	items, rankingVersion, stats, err := service.rebuild(ctx, owner, "mixed-explanations")
	if err != nil || len(items) != 2 || rankingVersion != 1 {
		t.Fatalf("rebuild items=%#v version=%d stats=%#v err=%v", items, rankingVersion, stats, err)
	}
	if stats.AIGenerated != 1 || stats.Deterministic != 1 || stats.None != 0 || stats.FailureReasons[ExplanationFailureRateLimited] != 1 {
		t.Fatalf("stats=%#v", stats)
	}
	byName := map[string]Recommendation{}
	for _, item := range items {
		byName[item.Name] = item
	}
	if byName["Future Horror"].ExplanationSource != ExplanationSourceAI || byName["Future Tactics"].ExplanationSource != ExplanationSourceDeterministic {
		t.Fatalf("mixed sources=%#v", byName)
	}
	if byName["Future Horror"].Explanation == byName["Future Tactics"].Explanation || !strings.Contains(byName["Future Horror"].Explanation, "horror") || !strings.Contains(byName["Future Tactics"].Explanation, "strategy") {
		t.Fatalf("explanations are not candidate-specific: %#v", byName)
	}
	var published bool
	if err := db.Pool.QueryRow(ctx, `SELECT published FROM recommendation_runs WHERE user_id=$1 AND ranking_version=$2`, owner, rankingVersion).Scan(&published); err != nil || !published {
		t.Fatalf("published=%v err=%v", published, err)
	}
}

type mixedExplanationClient struct{}

func (mixedExplanationClient) ExplainRecommendation(_ context.Context, input ai.RecommendationEvidence) (ai.Explanation, error) {
	if input.GameName == "Future Tactics" {
		return ai.Explanation{}, &ai.RequestError{StatusCode: 429, Temporary: true}
	}
	return ai.Explanation{Text: "Игра совпадает с вашими интересами в horror.", EvidenceKeys: []string{input.Evidence[0].Key}}, nil
}

func TestExplanationPersistenceStoresV2AndProjectsPublicFields(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	owner, profileID, recommendationID := uuid.New(), uuid.New(), uuid.New()
	gameID, sourceID := insertRankingGame(t, db, 9400, "Grounded Candidate"), uuid.New()
	factors, _ := json.Marshal(Factors{TagAffinity: .8, Confidence: 1})
	weights, _ := json.Marshal(DefaultWeights())
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id) VALUES($1)`, []any{owner}},
		{`INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata) VALUES($1,$2,1,'{}')`, []any{profileID, owner}},
		{`INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published) VALUES($1,$2,$3,1,$4,'grounded-persistence',false)`, []any{uuid.New(), owner, profileID, AlgorithmVersion}},
		{`INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,ranking_version,score) VALUES($1,$2,$3,$4,$5,1,50)`, []any{recommendationID, owner, gameID, profileID, AlgorithmVersion}},
		{`INSERT INTO recommendation_factors(recommendation_id,factors,weights) VALUES($1,$2,$3)`, []any{recommendationID, factors, weights}},
	} {
		if _, err := db.Pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	record := ExplanationRecord{
		Text:          "Совпадают темы и механики: horror.",
		Source:        ExplanationSourceDeterministic,
		FailureReason: ExplanationFailureDisabled,
		Supplied: []GroundingEvidence{{
			ID: "feature-overlap-1", Kind: EvidenceFeatureOverlap,
			PublicText: "Совпадают темы и механики: horror.", Strength: 8,
			SourceGameIDs: []uuid.UUID{sourceID},
		}},
		CitedIDs: []string{"feature-overlap-1"},
	}
	if err := repo.SaveExplanation(ctx, owner, 1, "grounded-persistence", recommendationID, "Grounded Candidate", record); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	var source, reason, hash string
	var generated bool
	if err := db.Pool.QueryRow(ctx, `SELECT explanation_evidence,explanation_source,explanation_failure_reason,explanation_generated,explanation_input_content_hash FROM recommendations WHERE id=$1`, recommendationID).Scan(&raw, &source, &reason, &generated, &hash); err != nil {
		t.Fatal(err)
	}
	var stored StoredEvidence
	if err := json.Unmarshal(raw, &stored); err != nil || stored.Version != StoredEvidenceVersion || len(stored.Items) != 1 || len(stored.Items[0].SourceGameIDs) != 1 || stored.Items[0].SourceGameIDs[0] != sourceID {
		t.Fatalf("stored evidence=%#v err=%v", stored, err)
	}
	if source != "deterministic" || reason != "disabled" || generated || hash == "" {
		t.Fatalf("status source=%q reason=%q generated=%v hash=%q", source, reason, generated, hash)
	}
	oldHash := hash
	record.Supplied[0].PublicText = "Совпадают темы и механики: exploration."
	record.Text = record.Supplied[0].PublicText
	if err := repo.SaveExplanation(ctx, owner, 1, "grounded-persistence", recommendationID, "Grounded Candidate", record); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT explanation_input_content_hash FROM recommendations WHERE id=$1`, recommendationID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == oldHash {
		t.Fatal("v2 explanation hash did not change with public evidence")
	}
	if err := repo.PublishRanking(ctx, owner, 1, "grounded-persistence"); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(ctx, owner, recommendationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExplanationSource != ExplanationSourceDeterministic || got.GeneratedExplanation || len(got.Evidence) != 1 || got.Evidence[0].Kind != EvidenceFeatureOverlap {
		t.Fatalf("public recommendation=%#v", got)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), sourceID.String()) || strings.Contains(string(encoded), "sourceGameIds") || strings.Contains(string(encoded), "strength") {
		t.Fatalf("private grounding leaked: %s", encoded)
	}
}

func TestLegacyExplanationReadDegradesMalformedAndDeterministicRows(t *testing.T) {
	db := openRankingStore(t)
	ctx := context.Background()
	owner, profileID := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,algorithm_metadata) VALUES($1,$2,1,'{}')`, profileID, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published) VALUES($1,$2,$3,1,$4,'legacy-read',true)`, uuid.New(), owner, profileID, AlgorithmVersion); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, explanation, evidence, source string
		generated                           bool
		wantSource                          ExplanationSource
	}{
		{name: "safe generated", explanation: "Игра близка к SOMA по атмосфере.", evidence: `[{"id":"semantic-affinity","text":"Похожа на игры, которые вам нравятся"}]`, source: "ai", generated: true, wantSource: ExplanationSourceAI},
		{name: "wishlist only generated", explanation: "Игра вам понравится, потому что вы добавили её в желаемое.", evidence: `[{"id":"wishlist","text":"Игра есть в вашем списке желаемого"}]`, source: "ai", generated: true, wantSource: ExplanationSourceNone},
		{name: "mixed wishlist claim generated", explanation: "Игра вам понравится, потому что вы добавили её в желаемое.", evidence: `[{"id":"semantic-affinity","text":"Похожа на игры, которые вам нравятся"},{"id":"wishlist","text":"Игра есть в вашем списке желаемого"}]`, source: "ai", generated: true, wantSource: ExplanationSourceNone},
		{name: "generic deterministic", explanation: "Рекомендуем, потому что игра похожа на ваши интересы.", evidence: `[{"id":"semantic-affinity","text":"Похожа на игры, которые вам нравятся"}]`, source: "deterministic", wantSource: ExplanationSourceNone},
		{name: "malformed", explanation: "Технически небезопасно.", evidence: `"not-an-evidence-object"`, source: "ai", generated: true, wantSource: ExplanationSourceNone},
		{name: "unknown v2 kind", explanation: "Неподдерживаемое основание.", evidence: `{"version":2,"items":[{"id":"unknown-1","kind":"unknown","publicText":"Неизвестное основание.","strength":3}]}`, source: "deterministic", wantSource: ExplanationSourceNone},
		{name: "tampered deterministic v2", explanation: "Другой неподтверждённый текст.", evidence: `{"version":2,"items":[{"id":"feature-overlap-1","kind":"feature_overlap","publicText":"Совпадают темы и механики: horror.","strength":3}]}`, source: "deterministic", wantSource: ExplanationSourceNone},
	}
	weights, _ := json.Marshal(DefaultWeights())
	factors, _ := json.Marshal(Factors{Confidence: 1})
	for i, test := range tests {
		gameID := insertRankingGame(t, db, int64(9500+i), test.name)
		recommendationID := uuid.New()
		if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,ranking_version,score,explanation,explanation_evidence,explanation_generated,explanation_source,created_at) VALUES($1,$2,$3,$4,$5,1,$6,$7,$8,$9,$10,$11)`, recommendationID, owner, gameID, profileID, AlgorithmVersion, 50-i, test.explanation, test.evidence, test.generated, test.source, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendation_factors(recommendation_id,factors,weights) VALUES($1,$2,$3)`, recommendationID, factors, weights); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	items, err := repo.Latest(ctx, owner)
	if err != nil || len(items) != len(tests) {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	byName := make(map[string]Recommendation, len(items))
	for _, item := range items {
		byName[item.Name] = item
	}
	for _, test := range tests {
		got := byName[test.name]
		if got.ExplanationSource != test.wantSource || got.GeneratedExplanation != (test.wantSource == ExplanationSourceAI) {
			t.Fatalf("%s source=%q generated=%v", test.name, got.ExplanationSource, got.GeneratedExplanation)
		}
		if test.wantSource == ExplanationSourceNone {
			if got.Explanation != neutralExplanation || len(got.Evidence) != 0 {
				t.Fatalf("%s did not degrade safely: %#v", test.name, got)
			}
		} else if got.Explanation != test.explanation || len(got.Evidence) != 1 {
			t.Fatalf("%s safe legacy row lost: %#v", test.name, got)
		}
	}
}
