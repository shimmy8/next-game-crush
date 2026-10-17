package httpapi

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Dependencies struct {
	Frontend fs.FS
}

func NewRouter(deps Dependencies) http.Handler {
	router := chi.NewRouter()
	router.Use(middleware.Recoverer)
	router.Get("/api/health", health)
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
