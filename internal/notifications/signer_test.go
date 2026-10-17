package notifications

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSignerExpiryTamperingAndBinding(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	action := Action{UserID: uuid.New(), RecommendationID: uuid.New(), Action: "like", IssuedAt: now, ExpiresAt: now.Add(time.Hour), Nonce: uuid.New()}
	signer, err := NewSigner([]byte(strings.Repeat("s", 32)), "recommendation-feedback")
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(action)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := signer.Verify(token, now.Add(time.Minute))
	if err != nil || verified != action {
		t.Fatalf("verify = %#v, %v", verified, err)
	}

	t.Run("expired", func(t *testing.T) {
		if _, err := signer.Verify(token, action.ExpiresAt); err != ErrExpired {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("tampered", func(t *testing.T) {
		last := token[len(token)-1]
		replacement := byte('A')
		if last == replacement {
			replacement = 'B'
		}
		if _, err := signer.Verify(token[:len(token)-1]+string(replacement), now); err == nil {
			t.Fatal("tampered token verified")
		}
	})
	t.Run("purpose bound", func(t *testing.T) {
		other, _ := NewSigner([]byte(strings.Repeat("s", 32)), "unsubscribe")
		if _, err := other.Verify(token, now); err == nil {
			t.Fatal("token verified for another purpose")
		}
	})
	t.Run("recommendation bound", func(t *testing.T) {
		payload, signature, ok := strings.Cut(token, ".")
		if !ok {
			t.Fatal("missing signature")
		}
		// A payload change cannot retain the original signature.
		payload = strings.Replace(payload, payload[len(payload)-1:], "A", 1)
		if _, err := signer.Verify(payload+"."+signature, now); err == nil {
			t.Fatal("altered recommendation verified")
		}
	})
}

func TestSignerRejectsInvalidActionsAndShortSecrets(t *testing.T) {
	if _, err := NewSigner([]byte("short"), "recommendation-feedback"); err == nil {
		t.Fatal("short secret accepted")
	}
	signer, _ := NewSigner([]byte(strings.Repeat("x", 32)), "recommendation-feedback")
	now := time.Now().UTC()
	_, err := signer.Sign(Action{UserID: uuid.New(), RecommendationID: uuid.New(), Action: "delete_everything", IssuedAt: now, ExpiresAt: now.Add(time.Hour), Nonce: uuid.New()})
	if err == nil {
		t.Fatal("invalid action accepted")
	}
}
