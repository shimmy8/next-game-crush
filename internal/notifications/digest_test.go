package notifications

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
)

func TestComposeDigestIsDeterministicEscapedAndMultipart(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	signer, _ := NewSigner([]byte(strings.Repeat("x", 32)), "recommendation-feedback")
	positivePercent := int32(91)
	input := DigestInput{UserID: uuid.New(), DisplayName: `<script>alert("x")</script>`, Recommendations: []DigestRecommendation{
		{ID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), GameID: uuid.MustParse("10000000-0000-0000-0000-000000000002"), Name: "Zulu & <bad>", Score: 91, Explanation: `<img src=x onerror=alert(1)>`, Release: "Coming Soon", Reviews: &DigestReviewSummary{Band: "unrated", Label: "No user reviews"}},
		{ID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), GameID: uuid.MustParse("10000000-0000-0000-0000-000000000001"), Name: "Alpha", Score: 91, Explanation: "Grounded", Release: "October 2026", Reviews: &DigestReviewSummary{Band: "positive", Label: "Very Positive", PositivePercent: &positivePercent, Total: 1284}},
	}, Wishlist: []DigestWishlist{{GameID: uuid.New(), Name: "Wish <one>", Release: "18 Oct 2026"}}}
	one, err := ComposeDigest(input, signer, "http://localhost:8080", now)
	if err != nil {
		t.Fatal(err)
	}
	two, err := ComposeDigest(input, signer, "http://localhost:8080", now)
	if err != nil {
		t.Fatal(err)
	}
	if one.ContentHash != two.ContentHash {
		t.Fatalf("content hash changed: %s != %s", one.ContentHash, two.ContentHash)
	}
	input.Recommendations[0].ID = uuid.New()
	input.Recommendations[1].ID = uuid.New()
	rebuilt, err := ComposeDigest(input, signer, "http://localhost:8080", now)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ContentHash != one.ContentHash {
		t.Fatal("ephemeral recommendation IDs changed stable digest identity")
	}
	if strings.Index(one.HTML, "Alpha") > strings.Index(one.HTML, "Zulu") {
		t.Fatal("tie ordering is not deterministic")
	}
	for _, unsafe := range []string{"<script>", "<img", "Wish <one>"} {
		if strings.Contains(one.HTML, unsafe) {
			t.Fatalf("HTML contains unsafe %q", unsafe)
		}
	}
	for _, text := range []string{"Alpha", "Like", "Dislike", "Remind me", "Upcoming from your wishlist", "91% положительных · 1 284 отзыва", "Пока нет отзывов"} {
		if !strings.Contains(one.Text, text) {
			t.Fatalf("plain part missing %q", text)
		}
		if !strings.Contains(one.HTML, text) {
			t.Fatalf("HTML part missing %q", text)
		}
	}
	changedPercent := int32(92)
	input.Recommendations[1].Reviews.PositivePercent = &changedPercent
	changed, err := ComposeDigest(input, signer, "http://localhost:8080", now)
	if err != nil {
		t.Fatal(err)
	}
	if changed.ContentHash == one.ContentHash || changed.Recommendations[0].ContentHash == one.Recommendations[0].ContentHash {
		t.Fatal("rendered review change did not change digest identities")
	}
}

