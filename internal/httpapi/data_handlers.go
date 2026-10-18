package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"next-game-crush/internal/auth"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/store"
)

type ComponentHealth struct {
	Status    string     `json:"status"`
	CheckedAt time.Time  `json:"checkedAt"`
	FreshAt   *time.Time `json:"freshAt,omitempty"`
	Message   string     `json:"message,omitempty"`
}

type HealthReport struct {
	Status           string                        `json:"status"`
	Components       map[string]ComponentHealth    `json:"components"`
	DemoEnabled      bool                          `json:"demoEnabled"`
	CatalogDiscovery *catalog.DiscoveryDiagnostics `json:"catalogDiscovery,omitempty"`
	CatalogSearch    *catalog.SearchDiagnostics    `json:"catalogSearch,omitempty"`
}

type JobDiagnostic struct {
	ID           uuid.UUID              `json:"id"`
	Kind         string                 `json:"kind"`
	Stage        string                 `json:"stage"`
	Status       string                 `json:"status"`
	Attempts     int                    `json:"attempts"`
	CreatedAt    time.Time              `json:"createdAt"`
	UpdatedAt    time.Time              `json:"updatedAt"`
	Error        string                 `json:"error,omitempty"`
	Explanations *ExplanationDiagnostic `json:"explanations,omitempty"`
}

type ExplanationDiagnostic struct {
	AIGenerated    int            `json:"aiGenerated"`
	Deterministic  int            `json:"deterministic"`
	None           int            `json:"none"`
	FailureReasons map[string]int `json:"failureReasons"`
}

var explanationFailureReasonAllowlist = [...]string{
	"disabled",
	"rate_limited",
	"timeout",
	"provider_unavailable",
	"refusal",
	"invalid_output",
	"invalid_citation",
	"unsafe_text",
	"unknown",
}

type DataService interface {
	DeleteUserData(context.Context, uuid.UUID) error
	Jobs(context.Context, uuid.UUID) ([]JobDiagnostic, error)
	Health(context.Context) HealthReport
}

type CatalogSearchDiagnosticsReader interface {
	Diagnostics(context.Context) (catalog.SearchDiagnostics, error)
}

type SQLDataService struct {
	store          *store.Store
	aiEnabled      bool
	smtpConfigured bool
	now            func() time.Time
	discovery      interface {
		Diagnostics(context.Context) (catalog.DiscoveryDiagnostics, error)
	}
	searchDiagnostics CatalogSearchDiagnosticsReader
}

func (s *SQLDataService) SetSearchDiagnosticsReader(reader CatalogSearchDiagnosticsReader) {
	s.searchDiagnostics = reader
}

func (s *SQLDataService) SetDiscoveryDiagnosticsReader(reader interface {
	Diagnostics(context.Context) (catalog.DiscoveryDiagnostics, error)
}) {
	s.discovery = reader
}

func NewSQLDataService(database *store.Store, aiEnabled, smtpConfigured bool) *SQLDataService {
	return &SQLDataService{store: database, aiEnabled: aiEnabled, smtpConfigured: smtpConfigured, now: time.Now}
}

