package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/preferences"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/recommendations"
)

func TestRecordIsIdempotent(t *testing.T) {
	repo := newFakeRepository()
	queue := &fakeQueue{}
	service, err := NewService(repo, fakeClassifier{}, "test-model", queue)
	if err != nil {
		t.Fatal(err)
	}
	command := Command{UserID: uuid.New(), RecommendationID: uuid.New(), Action: ActionLike, Reason: "genre", FreeText: "More games like this", IdempotencyKey: "browser-event-1"}

	first, err := service.Record(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Record(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("accepted values differ: %#v %#v", first, second)
	}
	if repo.created != 1 || queue.enqueues != 2 {
		t.Fatalf("created=%d enqueues=%d", repo.created, queue.enqueues)
	}
	if len(repo.snapshots) != 1 || repo.snapshots[0].ID != command.RecommendationID {
		t.Fatal("original recommendation snapshot was not saved once")
	}
	for _, spec := range queue.specs {
		if spec.UserID != command.UserID || spec.Priority != 100 {
			t.Fatal("feedback workflow must preserve user ownership and foreground priority")
		}
	}
}

func TestRecordCanonicalizesMultipleReasonsForOrderInsensitiveIdempotency(t *testing.T) {
	repo := newFakeRepository()
	service, err := NewService(repo, fakeClassifier{}, "test-model", &fakeQueue{})
	if err != nil {
		t.Fatal(err)
	}
	base := Command{UserID: uuid.New(), RecommendationID: uuid.New(), Action: ActionLike, Reasons: []string{"setting", "genre"}, FreeText: "More like this", IdempotencyKey: "multi-reason"}
	first, err := service.Record(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	base.Reasons = []string{"genre", "setting"}
	second, err := service.Record(context.Background(), base)
	if err != nil || first != second || repo.created != 1 {
		t.Fatalf("order-insensitive retry = %#v %#v created=%d err=%v", first, second, repo.created, err)
	}
	event := repo.events[base.IdempotencyKey]
	if !reflect.DeepEqual(event.Reasons, []string{"genre", "setting"}) || event.Reason != "genre" {
		t.Fatalf("canonical reasons=%v legacy=%q", event.Reasons, event.Reason)
	}
	for _, invalid := range [][]string{{"genre", "genre"}, {""}, {"unknown"}} {
		command := base
		command.IdempotencyKey = uuid.NewString()
		command.Reasons = invalid
		if _, err := service.Record(context.Background(), command); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("reasons %q error=%v", invalid, err)
		}
	}
}

func TestLowConfidenceTextDoesNotChangeProfile(t *testing.T) {
	repo := newFakeRepository()
	queue := &fakeQueue{}
	service, _ := NewService(repo, fakeClassifier{output: ai.FeedbackClassification{Taxonomy: "genre", Polarity: "negative", Confidence: .3, Actionable: true, EvidenceKeys: []string{"feedback-text"}}}, "test-model", queue)
	accepted, err := service.Record(context.Background(), Command{UserID: uuid.New(), RecommendationID: uuid.New(), Action: ActionDislike, FreeText: "Maybe not quite my thing", IdempotencyKey: "low-confidence"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessClassification(context.Background(), accepted.EventID); err != nil {
		t.Fatal(err)
	}
	if len(repo.classifications) != 1 || repo.classifications[0].Actionable {
		t.Fatalf("classification=%#v", repo.classifications)
	}
}

func TestLikeRaisesRelatedCandidate(t *testing.T) {
	x, y := make([]float32, profile.Dimensions), make([]float32, profile.Dimensions)
	x[0], y[1] = 1, 1
	targetID := uuid.New()
	asOf := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	before := profile.BuildVectors([]profile.Signal{{GameID: uuid.New(), Vector: x, PlaytimeMinutes: 100}, {GameID: targetID, Vector: y, Wishlisted: true}}, asOf)
	after := profile.BuildVectors([]profile.Signal{{GameID: uuid.New(), Vector: x, PlaytimeMinutes: 100}, {GameID: targetID, Vector: y, Wishlisted: true, Likes: 1}}, asOf)
	beforeScore := recommendations.Score(recommendations.Factors{PositiveSimilarity: positive(profile.Cosine(before.Positive, y)), Confidence: 1}, recommendations.DefaultWeights()).Score
	afterScore := recommendations.Score(recommendations.Factors{PositiveSimilarity: positive(profile.Cosine(after.Positive, y)), Confidence: 1}, recommendations.DefaultWeights()).Score
	if afterScore <= beforeScore {
		t.Fatalf("related score did not rise: before=%f after=%f", beforeScore, afterScore)
	}
}

func TestHistoricalDislikesStaySoftWhileCurrentNegativeActionExcludesExactGame(t *testing.T) {
	if !recommendations.Eligible(recommendations.Candidate{GameID: uuid.New(), Dislikes: 2}) {
		t.Fatal("historical dislikes must remain a soft signal")
	}
	if recommendations.Eligible(recommendations.Candidate{GameID: uuid.New(), Dislikes: 2, ExcludedByLatestDirectAction: true}) {
		t.Fatal("current dislike or legacy exclude must exclude the exact game")
	}
}

func TestMissingOpenAIStoresDeterministicFallback(t *testing.T) {
	repo := newFakeRepository()
	service, _ := NewService(repo, fakeClassifier{err: ai.ErrCapabilityDisabled}, "test-model", &fakeQueue{})
	accepted, err := service.Record(context.Background(), Command{UserID: uuid.New(), RecommendationID: uuid.New(), Action: ActionLike, Reason: "setting", FreeText: "I love this world", IdempotencyKey: "no-ai"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessClassification(context.Background(), accepted.EventID); err != nil {
		t.Fatal(err)
	}
	got := repo.classifications[0]
	if got.Model != "deterministic-fallback" || got.Taxonomy != "setting" || !got.Actionable || got.Confidence != 1 {
		t.Fatalf("classification=%#v", got)
	}
}

func TestClassificationIsCachedAcrossWorkflowResumes(t *testing.T) {
	repo := newFakeRepository()
	calls := 0
	service, _ := NewService(repo, fakeClassifier{output: ai.FeedbackClassification{Taxonomy: "genre", Polarity: "positive", Confidence: .9, Actionable: true}, calls: &calls}, "test-model", &fakeQueue{})
	accepted, err := service.Record(context.Background(), Command{UserID: uuid.New(), RecommendationID: uuid.New(), Action: ActionLike, FreeText: "More like this", IdempotencyKey: "cached"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessClassification(context.Background(), accepted.EventID); err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessClassification(context.Background(), accepted.EventID); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("classifier calls=%d, want 1", calls)
	}
}

func TestNonActionableFeedbackNeverCreatesAdjustment(t *testing.T) {
	valid := ai.FeedbackClassification{Taxonomy: "multiplayer", Polarity: "negative", Confidence: .92, Actionable: true, Preference: &preferences.Target{Dimension: "mode", Target: preferences.ModeCoop, Direction: "avoid"}, EvidenceKeys: []string{"feedback-text"}}
	for _, test := range []struct {
		name, action, reason, text string
		output                     ai.FeedbackClassification
		err                        error
		want                       preferences.Mode
	}{
		{"valid", ActionDislike, "", "не люблю коопы", valid, nil, preferences.ModeCoop},
		{"like", ActionLike, "", "не люблю коопы", valid, nil, ""},
		{"remind", ActionRemind, "", "", ai.FeedbackClassification{}, nil, ""},
		{"no text", ActionDislike, "multiplayer", "", ai.FeedbackClassification{}, nil, preferences.ModeMultiplayer},
		{"disabled", ActionDislike, "multiplayer", "не люблю коопы", ai.FeedbackClassification{}, ai.ErrCapabilityDisabled, preferences.ModeMultiplayer},
		{"http", ActionDislike, "multiplayer", "не люблю коопы", ai.FeedbackClassification{}, ai.ErrHTTP, preferences.ModeMultiplayer},
		{"timeout", ActionDislike, "multiplayer", "не люблю коопы", ai.FeedbackClassification{}, context.DeadlineExceeded, preferences.ModeMultiplayer},
		{"refusal", ActionDislike, "multiplayer", "не люблю коопы", ai.FeedbackClassification{}, ai.ErrRefusal, ""},
		{"incomplete", ActionDislike, "multiplayer", "не люблю коопы", ai.FeedbackClassification{}, ai.ErrIncomplete, ""},
		{"malformed", ActionDislike, "multiplayer", "не люблю коопы", ai.FeedbackClassification{}, ai.ErrInvalidOutput, ""},
		{"bad evidence", ActionDislike, "multiplayer", "не люблю коопы", ai.FeedbackClassification{}, ai.ErrInvalidEvidence, ""},
		{"free text disabled", ActionDislike, "", "не люблю коопы", ai.FeedbackClassification{}, ai.ErrCapabilityDisabled, ""},
		{"unsupported reason disabled", ActionDislike, "genre", "не люблю коопы", ai.FeedbackClassification{}, ai.ErrCapabilityDisabled, ""},
		{"valid nonactionable", ActionDislike, "multiplayer", "maybe", ai.FeedbackClassification{Taxonomy: "multiplayer", Polarity: "neutral", Confidence: .6, EvidenceKeys: []string{"feedback-text"}}, nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newFakeRepository()
			s, _ := NewService(repo, fakeClassifier{output: test.output, err: test.err}, "test", &fakeQueue{})
			accepted, err := s.Record(context.Background(), Command{UserID: uuid.New(), RecommendationID: uuid.New(), Action: test.action, Reason: test.reason, FreeText: test.text, IdempotencyKey: test.name})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.ProcessClassification(context.Background(), accepted.EventID); err != nil {
				t.Fatal(err)
			}
			c := repo.classifications[0]
			if test.want == "" {
				if c.Preference != nil {
					t.Fatal("unexpected preference")
				}
			} else if c.Preference == nil || c.Preference.Target != test.want {
				t.Fatal("missing preference")
			}
		})
	}
	for _, mutate := range []func(*ai.FeedbackClassification){
		func(v *ai.FeedbackClassification) { v.Polarity = "positive" }, func(v *ai.FeedbackClassification) { v.Polarity = "neutral" }, func(v *ai.FeedbackClassification) { v.Actionable = false }, func(v *ai.FeedbackClassification) { v.Confidence = .699 }, func(v *ai.FeedbackClassification) { v.Preference.Target = "unsupported" }, func(v *ai.FeedbackClassification) { v.Taxonomy = "single-player" }, func(v *ai.FeedbackClassification) { v.EvidenceKeys = []string{"other"} }, func(v *ai.FeedbackClassification) { v.EvidenceKeys = []string{"feedback-text", "feedback-text"} },
	} {
		v := valid
		p := *valid.Preference
		v.Preference = &p
		mutate(&v)
		repo := newFakeRepository()
		s, _ := NewService(repo, fakeClassifier{output: v}, "test", &fakeQueue{})
		a, _ := s.Record(context.Background(), Command{UserID: uuid.New(), RecommendationID: uuid.New(), Action: ActionDislike, Reason: "multiplayer", FreeText: "private text", IdempotencyKey: "invalid"})
		if err := s.ProcessClassification(context.Background(), a.EventID); err != nil {
			t.Fatal(err)
		}
		if repo.classifications[0].Preference != nil || repo.classifications[0].Actionable {
			t.Fatal("injected invalid output accepted")
		}
	}
}

func TestTextOver500CharactersIsRejected(t *testing.T) {
	service, _ := NewService(newFakeRepository(), fakeClassifier{}, "test-model", &fakeQueue{})
	_, err := service.Record(context.Background(), Command{UserID: uuid.New(), RecommendationID: uuid.New(), Action: ActionLike, FreeText: strings.Repeat("я", 501), IdempotencyKey: "too-long"})
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("error=%v", err)
	}
}

type fakeClassifier struct {
	output ai.FeedbackClassification
	err    error
	calls  *int
}

func (f fakeClassifier) ClassifyFeedback(context.Context, ai.FeedbackText) (ai.FeedbackClassification, error) {
	if f.calls != nil {
		*f.calls++
	}
	return f.output, f.err
}

type fakeQueue struct {
	enqueues int
	ids      []uuid.UUID
	specs    []jobs.Spec
}

func (q *fakeQueue) Enqueue(_ context.Context, spec jobs.Spec) (uuid.UUID, error) {
	q.enqueues++
	q.specs = append(q.specs, spec)
	for i, key := range []string{} {
		_ = i
		_ = key
	}
	// Model the queue's idempotency contract.
	for index := 0; index+1 < len(q.ids); index += 2 {
		if q.ids[index].String() == spec.IdempotencyKey {
			return q.ids[index+1], nil
		}
	}
	id := uuid.New()
	// Store a deterministic lookup token without needing another map.
	keyID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(spec.IdempotencyKey))
	for index := 0; index+1 < len(q.ids); index += 2 {
		if q.ids[index] == keyID {
			return q.ids[index+1], nil
		}
	}
	q.ids = append(q.ids, keyID, id)
	return id, nil
}

type fakeRepository struct {
	events          map[string]Event
	created         int
	snapshots       []recommendations.Recommendation
	classifications []Classification
}

func newFakeRepository() *fakeRepository { return &fakeRepository{events: map[string]Event{}} }
func (r *fakeRepository) Record(_ context.Context, command Command) (Event, bool, error) {
	if event, ok := r.events[command.IdempotencyKey]; ok {
		return event, false, nil
	}
	snapshot := recommendations.Recommendation{ID: command.RecommendationID, RankingVersion: 7, GameID: uuid.New(), Name: "Snapshot"}
	event := Event{ID: uuid.New(), UserID: command.UserID, RecommendationID: command.RecommendationID, Action: command.Action, Reason: command.Reason, Reasons: append([]string(nil), command.Reasons...), FreeText: command.FreeText, IdempotencyKey: command.IdempotencyKey, PreviousRankingVersion: 7, Snapshot: json.RawMessage(`{"name":"Snapshot"}`)}
	r.events[command.IdempotencyKey] = event
	r.created++
	r.snapshots = append(r.snapshots, snapshot)
	return event, true, nil
}
func (r *fakeRepository) LinkRebuildJob(_ context.Context, eventID, jobID uuid.UUID) error {
	for key, event := range r.events {
		if event.ID == eventID {
			event.RebuildJobID = jobID
			r.events[key] = event
		}
	}
	return nil
}
func (r *fakeRepository) Event(_ context.Context, id uuid.UUID) (Event, error) {
	for _, event := range r.events {
		if event.ID == id {
			return event, nil
		}
	}
	return Event{}, errors.New("not found")
}
func (r *fakeRepository) SaveClassification(_ context.Context, classification Classification) error {
	for _, value := range r.classifications {
		if value.FeedbackID == classification.FeedbackID {
			return nil
		}
	}
	r.classifications = append(r.classifications, classification)
	return nil
}
func (r *fakeRepository) Classification(_ context.Context, eventID uuid.UUID) (Classification, error) {
	for _, classification := range r.classifications {
		if classification.FeedbackID == eventID {
			return classification, nil
		}
	}
	return Classification{}, pgx.ErrNoRows
}
func (r *fakeRepository) FinalizeRanking(context.Context, uuid.UUID) (int64, error) { return 8, nil }
func (r *fakeRepository) RankingDiff(context.Context, uuid.UUID, int64, uuid.UUID) (RankingDiff, error) {
	return RankingDiff{}, nil
}

func positive(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}
