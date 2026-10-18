package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/google/uuid"
	"next-game-crush/internal/auth"
	syncservice "next-game-crush/internal/sync"
)

const loginStateCookieName = "ngc_login_state"

func beginSteamLogin(service *auth.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		redirectURL, err := service.Begin(r.URL.Query().Get("return"))
		if err != nil {
			diagnostic := auth.Diagnose(err)
			logSteamAuth(r, logger, authFailureLogLevel(diagnostic), "begin", failureOutcome(diagnostic), diagnostic, http.StatusBadRequest)
			http.Error(w, "invalid login request", http.StatusBadRequest)
			return
		}
		providerURL, err := url.Parse(redirectURL)
		if err != nil {
			diagnostic := auth.Diagnostic{Stage: "redirect_construction", Reason: "internal_failure", Infrastructure: true}
			logSteamAuth(r, logger, slog.LevelError, "begin", "failed", diagnostic, http.StatusInternalServerError)
			http.Error(w, "login unavailable", http.StatusInternalServerError)
			return
		}
		returnTo, err := url.Parse(providerURL.Query().Get("openid.return_to"))
		if err != nil || returnTo.Query().Get("state") == "" {
			diagnostic := auth.Diagnostic{Stage: "redirect_construction", Reason: "internal_failure", Infrastructure: true}
			logSteamAuth(r, logger, slog.LevelError, "begin", "failed", diagnostic, http.StatusInternalServerError)
			http.Error(w, "login unavailable", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: loginStateCookieName, Value: returnTo.Query().Get("state"), Path: "/auth/steam/callback", HttpOnly: true, Secure: service.SecureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: 600})
		logSteamAuth(r, logger, slog.LevelInfo, "begin", "redirected", auth.Diagnostic{}, http.StatusSeeOther)
		http.Redirect(w, r, redirectURL, http.StatusSeeOther)
	}
}

func completeSteamLogin(service *auth.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stateCookie, err := r.Cookie(loginStateCookieName)
		if err != nil {
			diagnostic := auth.Diagnostic{Stage: "callback_cookie", Reason: "missing"}
			logSteamAuth(r, logger, slog.LevelWarn, "callback", "rejected", diagnostic, http.StatusUnauthorized)
			http.Error(w, "Steam authentication failed", http.StatusUnauthorized)
			return
		}
		if !constantTimeEqual(stateCookie.Value, r.URL.Query().Get("state")) {
			diagnostic := auth.Diagnostic{Stage: "callback_cookie", Reason: "state_mismatch"}
			logSteamAuth(r, logger, slog.LevelWarn, "callback", "rejected", diagnostic, http.StatusUnauthorized)
			http.Error(w, "Steam authentication failed", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: loginStateCookieName, Path: "/auth/steam/callback", HttpOnly: true, Secure: service.SecureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: -1})
		ctx := r.Context()
		if cookie, cookieErr := r.Cookie(auth.SessionCookieName); cookieErr == nil {
			if old, authErr := service.Authenticate(ctx, cookie.Value); authErr == nil {
				ctx = service.WithSessionRotation(ctx, old.ID)
			}
		}
		session, err := service.Complete(ctx, r.URL.Query())
		if err != nil {
			diagnostic := auth.Diagnose(err)
			outcome := failureOutcome(diagnostic)
			if errors.Is(err, auth.ErrUserCancelled) {
				outcome = "cancelled"
			}
			logSteamAuth(r, logger, authFailureLogLevel(diagnostic), "callback", outcome, diagnostic, http.StatusUnauthorized)
			http.Error(w, "Steam authentication failed", http.StatusUnauthorized)
			return
		}
		service.SetSessionCookie(w, session)
		returnPath, err := url.Parse(session.ReturnPath)
		if err != nil || returnPath.IsAbs() || returnPath.Host != "" {
			returnPath = &url.URL{Path: "/"}
		}
		logSteamAuth(r, logger, slog.LevelInfo, "callback", "authenticated", auth.Diagnostic{}, http.StatusSeeOther)
		http.Redirect(w, r, returnPath.String(), http.StatusSeeOther)
	}
}

func authFailureLogLevel(diagnostic auth.Diagnostic) slog.Level {
	if diagnostic.Reason == "user_cancelled" {
		return slog.LevelInfo
	}
	if diagnostic.Infrastructure {
		return slog.LevelError
	}
	return slog.LevelWarn
}

func failureOutcome(diagnostic auth.Diagnostic) string {
	if diagnostic.Infrastructure {
		return "failed"
	}
	return "rejected"
}

func logSteamAuth(r *http.Request, logger *slog.Logger, level slog.Level, phase, outcome string, diagnostic auth.Diagnostic, status int) {
	logger.LogAttrs(r.Context(), level, "steam authentication",
		slog.String("event", "steam_auth"),
		slog.String("phase", phase),
		slog.String("outcome", outcome),
		slog.String("stage", diagnostic.Stage),
		slog.String("reason", diagnostic.Reason),
		slog.String("request_id", requestIDFromContext(r.Context())),
		slog.Int("http_status", status),
	)
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var different byte
	for i := range a {
		different |= a[i] ^ b[i]
	}
	return different == 0
}

type IdentityReadService interface {
	SteamIdentity(context.Context, uuid.UUID) (syncservice.SteamIdentity, error)
}

func me(identityReader IdentityReadService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		identity := syncservice.SteamIdentity{}
		if identityReader != nil {
			if refreshed, err := identityReader.SteamIdentity(r.Context(), session.UserID); err == nil {
				identity = refreshed
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"authenticated": true, "steamId": session.SteamID, "csrfToken": session.CSRFToken, "expiresAt": session.ExpiresAt, "displayName": identity.DisplayName, "avatarUrl": identity.AvatarURL})
	}
}

func logout(service *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if err := service.Logout(r.Context(), session.ID); err != nil {
			http.Error(w, "logout failed", http.StatusInternalServerError)
			return
		}
		service.ClearSessionCookie(w)
		w.WriteHeader(http.StatusNoContent)
	}
}

func fixtureLogin(service *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !service.FixtureEnabled() {
			http.NotFound(w, r)
			return
		}
		session, err := service.CreateFixtureSession(r.Context())
		if err != nil {
			http.Error(w, "fixture authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		service.SetSessionCookie(w, session)
		writeJSON(w, map[string]any{"authenticated": true, "csrfToken": session.CSRFToken})
	}
}
