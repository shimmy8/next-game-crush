package httpapi

import (
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/observability"
)

type Dependencies struct {
	Frontend        fs.FS
	Logger          *slog.Logger
	Observability   *observability.Logger
	Auth            *auth.Service
	Sync            SyncService
	Identity        IdentityReadService
	Profile         ProfileReadService
	Preferences     PreferenceAdjustmentService
	Recommendations RecommendationReadService
	Feedback        FeedbackService
	Digests         DigestService
	Data            DataService
	TestCatalog     TestCatalogService
	DemoEnabled     bool
}

func NewRouter(deps Dependencies) http.Handler {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deps.Observability == nil {
		deps.Observability = observability.New(nil)
	}
	router := chi.NewRouter()
	router.Use(requestIDMiddleware)
	router.Use(middleware.Recoverer)
	router.Get("/api/health", health(deps.Data, deps.DemoEnabled))
	if deps.Auth != nil {
		if deps.Auth.FixtureEnabled() {
			router.Post("/auth/fixture", fixtureLogin(deps.Auth))
		}
		if deps.Auth.DemoEnabled() {
			router.Post("/auth/demo", demoLogin(deps.Auth))
		}
		router.Get("/auth/steam", beginSteamLogin(deps.Auth, deps.Logger))
		router.Get("/auth/steam/callback", completeSteamLogin(deps.Auth, deps.Logger))
		router.With(deps.Auth.Middleware).Get("/api/me", me(deps.Identity))
		router.With(deps.Auth.Middleware, deps.Auth.RequireCSRF).Post("/auth/logout", logout(deps.Auth))
		if deps.Sync != nil {
			router.With(deps.Auth.Middleware, deps.Auth.RequireOwner, deps.Auth.RequireCSRF).Post("/api/sync/preflight", syncPreflight(deps.Sync))
			router.With(deps.Auth.Middleware, deps.Auth.RequireOwner, deps.Auth.RequireCSRF).Post("/api/sync", startSync(deps.Sync, deps.Observability))
			router.With(deps.Auth.Middleware, deps.Auth.RequireOwner, deps.Auth.RequireCSRF).Post("/api/sync/refresh-if-stale", refreshSyncIfStale(deps.Sync, deps.Observability))
			router.With(deps.Auth.Middleware).Get("/api/sync/current", currentSyncStatus(deps.Sync))
			router.With(deps.Auth.Middleware).Get("/api/sync/{id}", syncStatus(deps.Sync))
		}
		if deps.Profile != nil {
			router.With(deps.Auth.Middleware).Get("/api/profile", getProfile(deps.Profile, deps.Preferences))
		}
		if deps.Preferences != nil {
			router.With(deps.Auth.Middleware).Get("/api/preference-adjustments", getPreferenceAdjustments(deps.Preferences))
			router.With(deps.Auth.Middleware, deps.Auth.RequireOwner, deps.Auth.RequireCSRF).Delete("/api/preference-adjustments/{id}", disablePreferenceAdjustment(deps.Preferences))
		}
		if deps.Recommendations != nil {
			router.With(deps.Auth.Middleware).Get("/api/recommendations/overview", getRecommendationOverview(deps.Recommendations))
			router.With(deps.Auth.Middleware).Get("/api/calendar", getCalendar(deps.Recommendations))
			router.With(deps.Auth.Middleware).Get("/api/recommendations/{id}", getRecommendation(deps.Recommendations))
		}
		if deps.Feedback != nil {
			router.With(deps.Auth.Middleware, deps.Auth.RequireOwner, deps.Auth.RequireCSRF).Post("/api/recommendations/{id}/feedback", recordFeedback(deps.Feedback, false))
			router.With(deps.Auth.Middleware, deps.Auth.RequireOwner, deps.Auth.RequireCSRF).Post("/api/recommendations/{id}/reminder", recordFeedback(deps.Feedback, true))
			router.With(deps.Auth.Middleware).Get("/api/rankings/{version}/diff", rankingDiff(deps.Feedback))
		}
		if deps.Digests != nil {
			router.With(deps.Auth.Middleware, deps.Auth.RequireOwner, deps.Auth.RequireCSRF).Post("/api/digests/test", sendTestDigest(deps.Digests))
		}
		if deps.Data != nil {
			router.With(deps.Auth.Middleware).Get("/api/jobs", listJobs(deps.Data))
			router.With(deps.Auth.Middleware, deps.Auth.RequireOwner, deps.Auth.RequireCSRF).Delete("/api/data", deleteData(deps.Data, deps.Auth))
		}
		if deps.TestCatalog != nil {
			router.With(deps.Auth.Middleware, deps.Auth.RequireOwner, deps.Auth.RequireCSRF).Post("/api/test/catalog/runs", startTestCatalogRun(deps.TestCatalog))
			router.With(deps.Auth.Middleware, deps.Auth.RequireOwner).Get("/api/test/catalog/runs/{id}", getTestCatalogRun(deps.TestCatalog))
			router.With(deps.Auth.Middleware, deps.Auth.RequireOwner, deps.Auth.RequireCSRF).Post("/api/test/catalog/runtime/restart", restartTestCatalogRuntime(deps.TestCatalog))
		}
	}
	if deps.Digests != nil {
		router.Get("/actions/feedback/{token}", emailFeedbackAction(deps.Digests))
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
