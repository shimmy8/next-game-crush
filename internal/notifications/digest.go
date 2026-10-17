package notifications

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
)

const (
	JobKind       = "notifications.digest.send"
	actionPurpose = "recommendation-feedback"
)

var ErrAmbiguousDelivery = errors.New("SMTP accepted message but delivery confirmation failed")

type DigestRecommendation struct {
	ID, GameID                        uuid.UUID
	Name                              string
	Score                             float64
	Explanation, Release, ContentHash string
}
type DigestWishlist struct {
	GameID        uuid.UUID
	Name, Release string
}
type DigestInput struct {
	UserID          uuid.UUID
	DisplayName     string
	Recommendations []DigestRecommendation
	Wishlist        []DigestWishlist
}
type Digest struct {
	Subject, Text, HTML, ContentHash string
	Actions                          []Action
	Recommendations                  []DeliveryRecommendation
}
type DeliveryRecommendation struct {
	RecommendationID, GameID uuid.UUID
	ContentHash              string
}
type Delivery struct {
	ID, UserID                             uuid.UUID
	Status, Recipient, Subject, Text, HTML string
}
type NewDelivery struct {
	Delivery
	Kind, ContentHash, IdempotencyKey string
	Actions                           []Action
	Recommendations                   []DeliveryRecommendation
}

type Repository interface {
	DigestInput(context.Context, uuid.UUID) (DigestInput, error)
	CreateDelivery(context.Context, NewDelivery) (Delivery, bool, error)
	ClaimDelivery(context.Context, uuid.UUID, string, uuid.UUID) (Delivery, error)
	MarkSent(context.Context, uuid.UUID, string, uuid.UUID) error
	MarkFailed(context.Context, uuid.UUID, string, uuid.UUID, error) error
	MarkAmbiguous(context.Context, uuid.UUID, string, uuid.UUID, error) error
	ReviveFailedDeliveryJob(context.Context, uuid.UUID) error
	ConsumeAction(context.Context, Action) (alreadyConsumed bool, err error)
}
type Scheduler interface {
	Enqueue(context.Context, jobs.Spec) (uuid.UUID, error)
}

type Service struct {
	repository Repository
	scheduler  Scheduler
	sender     Sender
	signer     *Signer
	baseURL    string
	now        func() time.Time
}

