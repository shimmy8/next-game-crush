package notifications

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidToken = errors.New("invalid action token")
	ErrExpired      = errors.New("action token expired")
)

type Action struct {
	UserID           uuid.UUID `json:"u"`
	RecommendationID uuid.UUID `json:"r"`
	Action           string    `json:"a"`
	IssuedAt         time.Time `json:"i"`
	ExpiresAt        time.Time `json:"e"`
	Nonce            uuid.UUID `json:"n"`
}

type Signer struct {
	secret  []byte
	purpose string
}

func NewSigner(secret []byte, purpose string) (*Signer, error) {
	if len(secret) < 16 || strings.TrimSpace(purpose) == "" {
		return nil, errors.New("action signing secret must be at least 16 bytes and purpose is required")
	}
	return &Signer{secret: append([]byte(nil), secret...), purpose: purpose}, nil
}

func (s *Signer) Sign(action Action) (string, error) {
	action.IssuedAt = action.IssuedAt.UTC().Truncate(time.Microsecond)
	action.ExpiresAt = action.ExpiresAt.UTC().Truncate(time.Microsecond)
	if err := validateAction(action); err != nil {
		return "", err
	}
	payload, err := json.Marshal(action)
	if err != nil {
		return "", fmt.Errorf("encode action: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + base64.RawURLEncoding.EncodeToString(s.signature([]byte(encoded))), nil
}

func (s *Signer) Verify(token string, now time.Time) (Action, error) {
	payload, signature, ok := strings.Cut(token, ".")
	if !ok || payload == "" || signature == "" || strings.Contains(signature, ".") {
		return Action{}, ErrInvalidToken
	}
	provided, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || base64.RawURLEncoding.EncodeToString(provided) != signature || !hmac.Equal(provided, s.signature([]byte(payload))) {
		return Action{}, ErrInvalidToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != payload {
		return Action{}, ErrInvalidToken
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var action Action
	if err := decoder.Decode(&action); err != nil || validateAction(action) != nil {
		return Action{}, ErrInvalidToken
	}
	if !now.Before(action.ExpiresAt) {
		return Action{}, ErrExpired
	}
	if now.Before(action.IssuedAt.Add(-5 * time.Minute)) {
		return Action{}, ErrInvalidToken
	}
	return action, nil
}

func (s *Signer) signature(payload []byte) []byte {
	h := hmac.New(sha256.New, s.secret)
	h.Write([]byte(s.purpose))
	h.Write([]byte{0})
	h.Write(payload)
	return h.Sum(nil)
}

func validateAction(action Action) error {
	if action.UserID == uuid.Nil || action.RecommendationID == uuid.Nil || action.Nonce == uuid.Nil || action.IssuedAt.IsZero() || action.ExpiresAt.IsZero() || !action.ExpiresAt.After(action.IssuedAt) {
		return ErrInvalidToken
	}
	if action.Action != "like" && action.Action != "dislike" && action.Action != "remind_me" {
		return ErrInvalidToken
	}
	return nil
}
