package httpapi

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"next-game-crush/internal/auth"
)

type Dependencies struct {
	Frontend fs.FS
	Auth     *auth.Service
	Sync     SyncService
}

func NewRouter(deps Dependencies) http.Handler {
	router := chi.NewRouter()
	router.Use(middleware.Recoverer)
	router.Get("/api/health", health)
	if deps.Auth != nil {
		router.Get("/auth/steam", beginSteamLogin(deps.Auth))
		router.Get("/auth/steam/callback", completeSteamLogin(deps.Auth))
		router.With(deps.Auth.Middleware).Get("/api/me", me)
		router.With(deps.Auth.Middleware, deps.Auth.RequireCSRF).Post("/auth/logout", logout(deps.Auth))
		if deps.Sync != nil {
			router.With(deps.Auth.Middleware, deps.Auth.RequireCSRF).Post("/api/sync", startSync(deps.Sync))
			router.With(deps.Auth.Middleware).Get("/api/sync/{id}", syncStatus(deps.Sync))
		}
	}
	if deps.Frontend != nil {
		files := http.FileServer(http.FS(deps.Frontend))
		router.Get("/*", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api" || r.URL.Path == "/auth" || r.URL.Path == "/actions" || strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") || strings.HasPrefix(r.URL.Path, "/actions/") {
				http.NotFound(w, r)
				return
			}
			name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
			if name == "" {
				name = "index.html"
			}
			info, err := fs.Stat(deps.Frontend, name)
			if err != nil || info.IsDir() {
				if path.Ext(name) != "" {
					http.NotFound(w, r)
					return
				}
				// Browser routes resolve to the SPA; asset and API misses stay 404s.
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			}
			files.ServeHTTP(w, r)
		})
	}
	return router
}
