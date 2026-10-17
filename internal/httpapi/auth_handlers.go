package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"

	"next-game-crush/internal/auth"
)

const loginStateCookieName = "ngc_login_state"

func beginSteamLogin(service *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		redirectURL, err := service.Begin(r.URL.Query().Get("return"))
		if err != nil {
			http.Error(w, "invalid login request", http.StatusBadRequest)
			return
		}
		providerURL, err := url.Parse(redirectURL)
		if err != nil {
			http.Error(w, "login unavailable", http.StatusInternalServerError)
			return
		}
		returnTo, err := url.Parse(providerURL.Query().Get("openid.return_to"))
		if err != nil || returnTo.Query().Get("state") == "" {
			http.Error(w, "login unavailable", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: loginStateCookieName, Value: returnTo.Query().Get("state"), Path: "/auth/steam/callback", HttpOnly: true, Secure: service.SecureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: 600})
		http.Redirect(w, r, redirectURL, http.StatusSeeOther)
	}
}

func completeSteamLogin(service *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stateCookie, err := r.Cookie(loginStateCookieName)
		if err != nil || !constantTimeEqual(stateCookie.Value, r.URL.Query().Get("state")) {
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
			http.Error(w, "Steam authentication failed", http.StatusUnauthorized)
			return
		}
		service.SetSessionCookie(w, session)
		returnPath, err := url.Parse(session.ReturnPath)
		if err != nil || returnPath.IsAbs() || returnPath.Host != "" {
			returnPath = &url.URL{Path: "/"}
		}
		http.Redirect(w, r, returnPath.String(), http.StatusSeeOther)
	}
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

func me(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"authenticated": true, "steamId": session.SteamID, "csrfToken": session.CSRFToken, "expiresAt": session.ExpiresAt})
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
