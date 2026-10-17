package auth

import (
	"context"
	"net/http"
)

const SessionCookieName = "ngc_session"

type sessionContextKey struct{}

func (s *Service) SetSessionCookie(w http.ResponseWriter, session Session) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookieName, Value: session.Token, Path: "/", HttpOnly: true, Secure: s.baseURL.Scheme == "https", SameSite: http.SameSiteLaxMode, Expires: session.ExpiresAt, MaxAge: int(session.ExpiresAt.Sub(s.now()).Seconds())})
}

func (s *Service) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookieName, Path: "/", HttpOnly: true, Secure: s.baseURL.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(SessionCookieName)
		if err != nil {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		session, err := s.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			s.ClearSessionCookie(w)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, session)))
	})
}

func (s *Service) RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		session, ok := SessionFromContext(r.Context())
		provided := r.Header.Get("X-CSRF-Token")
		origin := r.Header.Get("Origin")
		if !ok || provided == "" || subtleCompare(provided, session.CSRFToken) == false || origin != "" && origin != s.Origin() {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func SessionFromContext(ctx context.Context) (Session, bool) {
	session, ok := ctx.Value(sessionContextKey{}).(Session)
	return session, ok
}
func subtleCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var result byte
	for i := range a {
		result |= a[i] ^ b[i]
	}
	return result == 0
}