func NewService(repository Repository, scheduler Scheduler, sender Sender, signer *Signer, baseURL string) (*Service, error) {
	if repository == nil || scheduler == nil || sender == nil || signer == nil {
		return nil, errors.New("notification repository, scheduler, sender, and signer are required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("valid application base URL is required")
	}
	return &Service{repository: repository, scheduler: scheduler, sender: sender, signer: signer, baseURL: strings.TrimRight(baseURL, "/"), now: time.Now}, nil
}

func (s *Service) SendTestDigest(ctx context.Context, userID uuid.UUID) (uuid.UUID, error) {
	if userID == uuid.Nil {
		return uuid.Nil, errors.New("user ID is required")
	}
	input, err := s.repository.DigestInput(ctx, userID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("load digest input: %w", err)
	}
	now := s.now().UTC()
	digest, err := ComposeDigest(input, s.signer, s.baseURL, now)
	if err != nil {
		return uuid.Nil, err
	}
	isoYear, isoWeek := now.ISOWeek()
	keyRaw := fmt.Sprintf("%s:test:%s:%d-%02d", userID, digest.ContentHash, isoYear, isoWeek)
	keySum := sha256.Sum256([]byte(keyRaw))
	key := "digest:" + hex.EncodeToString(keySum[:])
	delivery, _, err := s.repository.CreateDelivery(ctx, NewDelivery{Delivery: Delivery{ID: uuid.New(), UserID: userID, Status: "pending", Recipient: "next-game-crush@localhost", Subject: digest.Subject, Text: digest.Text, HTML: digest.HTML}, Kind: "test", ContentHash: digest.ContentHash, IdempotencyKey: key, Actions: digest.Actions, Recommendations: digest.Recommendations})
	if err != nil {
		return uuid.Nil, fmt.Errorf("create digest delivery: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{"version": 1, "delivery_id": delivery.ID})
	if _, err := s.scheduler.Enqueue(ctx, jobs.Spec{Kind: JobKind, Stage: "digest", Required: true, IdempotencyKey: "digest-delivery:" + delivery.ID.String(), Payload: payload, MaxAttempts: 3}); err != nil {
		return uuid.Nil, fmt.Errorf("enqueue digest delivery: %w", err)
	}
	if err := s.repository.ReviveFailedDeliveryJob(ctx, delivery.ID); err != nil {
		return uuid.Nil, fmt.Errorf("revive failed digest delivery: %w", err)
	}
	return delivery.ID, nil
}

func (s *Service) Deliver(ctx context.Context, deliveryID uuid.UUID, workerID string, token uuid.UUID) error {
	delivery, err := s.repository.ClaimDelivery(ctx, deliveryID, workerID, token)
	if err != nil {
		return err
	}
	if delivery.Status == "sent" || delivery.Status == "ambiguous" {
		return nil
	}
	accepted, sendErr := s.sender.Send(ctx, Message{To: delivery.Recipient, Subject: delivery.Subject, Text: delivery.Text, HTML: delivery.HTML})
	if sendErr != nil && !accepted {
		if markErr := s.repository.MarkFailed(ctx, delivery.ID, workerID, token, sendErr); markErr != nil {
			return errors.Join(sendErr, markErr)
		}
		return sendErr
	}
	if sendErr != nil || s.repository.MarkSent(ctx, delivery.ID, workerID, token) != nil {
		cause := sendErr
		if cause == nil {
			cause = errors.New("database confirmation failed after SMTP acceptance")
		}
		if err := s.repository.MarkAmbiguous(ctx, delivery.ID, workerID, token, cause); err != nil {
			return fmt.Errorf("persist ambiguous delivery after SMTP uncertainty: %w", errors.Join(cause, err))
		}
		return errors.Join(ErrAmbiguousDelivery, cause)
	}
	return nil
}

func (s *Service) ConsumeAction(ctx context.Context, token string) (bool, error) {
	action, err := s.signer.Verify(token, s.now().UTC())
	if err != nil {
		return false, err
	}
	return s.repository.ConsumeAction(ctx, action)
}

func (s *Service) RegisterHandlers(worker *jobs.Worker) error {
	if worker == nil {
		return errors.New("worker is required")
	}
	return worker.Register(JobKind, func(ctx context.Context, lease jobs.Lease) error {
		var payload struct {
			Version    int       `json:"version"`
			DeliveryID uuid.UUID `json:"delivery_id"`
		}
		if json.Unmarshal(lease.Payload, &payload) != nil || payload.Version != 1 || payload.DeliveryID == uuid.Nil {
			return errors.New("unsupported digest job payload schema")
		}
		err := s.Deliver(ctx, payload.DeliveryID, lease.WorkerID, lease.FencingToken)
		if errors.Is(err, ErrAmbiguousDelivery) {
			return nil
		}
		return err
	})
}

func ComposeDigest(input DigestInput, signer *Signer, baseURL string, now time.Time) (Digest, error) {
	if input.UserID == uuid.Nil || signer == nil {
		return Digest{}, errors.New("digest user and signer are required")
	}
	now = now.UTC().Truncate(time.Microsecond)
	recommendations := append([]DigestRecommendation(nil), input.Recommendations...)
	sort.Slice(recommendations, func(i, j int) bool {
		if recommendations[i].Score != recommendations[j].Score {
			return recommendations[i].Score > recommendations[j].Score
		}
		a, b := strings.ToLower(recommendations[i].Name), strings.ToLower(recommendations[j].Name)
		if a != b {
			return a < b
		}
		return recommendations[i].GameID.String() < recommendations[j].GameID.String()
	})
	wishlist := append([]DigestWishlist(nil), input.Wishlist...)
	sort.Slice(wishlist, func(i, j int) bool {
		if wishlist[i].Release != wishlist[j].Release {
			return wishlist[i].Release < wishlist[j].Release
		}
		a, b := strings.ToLower(wishlist[i].Name), strings.ToLower(wishlist[j].Name)
		if a != b {
			return a < b
		}
		return wishlist[i].GameID.String() < wishlist[j].GameID.String()
	})
	type stableRecommendation struct {
		GameID      uuid.UUID `json:"game_id"`
		Name        string    `json:"name"`
		Score       float64   `json:"score"`
		Explanation string    `json:"explanation"`
		Release     string    `json:"release"`
	}
	stable := make([]stableRecommendation, 0, len(recommendations))
	for _, item := range recommendations {
		stable = append(stable, stableRecommendation{item.GameID, item.Name, item.Score, item.Explanation, item.Release})
	}
	canonical, _ := json.Marshal(struct {
		DisplayName     string                 `json:"display_name"`
		Recommendations []stableRecommendation `json:"recommendations"`
		Wishlist        []DigestWishlist       `json:"wishlist"`
	}{input.DisplayName, stable, wishlist})
	sum := sha256.Sum256(canonical)
	digest := Digest{Subject: "Your Next Game Crush weekly picks", ContentHash: hex.EncodeToString(sum[:])}
	var text, body strings.Builder
	text.WriteString("Next Game Crush\n\nHello " + input.DisplayName + "\n\nUpcoming from your wishlist\n")
	body.WriteString("<!doctype html><html><body><h1>Next Game Crush</h1><p>Hello " + html.EscapeString(input.DisplayName) + "</p><h2>Upcoming from your wishlist</h2><ul>")
	for _, item := range wishlist {
		text.WriteString("- " + item.Name + " — " + item.Release + "\n")
		body.WriteString("<li><strong>" + html.EscapeString(item.Name) + "</strong> — " + html.EscapeString(item.Release) + "</li>")
	}
	body.WriteString("</ul><h2>Top new recommendations</h2>")
	text.WriteString("\nTop new recommendations\n")
	for _, item := range recommendations {
		hash := item.ContentHash
		if hash == "" {
			h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%.6f\x00%s\x00%s", item.GameID, item.Name, item.Score, item.Explanation, item.Release)))
			hash = hex.EncodeToString(h[:])
		}
		digest.Recommendations = append(digest.Recommendations, DeliveryRecommendation{RecommendationID: item.ID, GameID: item.GameID, ContentHash: hash})
		body.WriteString("<article><h3>" + html.EscapeString(item.Name) + "</h3><p>" + html.EscapeString(item.Release) + " · " + fmt.Sprintf("%.0f", item.Score) + "/100</p><p>" + html.EscapeString(item.Explanation) + "</p><p>")
		text.WriteString(fmt.Sprintf("- %s (%s, %.0f/100)\n  %s\n", item.Name, item.Release, item.Score, item.Explanation))
		for _, actionName := range []string{"like", "dislike", "remind_me"} {
			action := Action{UserID: input.UserID, RecommendationID: item.ID, Action: actionName, IssuedAt: now, ExpiresAt: now.Add(14 * 24 * time.Hour), Nonce: uuid.New()}
			token, err := signer.Sign(action)
			if err != nil {
				return Digest{}, err
			}
			digest.Actions = append(digest.Actions, action)
			label := map[string]string{"like": "Like", "dislike": "Dislike", "remind_me": "Remind me"}[actionName]
			link := strings.TrimRight(baseURL, "/") + "/actions/feedback/" + url.PathEscape(token)
			body.WriteString(`<a href="` + html.EscapeString(link) + `">` + label + `</a> `)
			text.WriteString("  " + label + ": " + link + "\n")
		}
		body.WriteString("</p></article>")
	}
	body.WriteString("</body></html>")
	digest.Text = text.String()
	digest.HTML = body.String()
	return digest, nil
}