func TestDeliveryAcceptanceBoundary(t *testing.T) {
	delivery := Delivery{ID: uuid.New(), UserID: uuid.New(), Text: "plain", HTML: "<p>html</p>", Subject: "subject", Recipient: "next-game-crush@localhost"}
	t.Run("failure before acceptance is retryable", func(t *testing.T) {
		repo := &deliveryRepoStub{delivery: delivery}
		svc := deliveryServiceForTest(repo, SenderFunc(func(context.Context, Message) (bool, error) { return false, errors.New("dial") }))
		err := svc.Deliver(context.Background(), delivery.ID, "worker", uuid.New())
		if err == nil || repo.failed != 1 || repo.ambiguous != 0 {
			t.Fatalf("err=%v failed=%d ambiguous=%d", err, repo.failed, repo.ambiguous)
		}
	})
	t.Run("failure after acceptance is ambiguous without retry", func(t *testing.T) {
		repo := &deliveryRepoStub{delivery: delivery, confirmErr: errors.New("commit lost")}
		svc := deliveryServiceForTest(repo, SenderFunc(func(context.Context, Message) (bool, error) { return true, nil }))
		err := svc.Deliver(context.Background(), delivery.ID, "worker", uuid.New())
		if !errors.Is(err, ErrAmbiguousDelivery) || repo.ambiguous != 1 {
			t.Fatalf("err=%v ambiguous=%d", err, repo.ambiguous)
		}
	})
	t.Run("ambiguity must be durable before job completion", func(t *testing.T) {
		repo := &deliveryRepoStub{delivery: delivery, confirmErr: errors.New("commit lost"), ambiguousErr: errors.New("database unavailable")}
		svc := deliveryServiceForTest(repo, SenderFunc(func(context.Context, Message) (bool, error) { return true, nil }))
		err := svc.Deliver(context.Background(), delivery.ID, "worker", uuid.New())
		if err == nil || errors.Is(err, ErrAmbiguousDelivery) {
			t.Fatalf("non-durable ambiguity was swallowed: %v", err)
		}
	})
}

func TestEnqueueFailureIsRecoveredByRepeatedRequest(t *testing.T) {
	userID := uuid.New()
	deliveryID := uuid.New()
	repo := &deliveryRepoStub{delivery: Delivery{ID: deliveryID, UserID: userID, Status: "pending"}, input: DigestInput{UserID: userID, DisplayName: "Player"}}
	scheduler := &recoveryScheduler{fail: true}
	signer, _ := NewSigner([]byte(strings.Repeat("x", 32)), actionPurpose)
	service, err := NewService(repo, scheduler, SenderFunc(func(context.Context, Message) (bool, error) { return true, nil }), signer, "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }
	if _, err = service.SendTestDigest(context.Background(), userID); err == nil {
		t.Fatal("first enqueue failure was hidden")
	}
	got, err := service.SendTestDigest(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if got != deliveryID || scheduler.attempts != 2 {
		t.Fatalf("delivery=%s attempts=%d", got, scheduler.attempts)
	}
}

type recoveryScheduler struct {
	fail     bool
	attempts int
}

func (s *recoveryScheduler) Enqueue(context.Context, jobs.Spec) (uuid.UUID, error) {
	s.attempts++
	if s.fail {
		s.fail = false
		return uuid.Nil, errors.New("queue unavailable")
	}
	return uuid.New(), nil
}

func deliveryServiceForTest(repository Repository, sender Sender) *Service {
	signer, _ := NewSigner([]byte(strings.Repeat("x", 32)), "recommendation-feedback")
	return &Service{repository: repository, sender: sender, signer: signer, now: time.Now}
}

type deliveryRepoStub struct {
	delivery          Delivery
	input             DigestInput
	created           bool
	confirmErr        error
	ambiguousErr      error
	failed, ambiguous int
}

func (r *deliveryRepoStub) DigestInput(context.Context, uuid.UUID) (DigestInput, error) {
	return r.input, nil
}
func (r *deliveryRepoStub) CreateDelivery(context.Context, NewDelivery) (Delivery, bool, error) {
	created := !r.created
	r.created = true
	return r.delivery, created, nil
}
func (r *deliveryRepoStub) ClaimDelivery(context.Context, uuid.UUID, string, uuid.UUID) (Delivery, error) {
	return r.delivery, nil
}
func (r *deliveryRepoStub) MarkSent(context.Context, uuid.UUID, string, uuid.UUID) error {
	return r.confirmErr
}
func (r *deliveryRepoStub) MarkFailed(context.Context, uuid.UUID, string, uuid.UUID, error) error {
	r.failed++
	return nil
}
func (r *deliveryRepoStub) MarkAmbiguous(context.Context, uuid.UUID, string, uuid.UUID, error) error {
	r.ambiguous++
	return r.ambiguousErr
}
func (r *deliveryRepoStub) ReviveFailedDeliveryJob(context.Context, uuid.UUID) error { return nil }
func (r *deliveryRepoStub) ConsumeAction(context.Context, Action) (bool, error)      { return false, nil }
