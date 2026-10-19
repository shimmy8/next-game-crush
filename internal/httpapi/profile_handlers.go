package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/profile"
)

type ProfileReadService interface {
	Latest(context.Context, uuid.UUID) (profile.Profile, error)
}

type profileView struct {
	ID            uuid.UUID             `json:"id"`
	Version       int64                 `json:"version"`
	Summary       string                `json:"summary"`
	SummaryStatus profile.SummaryStatus `json:"summaryStatus"`
	SummarySource profile.SummarySource `json:"summarySource"`
	Confidence    float64               `json:"confidence"`
	EvidenceCount int                   `json:"evidenceCount"`
	CreatedAt     time.Time             `json:"createdAt"`
	PublishedAt   time.Time             `json:"publishedAt"`
	Updating      bool                  `json:"updating"`
}

func getProfile(service ProfileReadService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		value, err := service.Latest(r.Context(), session.UserID)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "profile unavailable", http.StatusInternalServerError)
			return
		}
		if value.ID == uuid.Nil || value.Version < 1 || strings.TrimSpace(value.Summary) == "" || value.PublicationState != profile.PublicationPublished || value.PublishedAt == nil || value.PublishedAt.IsZero() || value.CreatedAt.IsZero() ||
			(value.SummarySource != profile.SummarySourceOpenAI && value.SummarySource != profile.SummarySourceDeterministic && value.SummarySource != profile.SummarySourceLegacy) ||
			value.Confidence < 0 || value.Confidence > 1 || value.EvidenceCount < 0 {
			http.Error(w, "profile unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, profileView{
			ID: value.ID, Version: value.Version, Summary: value.Summary, SummaryStatus: profile.SummaryReady,
			SummarySource: value.SummarySource, Confidence: value.Confidence, EvidenceCount: value.EvidenceCount,
			CreatedAt: value.CreatedAt, PublishedAt: value.PublishedAt.UTC(), Updating: value.Updating,
		})
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