// DeleteUserData is deliberately a single transaction. Deleting the user
// cascades every user-owned row while shared games, release dates, features and
// game embeddings remain. The tombstone rejects work scheduled by a stale
// handler and the missing users row rejects all later user-owned writes.
func (s *SQLDataService) DeleteUserData(ctx context.Context, userID uuid.UUID) error {
	if s == nil || s.store == nil || userID == uuid.Nil {
		return errors.New("data service and user are required")
	}
	for attempt := 0; attempt < 3; attempt++ {
		err := s.deleteUserDataOnce(ctx, userID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (pgErr.Code != "40001" && pgErr.Code != "40P01") {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return errors.New("data deletion could not obtain a consistent snapshot")
}

func (s *SQLDataService) deleteUserDataOnce(ctx context.Context, userID uuid.UUID) error {
	tx, err := s.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text,0))`, userID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO deleted_user_tombstones(user_id) VALUES($1) ON CONFLICT(user_id) DO UPDATE SET deleted_at=now()`, userID); err != nil {
		return err
	}
	// The FK on sync_jobs.user_id cancels queued, waiting and running jobs and
	// removes their attempts before the user-owned rows are cascaded.
	command, err := tx.Exec(ctx, `DELETE FROM users WHERE id=$1`, userID)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	// Run this after deleting the user: a sync transaction that already held an
	// FK reference can finish while DELETE waits, including inserting wishlist
	// catalog membership. Once DELETE returns, those writers are fenced and this
	// cleanup also removes their late catalog rows. Seed rows remain shared.
	if _, err = tx.Exec(ctx, `DELETE FROM catalog_entries ce WHERE origin='wishlist'
		AND NOT EXISTS(SELECT 1 FROM catalog_entry_sources src WHERE src.game_id=ce.game_id AND src.active)`); err != nil {
		return err
	}
	// Remove game metadata discovered only through this single user's imports.
	// Preserve shared provenance and discovery history even after active membership ends.
	if _, err = tx.Exec(ctx, `DELETE FROM games g WHERE NOT EXISTS(SELECT 1 FROM catalog_entries c WHERE c.game_id=g.id)
		AND NOT EXISTS(SELECT 1 FROM catalog_entry_sources src WHERE src.game_id=g.id)
		AND NOT EXISTS(SELECT 1 FROM catalog_discovery_observations obs WHERE obs.steam_app_id=g.steam_app_id)
		AND NOT EXISTS(SELECT 1 FROM user_games ug WHERE ug.game_id=g.id)
		AND NOT EXISTS(SELECT 1 FROM wishlist_entries w WHERE w.game_id=g.id)`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *SQLDataService) Jobs(ctx context.Context, userID uuid.UUID) ([]JobDiagnostic, error) {
	rows, err := s.store.Pool.Query(ctx, `SELECT id,kind,stage,status,attempts,created_at,updated_at,last_error,result FROM sync_jobs WHERE user_id=$1 ORDER BY created_at DESC,id DESC LIMIT 100`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []JobDiagnostic{}
	for rows.Next() {
		var item JobDiagnostic
		var raw *string
		var rawResult []byte
		if err := rows.Scan(&item.ID, &item.Kind, &item.Stage, &item.Status, &item.Attempts, &item.CreatedAt, &item.UpdatedAt, &raw, &rawResult); err != nil {
			return nil, err
		}
		if raw != nil {
			item.Error = SanitizeDiagnosticError(*raw)
		}
		if item.Kind == "recommendations.rebuild" {
			item.Explanations = parseExplanationDiagnostic(rawResult)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func parseExplanationDiagnostic(raw []byte) *ExplanationDiagnostic {
	if len(raw) == 0 {
		return nil
	}
	var result struct {
		Explanations json.RawMessage `json:"explanations"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || len(result.Explanations) == 0 || string(result.Explanations) == "null" {
		return nil
	}
	var aggregate struct {
		AIGenerated    json.RawMessage `json:"aiGenerated"`
		Deterministic  json.RawMessage `json:"deterministic"`
		None           json.RawMessage `json:"none"`
		FailureReasons json.RawMessage `json:"failureReasons"`
	}
	if err := json.Unmarshal(result.Explanations, &aggregate); err != nil {
		return nil
	}
	aiGenerated, ok := nonnegativeJSONInteger(aggregate.AIGenerated)
	if !ok {
		return nil
	}
	deterministic, ok := nonnegativeJSONInteger(aggregate.Deterministic)
	if !ok {
		return nil
	}
	none, ok := nonnegativeJSONInteger(aggregate.None)
	if !ok {
		return nil
	}
	var rawReasons map[string]json.RawMessage
	if len(aggregate.FailureReasons) == 0 || string(aggregate.FailureReasons) == "null" || json.Unmarshal(aggregate.FailureReasons, &rawReasons) != nil {
		return nil
	}
	reasons := make(map[string]int)
	for _, reason := range explanationFailureReasonAllowlist {
		rawCount, exists := rawReasons[reason]
		if !exists {
			continue
		}
		count, valid := nonnegativeJSONInteger(rawCount)
		if !valid {
			return nil
		}
		reasons[reason] = count
	}
	return &ExplanationDiagnostic{AIGenerated: aiGenerated, Deterministic: deterministic, None: none, FailureReasons: reasons}
}

func nonnegativeJSONInteger(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	if value < 0 {
		return 0, true
	}
	return value, true
}

func (s *SQLDataService) Health(ctx context.Context) HealthReport {
	now := s.now().UTC()
	result := HealthReport{Status: "ok", Components: map[string]ComponentHealth{}}
	db := ComponentHealth{Status: "ok", CheckedAt: now}
	if err := s.store.Pool.Ping(ctx); err != nil {
		db.Status = "unavailable"
		db.Message = "database unavailable"
		result.Status = "degraded"
	}
	result.Components["database"] = db
	result.Components["openai"] = ComponentHealth{Status: map[bool]string{true: "configured", false: "disabled"}[s.aiEnabled], CheckedAt: now}
	result.Components["smtp"] = ComponentHealth{Status: map[bool]string{true: "configured", false: "disabled"}[s.smtpConfigured], CheckedAt: now}
	if s.discovery != nil {
		diagnostics, err := s.discovery.Diagnostics(ctx)
		if err != nil {
			result.Status = "degraded"
			diagnostics = catalog.DiscoveryDiagnostics{PresentationStatus: catalog.PresentationStatus{Status: "degraded"}, ActiveBySource: map[string]int64{}, Reason: "diagnostics_unavailable"}
		}
		result.CatalogDiscovery = &diagnostics
	}
	if s.searchDiagnostics != nil {
		diagnostics, err := s.searchDiagnostics.Diagnostics(ctx)
		if err != nil {
			result.Status = "degraded"
			diagnostics = catalog.UnavailableSearchDiagnostics()
		}
		result.CatalogSearch = &diagnostics
	}
	var steamFresh, aiFresh, emailFresh *time.Time
	steamErr := s.store.Pool.QueryRow(ctx, `SELECT max(last_successful_fetch_at) FROM user_source_states`).Scan(&steamFresh)
	aiErr := s.store.Pool.QueryRow(ctx, `SELECT max(last_successful_fetch_at) FROM game_embeddings`).Scan(&aiFresh)
	emailErr := s.store.Pool.QueryRow(ctx, `SELECT max(accepted_at) FROM email_deliveries WHERE status='sent'`).Scan(&emailFresh)
	result.Components["steam"] = ComponentHealth{Status: freshnessStatus(steamFresh), CheckedAt: now, FreshAt: steamFresh}
	if steamErr != nil {
		result.Components["steam"] = ComponentHealth{Status: "unavailable", CheckedAt: now, Message: "freshness unavailable"}
		result.Status = "degraded"
	}
	if s.aiEnabled {
		c := result.Components["openai"]
		c.FreshAt = aiFresh
		if aiErr != nil {
			c.Status = "unavailable"
			c.Message = "freshness unavailable"
			result.Status = "degraded"
		} else if aiFresh != nil {
			c.Status = freshnessStatus(aiFresh)
		}
		result.Components["openai"] = c
	}
	if s.smtpConfigured {
		c := result.Components["smtp"]
		c.FreshAt = emailFresh
		if emailErr != nil {
			c.Status = "unavailable"
			c.Message = "freshness unavailable"
			result.Status = "degraded"
		} else if emailFresh != nil {
			c.Status = freshnessStatus(emailFresh)
		}
		result.Components["smtp"] = c
	}
	var queued, failed int
	if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE status IN ('queued','running','waiting','retryable')),count(*) FILTER(WHERE status='dead') FROM sync_jobs`).Scan(&queued, &failed); err != nil {
		result.Components["jobs"] = ComponentHealth{Status: "unavailable", CheckedAt: now, Message: "job status unavailable"}
		result.Status = "degraded"
	} else {
		status := "ok"
		message := ""
		if failed > 0 {
			status = "degraded"
			message = "one or more jobs need attention"
			result.Status = "degraded"
		}
		if queued > 0 && status == "ok" {
			status = "active"
		}
		result.Components["jobs"] = ComponentHealth{Status: status, CheckedAt: now, Message: message}
	}
	return result
}

func freshnessStatus(value *time.Time) string {
	if value == nil {
		return "unknown"
	}
	if time.Since(*value) > 24*time.Hour {
		return "stale"
	}
	return "ok"
}

var diagnosticSensitive = regexp.MustCompile(`(?i)(prompt|instruction|api[_ -]?key|authorization|bearer|secret|token|cookie|session|password|steamid)`)

func SanitizeDiagnosticError(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	if diagnosticSensitive.MatchString(raw) {
		return "upstream request failed"
	}
	lower := strings.ToLower(raw)
	for _, label := range []string{"timeout", "deadline", "rate limit", "unavailable", "refused", "invalid response", "lease expired"} {
		if strings.Contains(lower, label) {
			return label
		}
	}
	return "operation failed"
}

func deleteData(service DataService, authService *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if err := service.DeleteUserData(r.Context(), session.UserID); err != nil {
			http.Error(w, "data deletion failed", http.StatusInternalServerError)
			return
		}
		authService.ClearSessionCookie(w)
		w.WriteHeader(http.StatusNoContent)
	}
}

func listJobs(service DataService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := auth.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		items, err := service.Jobs(r.Context(), session.UserID)
		if err != nil {
			http.Error(w, "job diagnostics unavailable", http.StatusInternalServerError)
			return
		}
		for index := range items {
			items[index].Error = SanitizeDiagnosticError(items[index].Error)
		}
		writeJSON(w, items)
	}
}
