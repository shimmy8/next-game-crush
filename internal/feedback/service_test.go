package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/ai"
	"next-game-crush/internal/jobs"
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
	before := profile.BuildVectors([]profile.Signal{{GameID: uuid.New(), Vector: x, PlaytimeMinutes: 100}, {GameID: targetID, Vector: y, Wishlisted: true}})
	after := profile.BuildVectors([]profile.Signal{{GameID: uuid.New(), Vector: x, PlaytimeMinutes: 100}, {GameID: targetID, Vector: y, Wishlisted: true, Likes: 1}})
	beforeScore := recommendations.Score(recommendations.Factors{PositiveSimilarity: positive(profile.Cosine(before.Positive, y)), Confidence: 1}, recommendations.DefaultWeights()).Score
	afterScore := recommendations.Score(recommendations.Factors{PositiveSimilarity: positive(profile.Cosine(after.Positive, y)), Confidence: 1}, recommendations.DefaultWeights()).Score
	if afterScore <= beforeScore {
		t.Fatalf("related score did not rise: before=%f after=%f", beforeScore, afterScore)
	}
}

func TestSingleDislikeDoesNotBanGenre(t *testing.T) {
	if !recommendations.Eligible(recommendations.Candidate{GameID: uuid.New(), Dislikes: 1}) {
		t.Fatal("a single dislike must remain a soft signal")
	}
	if recommendations.Eligible(recommendations.Candidate{GameID: uuid.New(), Dislikes: 2}) {
		t.Fatal("repeated dislikes should permit exclusion")
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
}

func (q *fakeQueue) Enqueue(_ context.Context, spec jobs.Spec) (uuid.UUID, error) {
	q.enqueues++
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
	event := Event{ID: uuid.New(), UserID: command.UserID, RecommendationID: command.RecommendationID, Action: command.Action, Reason: command.Reason, FreeText: command.FreeText, IdempotencyKey: command.IdempotencyKey, PreviousRankingVersion: 7, Snapshot: json.RawMessage(`{"name":"Snapshot"}`)}
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
