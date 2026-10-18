package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/observability"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

func TestWorkflowProgress(t *testing.T) {
	job := func(stage, status string) dbgen.SyncJob {
		return dbgen.SyncJob{Stage: stage, Status: status}
	}
	root := job("steam", "running")
	base := []dbgen.SyncJob{job("catalog", "queued"), job("achievements", "queued")}
	tests := []struct {
		name     string
		root     dbgen.SyncJob
		children []dbgen.SyncJob
		want     int
	}{
		{name: "initial active", root: root, want: 0},
		{name: "partial import discovery", root: root, children: base[:1], want: 0},
		{name: "steam import", root: root, children: base, want: 20},
		{name: "catalog", root: root, children: []dbgen.SyncJob{job("catalog", "succeeded"), job("achievements", "queued")}, want: 40},
		{name: "achievements", root: root, children: []dbgen.SyncJob{job("catalog", "succeeded"), job("achievements", "succeeded")}, want: 60},
		{name: "fractional AI", root: root, children: []dbgen.SyncJob{job("catalog", "succeeded"), job("achievements", "succeeded"), job("ai_features", "succeeded"), job("ai_features", "running")}, want: 70},
		{name: "profile creation completes AI", root: root, children: []dbgen.SyncJob{job("catalog", "succeeded"), job("achievements", "succeeded"), job("ai_features", "succeeded"), job("profile", "queued")}, want: 80},
		{name: "profile", root: root, children: []dbgen.SyncJob{job("catalog", "succeeded"), job("achievements", "succeeded"), job("profile", "succeeded")}, want: 90},
		{name: "recommendation creation completes profile", root: root, children: []dbgen.SyncJob{job("catalog", "succeeded"), job("achievements", "succeeded"), job("profile", "queued"), job("recommendations", "queued")}, want: 90},
		{name: "recommendations", root: root, children: []dbgen.SyncJob{job("catalog", "succeeded"), job("achievements", "succeeded"), job("recommendations", "succeeded")}, want: 100},
		{name: "succeeded root", root: job("steam", "succeeded"), want: 100},
		{name: "dead required stage does not count", root: root, children: []dbgen.SyncJob{job("catalog", "dead"), job("achievements", "succeeded")}, want: 40},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := workflowProgress(test.root, test.children); got != test.want {
				t.Fatalf("workflowProgress() = %d, want %d", got, test.want)
			}
		})
	}

	completedSteam := []dbgen.SyncJob{job("catalog", "succeeded"), job("achievements", "succeeded")}
	withAIQueued := append(append([]dbgen.SyncJob{}, completedSteam...), job("ai_features", "queued"))
	withAITerminal := append(append([]dbgen.SyncJob{}, completedSteam...), job("ai_features", "succeeded"))
	withProfile := append(append([]dbgen.SyncJob{}, withAITerminal...), job("profile", "queued"))
	withRecommendations := append(append([]dbgen.SyncJob{}, withProfile...), job("recommendations", "queued"))
	previous := workflowProgress(root, completedSteam)
	for _, children := range [][]dbgen.SyncJob{withAIQueued, withAITerminal, withProfile, withRecommendations} {
		current := workflowProgress(root, children)
		if current < previous {
			t.Fatalf("progress decreased after child creation: %d -> %d", previous, current)
		}
		previous = current
	}
}

func TestServiceGetCurrentStatus(t *testing.T) {
	t.Run("active root returns ownership-checked status", func(t *testing.T) {
		service, _, db, userID := newTestService(t)
		jobID, err := service.Start(context.Background(), userID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(context.Background(), `INSERT INTO sync_jobs (id,kind,idempotency_key,parent_job_id,stage) VALUES
			($1,'catalog.refresh',$2,$3,'catalog'),
			($4,'achievements.sync',$5,$3,'achievements')`, uuid.New(), "current-status-catalog", jobID, uuid.New(), "current-status-achievements"); err != nil {
			t.Fatal(err)
		}
		status, found, err := service.GetCurrentStatus(context.Background(), userID)
		if err != nil || !found {
			t.Fatalf("GetCurrentStatus() found=%v err=%v", found, err)
		}
		if status.ID != jobID || status.Status != "queued" || status.Progress != 20 {
			t.Fatalf("status = %#v, want queued job %s at 20%%", status, jobID)
		}
		stages := make(map[string]bool)
		for _, stage := range status.Stages {
			stages[stage.Stage] = true
		}
		for _, want := range []string{"steam", "catalog", "achievements", "profile", "owned_games"} {
			if !stages[want] {
				t.Fatalf("full status omitted stage %q: %#v", want, status.Stages)
			}
		}
	})

	t.Run("no active root", func(t *testing.T) {
		service, _, _, userID := newTestService(t)
		status, found, err := service.GetCurrentStatus(context.Background(), userID)
		if err != nil || found || status.ID != uuid.Nil || status.Status != "" || len(status.Stages) != 0 {
			t.Fatalf("GetCurrentStatus() status=%#v found=%v err=%v", status, found, err)
		}
	})

	t.Run("another user cannot discover the active root", func(t *testing.T) {
		service, _, _, ownerID := newTestService(t)
		if _, err := service.Start(context.Background(), ownerID); err != nil {
			t.Fatal(err)
		}
		status, found, err := service.GetCurrentStatus(context.Background(), uuid.New())
		if err != nil || found || status.ID != uuid.Nil || status.Status != "" || len(status.Stages) != 0 {
			t.Fatalf("GetCurrentStatus() status=%#v found=%v err=%v", status, found, err)
		}
	})
}

func TestSourceLogsAreCorrelatedAndAggregateAchievements(t *testing.T) {
	var logs bytes.Buffer
	service, fake, _, userID := newTestService(t, WithLogger(observability.New(slog.New(slog.NewJSONHandler(&logs, nil)))))
	fake.mu.Lock()
	fake.owned = append(fake.owned, steam.OwnedGame{AppID: 11, Name: "Second Game"})
	fake.mu.Unlock()
	correlation := observability.Correlation{WorkflowID: uuid.New(), JobID: uuid.New(), ParentJobID: uuid.New()}
	if err := service.Run(observability.WithCorrelation(context.Background(), correlation), userID); err != nil {
		t.Fatal(err)
	}
	records := sourceLogRecords(t, logs.String())
	for _, source := range []string{"owned_games", "recently_played", "wishlist", "achievements"} {
		var stages []string
		for _, record := range records {
			if record["source"] == source {
				stages = append(stages, record["stage"].(string))
				if record["workflow_id"] != correlation.WorkflowID.String() || record["job_id"] != correlation.JobID.String() || record["parent_job_id"] != correlation.ParentJobID.String() {
					t.Fatalf("source %s correlation=%#v", source, record)
				}
			}
		}
		if strings.Join(stages, ",") != "started,finished" {
			t.Fatalf("source %s stages=%v logs=%s", source, stages, logs.String())
		}
	}
	for _, record := range records {
		if record["source"] == "achievements" && record["stage"] == "finished" {
			if record["total_count"] != float64(2) || record["available_count"] != float64(2) || record["unavailable_count"] != float64(0) || record["failed_count"] != float64(0) {
				t.Fatalf("achievement aggregate=%#v", record)
			}
			return
		}
	}
	t.Fatal("achievement finished event missing")
}

func TestSteamDiagnosticUsesOnlyReviewedCodesAndBoundedStatuses(t *testing.T) {
	for _, test := range []struct {
		name  string
		state steam.Availability
		err   error
		want  steamDiagnostic
	}{
		{name: "success", state: steam.Available, want: steamDiagnostic{}},
		{name: "private", state: steam.Unavailable, err: steam.ErrUnavailable, want: steamDiagnostic{reason: observability.ReasonUpstreamUnavailable}},
		{name: "timeout", state: steam.RequestFailed, err: context.DeadlineExceeded, want: steamDiagnostic{reason: observability.ReasonTimeout}},
		{name: "rate limited", state: steam.RequestFailed, err: &steam.RequestError{Operation: steam.OwnedGamesOperation, StatusCode: http.StatusTooManyRequests}, want: steamDiagnostic{reason: observability.ReasonRateLimited, upstreamStatus: http.StatusTooManyRequests}},
		{name: "request timeout", state: steam.RequestFailed, err: &steam.RequestError{Operation: steam.OwnedGamesOperation, StatusCode: http.StatusRequestTimeout}, want: steamDiagnostic{reason: observability.ReasonTimeout, upstreamStatus: http.StatusRequestTimeout}},
		{name: "non-retryable rejection", state: steam.RequestFailed, err: &steam.RequestError{Operation: steam.OwnedGamesOperation, StatusCode: http.StatusForbidden}, want: steamDiagnostic{reason: observability.ReasonUpstreamRejected, upstreamStatus: http.StatusForbidden}},
		{name: "server failure", state: steam.RequestFailed, err: &steam.RequestError{Operation: steam.OwnedGamesOperation, StatusCode: http.StatusNotImplemented}, want: steamDiagnostic{reason: observability.ReasonUpstreamUnavailable, upstreamStatus: http.StatusNotImplemented}},
		{name: "temporary transport failure", state: steam.RequestFailed, err: &steam.RequestError{Operation: steam.OwnedGamesOperation, Temporary: true}, want: steamDiagnostic{reason: observability.ReasonUpstreamUnavailable}},
		{name: "canceled temporary request", state: steam.RequestFailed, err: errors.Join(context.Canceled, &steam.RequestError{Operation: steam.OwnedGamesOperation, Temporary: true}), want: steamDiagnostic{reason: observability.ReasonInternalError}},
		{name: "malformed", state: steam.RequestFailed, err: steam.ErrInvalidResponse, want: steamDiagnostic{reason: observability.ReasonMalformedResponse}},
		{name: "unknown", state: steam.RequestFailed, err: errors.New("raw-canary"), want: steamDiagnostic{reason: observability.ReasonInternalError}},
		{name: "unsafe low status", state: steam.RequestFailed, err: &steam.RequestError{Operation: steam.OwnedGamesOperation, StatusCode: 399}, want: steamDiagnostic{reason: observability.ReasonInternalError}},
		{name: "unsafe high status", state: steam.RequestFailed, err: &steam.RequestError{Operation: steam.OwnedGamesOperation, StatusCode: 600}, want: steamDiagnostic{reason: observability.ReasonInternalError}},
		{name: "missing error", state: steam.RequestFailed, want: steamDiagnostic{reason: observability.ReasonInternalError}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := diagnoseSteamFailure(test.state, test.err); got != test.want {
				t.Fatalf("diagnostic=%#v want=%#v", got, test.want)
			}
		})
	}
}

func TestSourceLogPersistenceFailureAndCanariesAreBounded(t *testing.T) {
	var logs bytes.Buffer
	service, fake, db, userID := newTestService(t, WithLogger(observability.New(slog.New(slog.NewJSONHandler(&logs, nil)))))
	fake.mu.Lock()
	fake.owned[0].Name = "game-name-canary"
	fake.owned[0].AppID = 987654321
	fake.ownedErr = errors.New("upstream-raw-error-canary")
	fake.ownedAvailability = steam.RequestFailed
	fake.summary.DisplayName = "display-name-canary"
	fake.summary.AvatarURL = "https://avatars.cloudflare.steamstatic.com/avatar-canary.jpg"
	fake.mu.Unlock()
	if _, err := db.Pool.Exec(context.Background(), `DROP TABLE user_source_states CASCADE`); err != nil {
		t.Fatal(err)
	}
	if err := service.importSteam(context.Background(), userID); err == nil {
		t.Fatal("importSteam succeeded after source-state table removal")
	}
	records := sourceLogRecords(t, logs.String())
	if len(records) != 2 || records[1]["stage"] != "finished" || records[1]["reason"] != "persistence_failed" {
		t.Fatalf("records=%#v", records)
	}
	for _, canary := range []string{"76561197960287930", userID.String(), "987654321", "game-name-canary", "display-name-canary", "avatar-canary", "upstream-raw-error-canary"} {
		if strings.Contains(logs.String(), canary) {
			t.Fatalf("canary %q leaked: %s", canary, logs.String())
		}
	}
}

func TestSourceFailureSpansUseBoundedReasonsAndFields(t *testing.T) {
	var logs bytes.Buffer
	service, fake, _, userID := newTestService(t, WithLogger(observability.New(slog.New(slog.NewJSONHandler(&logs, nil)))))
	fake.mu.Lock()
	fake.ownedAvailability, fake.ownedErr = steam.RequestFailed, context.DeadlineExceeded
	fake.recentAvailability, fake.recentErr = steam.RequestFailed, &steam.RequestError{Operation: steam.RecentlyPlayedOperation, StatusCode: http.StatusTooManyRequests}
	fake.wishlistAvailability, fake.wishlistErr = steam.RequestFailed, steam.ErrInvalidResponse
	fake.mu.Unlock()
	if err := service.importSteam(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	records := sourceLogRecords(t, logs.String())
	wantReasons := map[string]string{"owned_games": "timeout", "recently_played": "rate_limited", "wishlist": "malformed_response"}
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "event": true, "stage": true, "source": true, "availability": true, "reason": true, "upstream_status": true, "duration_ms": true, "total_count": true, "available_count": true, "unavailable_count": true, "failed_count": true}
	for source, wantReason := range wantReasons {
		var matching []map[string]any
		for _, record := range records {
			if record["source"] == source {
				matching = append(matching, record)
				for key := range record {
					if !allowed[key] {
						t.Fatalf("source %s unexpected field %q in %#v", source, key, record)
					}
				}
			}
		}
		if len(matching) != 2 || matching[0]["stage"] != "started" || matching[1]["stage"] != "finished" || matching[1]["availability"] != "request_failed" || matching[1]["reason"] != wantReason {
			t.Fatalf("source %s records=%#v", source, matching)
		}
		if source == "recently_played" && matching[1]["upstream_status"] != float64(http.StatusTooManyRequests) {
			t.Fatalf("source %s upstream status=%v", source, matching[1]["upstream_status"])
		}
	}
}

func TestOwnedSourceSpanUnavailableAndUnknownFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		state  steam.Availability
		err    error
		reason string
	}{
		{name: "unavailable", state: steam.Unavailable, err: steam.ErrUnavailable, reason: "upstream_unavailable"},
		{name: "unknown", state: steam.RequestFailed, err: errors.New("unknown-error-canary"), reason: "internal_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			service, fake, _, userID := newTestService(t, WithLogger(observability.New(slog.New(slog.NewJSONHandler(&logs, nil)))))
			fake.mu.Lock()
			fake.ownedAvailability, fake.ownedErr = test.state, test.err
			fake.mu.Unlock()
			if err := service.importSteam(context.Background(), userID); err != nil {
				t.Fatal(err)
			}
			var owned []map[string]any
			for _, record := range sourceLogRecords(t, logs.String()) {
				if record["source"] == "owned_games" {
					owned = append(owned, record)
				}
			}
			if len(owned) != 2 || owned[1]["reason"] != test.reason {
				t.Fatalf("owned records=%#v", owned)
			}
			if strings.Contains(logs.String(), "unknown-error-canary") {
				t.Fatalf("raw error leaked: %s", logs.String())
			}
		})
	}
}

func TestCompleteSourceLogCanaryPreservesLastKnownGoodState(t *testing.T) {
	const ownedAppID int64 = 987654321
	const wishlistAppID int64 = 987654322
	var logs bytes.Buffer
	service, fake, db, userID := newTestService(t, WithLogger(observability.New(slog.New(slog.NewJSONHandler(&logs, nil)))))
	if _, err := db.Pool.Exec(context.Background(), `UPDATE steam_accounts SET steam_id='steam-id-canary' WHERE user_id=$1`, userID); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.summary = steam.PlayerSummary{Visibility: steam.VisibilityPublic, DisplayName: "display-name-canary", AvatarURL: "https://avatars.cloudflare.steamstatic.com/avatar-canary.jpg"}
	fake.owned = []steam.OwnedGame{{AppID: ownedAppID, Name: "owned-game-canary", PlaytimeForever: 600}}
	fake.recent = []steam.RecentGame{{AppID: ownedAppID, Name: "recent-game-canary", PlaytimeForever: 600, PlaytimeTwoWeeks: 30}}
	fake.wishlist = []steam.WishlistItem{{AppID: wishlistAppID, Priority: 1}}
	fake.details[wishlistAppID] = steam.GameDetails{AppID: wishlistAppID, Type: "game", Name: "wishlist-game-canary", ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "Coming Soon"}}
	fake.mu.Unlock()
	if _, err := service.Preflight(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var accountBefore, achievementBefore []byte
	if err := db.Pool.QueryRow(context.Background(), `SELECT raw_snapshot FROM steam_accounts WHERE user_id=$1`, userID).Scan(&accountBefore); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(context.Background(), `SELECT raw_snapshot FROM achievement_summaries WHERE user_id=$1`, userID).Scan(&achievementBefore); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.ownedAvailability, fake.ownedErr = steam.RequestFailed, errors.New("owned-error-canary")
	fake.recentAvailability, fake.recentErr = steam.RequestFailed, errors.New("recent-error-canary")
	fake.wishlistAvailability, fake.wishlistErr = steam.RequestFailed, errors.New("wishlist-error-canary")
	fake.achievementAvailability[ownedAppID] = steam.RequestFailed
	fake.achievementErrors[ownedAppID] = errors.New("achievement-error-canary")
	fake.mu.Unlock()
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var accountAfter, achievementAfter []byte
	if err := db.Pool.QueryRow(context.Background(), `SELECT raw_snapshot FROM steam_accounts WHERE user_id=$1`, userID).Scan(&accountAfter); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(context.Background(), `SELECT raw_snapshot FROM achievement_summaries WHERE user_id=$1`, userID).Scan(&achievementAfter); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(accountBefore, accountAfter) || !bytes.Equal(achievementBefore, achievementAfter) {
		t.Fatal("upstream failure replaced a last-known-good snapshot")
	}
	records := sourceLogRecords(t, logs.String())
	if len(records) != 16 {
		t.Fatalf("source event count=%d want=16 logs=%s", len(records), logs.String())
	}
	for _, canary := range []string{"steam-id-canary", userID.String(), "987654321", "987654322", "display-name-canary", "avatar-canary", "owned-game-canary", "recent-game-canary", "wishlist-game-canary", "owned-error-canary", "recent-error-canary", "wishlist-error-canary", "achievement-error-canary"} {
		if strings.Contains(logs.String(), canary) {
			t.Fatalf("canary %q leaked: %s", canary, logs.String())
		}
	}
}

func TestAchievementLogAggregatesUnavailableAndFailedTargets(t *testing.T) {
	var logs bytes.Buffer
	service, fake, _, userID := newTestService(t, WithLogger(observability.New(slog.New(slog.NewJSONHandler(&logs, nil)))))
	fake.mu.Lock()
	fake.owned = append(fake.owned, steam.OwnedGame{AppID: 11, Name: "Second"})
	fake.mu.Unlock()
	if err := service.importSteam(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	fake.mu.Lock()
	fake.achievementAvailability[10] = steam.Unavailable
	fake.achievementErrors[10] = steam.ErrUnavailable
	fake.achievementAvailability[11] = steam.RequestFailed
	fake.achievementErrors[11] = errors.New("achievement-error-canary")
	fake.mu.Unlock()
	if err := service.syncAchievements(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	records := sourceLogRecords(t, logs.String())
	if len(records) != 2 {
		t.Fatalf("achievement records=%#v", records)
	}
	finished := records[1]
	if finished["source"] != "achievements" || finished["stage"] != "finished" || finished["reason"] != "internal_error" || finished["total_count"] != float64(2) || finished["available_count"] != float64(0) || finished["unavailable_count"] != float64(1) || finished["failed_count"] != float64(1) {
		t.Fatalf("achievement aggregate=%#v", finished)
	}
	if strings.Contains(logs.String(), "achievement-error-canary") || strings.Contains(logs.String(), "app_id") || strings.Contains(logs.String(), "game_name") {
		t.Fatalf("per-game or raw error data leaked: %s", logs.String())
	}
}

func TestAchievementLogPropagatesSelectedSafeDiagnosticWithoutIdentifiers(t *testing.T) {
	var logs bytes.Buffer
	service, fake, _, userID := newTestService(t, WithLogger(observability.New(slog.New(slog.NewJSONHandler(&logs, nil)))))
	fake.mu.Lock()
	fake.owned = append(fake.owned, steam.OwnedGame{AppID: 987654321, Name: "achievement-game-canary"})
	fake.mu.Unlock()
	if err := service.importSteam(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	fake.mu.Lock()
	fake.achievementAvailability[987654321] = steam.RequestFailed
	fake.achievementErrors[987654321] = &steam.RequestError{Operation: steam.AchievementsOperation, StatusCode: http.StatusForbidden}
	fake.mu.Unlock()
	if err := service.syncAchievements(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	records := sourceLogRecords(t, logs.String())
	if len(records) != 2 {
		t.Fatalf("achievement records=%#v", records)
	}
	finished := records[1]
	if finished["reason"] != "upstream_rejected" || finished["upstream_status"] != float64(http.StatusForbidden) {
		t.Fatalf("achievement diagnostic=%#v", finished)
	}
	for _, canary := range []string{"987654321", "achievement-game-canary", "app_id", "game_name", "request failed"} {
		if strings.Contains(logs.String(), canary) {
			t.Fatalf("canary %q leaked: %s", canary, logs.String())
		}
	}
}

func TestAchievementNotModifiedPreservesAggregateSuccessAndSelectedFailure(t *testing.T) {
	for _, test := range []struct {
		name               string
		configure          func(*fakeSteam)
		wantAvailability   string
		wantAvailable      float64
		wantFailed         float64
		wantReason         any
		wantUpstreamStatus any
	}{
		{
			name: "all unchanged remains successful",
			configure: func(fake *fakeSteam) {
				fake.achievementAvailability[10] = steam.NotModified
				fake.achievementErrors[10] = steam.ErrNotModified
			},
			wantAvailability: "available",
			wantAvailable:    1,
			wantFailed:       0,
		},
		{
			name: "later unchanged target does not erase failure",
			configure: func(fake *fakeSteam) {
				fake.achievementAvailability[10] = steam.RequestFailed
				fake.achievementErrors[10] = &steam.RequestError{Operation: steam.AchievementsOperation, StatusCode: http.StatusTooManyRequests}
				fake.achievementAvailability[11] = steam.NotModified
				fake.achievementErrors[11] = steam.ErrNotModified
			},
			wantAvailability:   "request_failed",
			wantAvailable:      1,
			wantFailed:         1,
			wantReason:         "rate_limited",
			wantUpstreamStatus: float64(http.StatusTooManyRequests),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			service, fake, _, userID := newTestService(t, WithLogger(observability.New(slog.New(slog.NewJSONHandler(&logs, nil)))))
			if test.wantAvailable == 1 && test.wantFailed == 1 {
				fake.mu.Lock()
				fake.owned = append(fake.owned, steam.OwnedGame{AppID: 11, Name: "Second"})
				fake.mu.Unlock()
			}
			if err := service.importSteam(context.Background(), userID); err != nil {
				t.Fatal(err)
			}
			logs.Reset()
			fake.mu.Lock()
			test.configure(fake)
			fake.mu.Unlock()
			if err := service.syncAchievements(context.Background(), userID); err != nil {
				t.Fatal(err)
			}
			finished := sourceLogRecords(t, logs.String())[1]
			if finished["availability"] != test.wantAvailability || finished["available_count"] != test.wantAvailable || finished["failed_count"] != test.wantFailed || finished["reason"] != test.wantReason || finished["upstream_status"] != test.wantUpstreamStatus {
				t.Fatalf("achievement aggregate=%#v", finished)
			}
		})
	}
}

func sourceLogRecords(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["event"] == "sync_source" {
			records = append(records, record)
		}
	}
	return records
}

func TestSyncIsIdempotent(t *testing.T) {
	service, _, db, userID := newTestService(t)
	first, err := service.Start(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Start(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("active sync IDs differ: %s != %s", first, second)
	}
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"user_games": 1, "wishlist_entries": 1, "achievement_summaries": 1} {
		var got int
		if err := db.Pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s count = %d, want %d", table, got, want)
		}
	}
}

func TestPreflight(t *testing.T) {
	t.Run("public profile and non-empty owned games permit sync and refresh identity", func(t *testing.T) {
		service, fake, db, userID := newTestService(t)
		result, err := service.Preflight(context.Background(), userID)
		if err != nil || !result.CanSync || result.ProfileVisibility != steam.VisibilityPublic || result.GameDetailsVisibility != steam.VisibilityPublic || result.Reason != nil {
			t.Fatalf("result = %#v, err = %v", result, err)
		}
		if result.DisplayName == nil || *result.DisplayName != "Player" || result.AvatarURL == nil || *result.AvatarURL == "" {
			t.Fatalf("identity = %#v/%#v", result.DisplayName, result.AvatarURL)
		}
		var displayName, avatarURL string
		if err := db.Pool.QueryRow(context.Background(), `SELECT display_name,avatar_url FROM steam_accounts WHERE user_id=$1`, userID).Scan(&displayName, &avatarURL); err != nil {
			t.Fatal(err)
		}
		if displayName != *result.DisplayName || avatarURL != *result.AvatarURL {
			t.Fatalf("stored identity = %q/%q", displayName, avatarURL)
		}
		if fake.probeOwnedCalls != 1 || fake.ownedCalls != 0 {
			t.Fatalf("probe/import calls = %d/%d, want 1/0", fake.probeOwnedCalls, fake.ownedCalls)
		}
	})

	t.Run("private profile blocks without probing optional or owned sources", func(t *testing.T) {
		service, fake, _, userID := newTestService(t)
		fake.summary.Visibility = steam.VisibilityPrivate
		result, err := service.Preflight(context.Background(), userID)
		if err != nil || result.CanSync || result.ProfileVisibility != steam.VisibilityPrivate || result.GameDetailsVisibility != steam.VisibilityUnknown || result.Reason == nil || *result.Reason != "profile_private" {
			t.Fatalf("result = %#v, err = %v", result, err)
		}
		if fake.probeOwnedCalls != 0 || fake.ownedCalls != 0 || fake.recentCalls != 0 || fake.wishlistCalls != 0 {
			t.Fatalf("calls probe/owned/recent/wishlist = %d/%d/%d/%d", fake.probeOwnedCalls, fake.ownedCalls, fake.recentCalls, fake.wishlistCalls)
		}
	})

	for _, test := range []struct {
		name         string
		availability steam.Availability
		games        []steam.OwnedGame
	}{
		{name: "unavailable", availability: steam.Unavailable},
		{name: "ambiguous empty", availability: steam.Available, games: []steam.OwnedGame{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, fake, _, userID := newTestService(t)
			fake.ownedAvailability, fake.owned = test.availability, test.games
			result, err := service.Preflight(context.Background(), userID)
			if err != nil || result.CanSync || result.GameDetailsVisibility != steam.VisibilityUnknown || result.Reason == nil || *result.Reason != "game_details_unavailable" {
				t.Fatalf("result = %#v, err = %v", result, err)
			}
		})
	}

	t.Run("upstream failures are failed checks and preserve stored identity", func(t *testing.T) {
		service, fake, db, userID := newTestService(t)
		if _, err := db.Pool.Exec(context.Background(), `UPDATE steam_accounts SET display_name='Saved',avatar_url='https://avatars.cloudflare.steamstatic.com/saved.jpg' WHERE user_id=$1`, userID); err != nil {
			t.Fatal(err)
		}
		fake.summaryErr = errors.New("canary upstream payload")
		result, err := service.Preflight(context.Background(), userID)
		if !errors.Is(err, ErrPreflightFailed) || result.CanSync || result.ProfileVisibility != steam.VisibilityUnknown || result.Reason == nil || *result.Reason != "check_failed" {
			t.Fatalf("result = %#v, err = %v", result, err)
		}
		if result.DisplayName == nil || *result.DisplayName != "Saved" || result.AvatarURL == nil || !strings.Contains(*result.AvatarURL, "saved.jpg") {
			t.Fatalf("last-known identity = %#v/%#v", result.DisplayName, result.AvatarURL)
		}
	})

	t.Run("empty refreshed avatar preserves last-known avatar", func(t *testing.T) {
		service, fake, db, userID := newTestService(t)
		const savedAvatar = "https://avatars.cloudflare.steamstatic.com/saved.jpg"
		if _, err := db.Pool.Exec(context.Background(), `UPDATE steam_accounts SET display_name='Saved',avatar_url=$2 WHERE user_id=$1`, userID, savedAvatar); err != nil {
			t.Fatal(err)
		}
		fake.summary = steam.PlayerSummary{Visibility: steam.VisibilityPublic, DisplayName: "Refreshed", AvatarURL: ""}
		result, err := service.Preflight(context.Background(), userID)
		if err != nil || !result.CanSync || result.DisplayName == nil || *result.DisplayName != "Refreshed" || result.AvatarURL == nil || *result.AvatarURL != savedAvatar {
			t.Fatalf("result = %#v, err = %v", result, err)
		}
		var displayName, avatarURL string
		if err := db.Pool.QueryRow(context.Background(), `SELECT display_name,avatar_url FROM steam_accounts WHERE user_id=$1`, userID).Scan(&displayName, &avatarURL); err != nil {
			t.Fatal(err)
		}
		if displayName != "Refreshed" || avatarURL != savedAvatar {
			t.Fatalf("stored identity = %q/%q", displayName, avatarURL)
		}
	})

	t.Run("empty refreshed display name preserves last-known name", func(t *testing.T) {
		service, fake, db, userID := newTestService(t)
		if _, err := db.Pool.Exec(context.Background(), `UPDATE steam_accounts SET display_name='Saved' WHERE user_id=$1`, userID); err != nil {
			t.Fatal(err)
		}
		fake.summary = steam.PlayerSummary{Visibility: steam.VisibilityPublic, DisplayName: "", AvatarURL: "https://avatars.cloudflare.steamstatic.com/refreshed.jpg"}
		result, err := service.Preflight(context.Background(), userID)
		if err != nil || !result.CanSync || result.DisplayName == nil || *result.DisplayName != "Saved" {
			t.Fatalf("result=%#v err=%v", result, err)
		}
		var displayName string
		if err := db.Pool.QueryRow(context.Background(), `SELECT display_name FROM steam_accounts WHERE user_id=$1`, userID).Scan(&displayName); err != nil || displayName != "Saved" {
			t.Fatalf("stored display name=%q err=%v", displayName, err)
		}
	})

	t.Run("invalid user ID fails before upstream access", func(t *testing.T) {
		service, fake, _, _ := newTestService(t)
		if _, err := service.Preflight(context.Background(), uuid.Nil); err == nil || errors.Is(err, ErrPreflightFailed) {
			t.Fatalf("error = %v", err)
		}
		if fake.summaryCalls != 0 {
			t.Fatalf("summary calls = %d", fake.summaryCalls)
		}
	})
}

func TestPreflightStartThenImportUsesProbeBeforeOwnedGames(t *testing.T) {
	service, fake, db, userID := newTestService(t)

	if _, err := service.Preflight(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Start(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	if fake.probeOwnedCalls != 2 || fake.ownedCalls != 0 {
		t.Fatalf("before import probe/owned calls = %d/%d, want 2/0", fake.probeOwnedCalls, fake.ownedCalls)
	}

	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	if fake.probeOwnedCalls != 2 || fake.ownedCalls != 1 {
		t.Fatalf("after import probe/owned calls = %d/%d, want 2/1", fake.probeOwnedCalls, fake.ownedCalls)
	}
	var count int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM user_games WHERE user_id=$1`, userID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("imported user games = %d, want 1", count)
	}
}

func TestStartRequiresPreflight(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*fakeSteam)
		want  error
	}{
		{name: "private", setup: func(fake *fakeSteam) { fake.summary.Visibility = steam.VisibilityPrivate }, want: ErrPreflightBlocked},
		{name: "ambiguous", setup: func(fake *fakeSteam) { fake.owned = []steam.OwnedGame{} }, want: ErrPreflightBlocked},
		{name: "failed", setup: func(fake *fakeSteam) { fake.ownedErr = errors.New("upstream failed") }, want: ErrPreflightFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, fake, db, userID := newTestService(t)
			test.setup(fake)
			if _, err := service.Start(context.Background(), userID); !errors.Is(err, test.want) {
				t.Fatalf("Start error = %v, want %v", err, test.want)
			}
			var count int
			if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM sync_jobs WHERE kind='steam.sync'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("steam.sync jobs = %d", count)
			}
			if fake.recentCalls != 0 || fake.wishlistCalls != 0 {
				t.Fatalf("optional source calls = %d/%d", fake.recentCalls, fake.wishlistCalls)
			}
		})
	}
}

func TestSyncAddsWishlistGameToCatalog(t *testing.T) {
	service, _, db, userID := newTestService(t)
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM games g JOIN wishlist_entries w ON w.game_id=g.id WHERE w.user_id=$1 AND g.steam_app_id=20`, userID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("wishlist catalog rows = %d, want 1", count)
	}
}

func TestSyncPreservesSnapshotWhenProfileBecomesPrivate(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := db.Pool.QueryRow(context.Background(), `SELECT ug.playtime_minutes FROM user_games ug JOIN games g ON g.id=ug.game_id WHERE ug.user_id=$1 AND g.steam_app_id=10`, userID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.ownedAvailability, fake.recentAvailability, fake.wishlistAvailability = steam.Unavailable, steam.Unavailable, steam.Unavailable
	fake.owned, fake.recent, fake.wishlist = nil, nil, nil
	fake.mu.Unlock()
	service.now = func() time.Time { return time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC) }
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var after int64
	var availability string
	if err := db.Pool.QueryRow(context.Background(), `SELECT ug.playtime_minutes FROM user_games ug JOIN games g ON g.id=ug.game_id WHERE ug.user_id=$1 AND g.steam_app_id=10`, userID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(context.Background(), `SELECT availability FROM steam_accounts WHERE user_id=$1`, userID).Scan(&availability); err != nil {
		t.Fatal(err)
	}
	if after != before || availability != string(steam.Unavailable) {
		t.Fatalf("snapshot after private = %d/%s, want %d/unavailable", after, availability, before)
	}
	var lastAttempt, lastSuccess time.Time
	if err := db.Pool.QueryRow(context.Background(), `SELECT last_attempt_at,last_successful_fetch_at FROM user_source_states WHERE user_id=$1 AND source='owned_games'`, userID).Scan(&lastAttempt, &lastSuccess); err != nil {
		t.Fatal(err)
	}
	if !lastAttempt.After(lastSuccess) {
		t.Fatalf("private state freshness = attempt %s, success %s", lastAttempt, lastSuccess)
	}
	if _, err := service.Start(context.Background(), userID); !errors.Is(err, ErrPreflightBlocked) {
		t.Fatalf("Start error = %v, want ErrPreflightBlocked", err)
	}
}

func TestCatalogFailurePreservesLastGoodSnapshot(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := db.Pool.QueryRow(context.Background(), `SELECT source_snapshot FROM games WHERE steam_app_id=20`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.detailErrors[20] = steam.ErrUnavailable
	fake.mu.Unlock()
	service.now = func() time.Time { return time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC) }
	if err := service.catalog.RefreshKnown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var after []byte
	var availability string
	if err := db.Pool.QueryRow(context.Background(), `SELECT source_snapshot,availability FROM games WHERE steam_app_id=20`).Scan(&after, &availability); err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || availability != string(steam.Unavailable) {
		t.Fatalf("catalog fallback changed snapshot or state: availability=%s", availability)
	}
}

func TestReleaseChangeAppendsHistory(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var gameID uuid.UUID
	if err := db.Pool.QueryRow(context.Background(), `SELECT id FROM games WHERE steam_app_id=30`).Scan(&gameID); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM game_release_dates WHERE game_id=$1`, gameID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	details := fake.details[30]
	details.ReleaseDate.Raw = "Dec 2026"
	fake.details[30] = details
	fake.mu.Unlock()
	if err := service.catalog.RefreshKnown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM game_release_dates WHERE game_id=$1`, gameID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Fatalf("history count = %d, want %d", after, before+1)
	}
	if err := service.catalog.RefreshKnown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var repeated int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM game_release_dates WHERE game_id=$1`, gameID).Scan(&repeated); err != nil {
		t.Fatal(err)
	}
	if repeated != after {
		t.Fatalf("unchanged refresh appended history: count = %d, want %d", repeated, after)
	}
}

func TestReleasedWishlistGameIsNotCatalogMember(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	fake.mu.Lock()
	details := fake.details[20]
	details.ReleaseDate = steam.ReleaseDate{ComingSoon: false, Raw: "1 Jan, 2020"}
	fake.details[20] = details
	fake.mu.Unlock()
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM catalog_entries ce JOIN games g ON g.id=ce.game_id WHERE g.steam_app_id=20`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("released wishlist catalog memberships = %d, want 0", count)
	}
}

func TestWorkerRunsEnqueuedWorkflowToRequiredChildCompletion(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	service.SetAIJobPlanner(testAIPlanner{})
	worker := jobs.NewWorker(service.queue, "sync-test", jobs.WithPollInterval(5*time.Millisecond), jobs.WithLeaseDuration(time.Second))
	if err := service.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	if err := worker.Register("ai.features", func(context.Context, jobs.Lease) error { return nil }); err != nil {
		t.Fatal(err)
	}
	jobID, err := service.Start(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		var aiStatus *string
		if err := db.Pool.QueryRow(context.Background(), `SELECT status FROM sync_jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		_ = db.Pool.QueryRow(context.Background(), `SELECT status FROM sync_jobs WHERE kind='ai.features' AND parent_job_id IS NULL`).Scan(&aiStatus)
		if status == "succeeded" && aiStatus != nil && *aiStatus == "succeeded" {
			break
		}
		if status == "dead" {
			t.Fatal("workflow dead-lettered")
		}
		if time.Now().After(deadline) {
			t.Fatalf("workflow/AI status = %s/%v after timeout", status, aiStatus)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var children, succeeded int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*),count(*) FILTER (WHERE status='succeeded') FROM sync_jobs WHERE parent_job_id=$1`, jobID).Scan(&children, &succeeded); err != nil {
		t.Fatal(err)
	}
	if children != 2 || succeeded != 2 {
		t.Fatalf("child jobs = %d/%d succeeded, want 2/2", succeeded, children)
	}
	var aiStatus string
	if err := db.Pool.QueryRow(context.Background(), `SELECT status FROM sync_jobs WHERE kind='ai.features' AND parent_job_id IS NULL`).Scan(&aiStatus); err != nil {
		t.Fatal(err)
	}
	if aiStatus != "succeeded" {
		t.Fatalf("independent AI job status = %s", aiStatus)
	}
	fake.mu.Lock()
	ownedCalls := fake.ownedCalls
	probeOwnedCalls := fake.probeOwnedCalls
	fake.mu.Unlock()
	if probeOwnedCalls != 1 || ownedCalls != 1 {
		t.Fatalf("parent calls probe/import = %d/%d, want 1/1", probeOwnedCalls, ownedCalls)
	}
}

func TestAIFailureDoesNotFailSteamSynchronization(t *testing.T) {
	service, _, db, userID := newTestService(t)
	service.SetAIJobPlanner(failingAIPlanner{})
	worker := jobs.NewWorker(service.queue, "ai-failure-test", jobs.WithPollInterval(5*time.Millisecond), jobs.WithLeaseDuration(time.Second), jobs.WithRetryBase(time.Millisecond))
	if err := service.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	if err := worker.Register("ai.features", func(context.Context, jobs.Lease) error { return errors.New("simulated refusal") }); err != nil {
		t.Fatal(err)
	}
	jobID, err := service.Start(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var steamStatus string
		var aiStatus *string
		if err := db.Pool.QueryRow(context.Background(), `SELECT status FROM sync_jobs WHERE id=$1`, jobID).Scan(&steamStatus); err != nil {
			t.Fatal(err)
		}
		_ = db.Pool.QueryRow(context.Background(), `SELECT status FROM sync_jobs WHERE kind='ai.features' AND parent_job_id IS NULL`).Scan(&aiStatus)
		if steamStatus == "succeeded" && aiStatus != nil && *aiStatus == "dead" {
			break
		}
		if steamStatus == "dead" || time.Now().After(deadline) {
			t.Fatalf("steam/AI status = %s/%v", steamStatus, aiStatus)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type testAIPlanner struct{}

func (testAIPlanner) PlanFeatureJobs(_ context.Context, _ uuid.UUID, rootKey string) ([]jobs.Spec, error) {
	return []jobs.Spec{{Kind: "ai.features", Stage: "ai_features", Required: true, IdempotencyKey: rootKey + ":ai.features:v1:test", Payload: []byte(`{"version":1}`), MaxAttempts: 3}}, nil
}

type failingAIPlanner struct{}

func (failingAIPlanner) PlanFeatureJobs(_ context.Context, _ uuid.UUID, rootKey string) ([]jobs.Spec, error) {
	return []jobs.Spec{{Kind: "ai.features", Stage: "ai_features", IdempotencyKey: rootKey + ":ai.features:v1:failure", Payload: []byte(`{"version":1}`), MaxAttempts: 1}}, nil
}

func TestWorkerLeavesReleasedWishlistGameOutsideCatalog(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	fake.mu.Lock()
	details := fake.details[20]
	details.ReleaseDate = steam.ReleaseDate{ComingSoon: false, Raw: "1 Jan, 2020"}
	fake.details[20] = details
	fake.mu.Unlock()
	worker := jobs.NewWorker(service.queue, "released-test", jobs.WithPollInterval(5*time.Millisecond), jobs.WithLeaseDuration(time.Second))
	if err := service.RegisterHandlers(worker); err != nil {
		t.Fatal(err)
	}
	jobID, err := service.Start(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		if err := db.Pool.QueryRow(context.Background(), `SELECT status FROM sync_jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workflow status = %s after timeout", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM catalog_entries ce JOIN games g ON g.id=ce.game_id WHERE g.steam_app_id=20`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("released wishlist catalog memberships after worker = %d, want 0", count)
	}
}

func TestKnownReleasedWishlistGameStaysExcludedWhenRefreshFails(t *testing.T) {
	service, fake, db, userID := newTestService(t)
	fake.mu.Lock()
	details := fake.details[20]
	details.ReleaseDate = steam.ReleaseDate{ComingSoon: false, Raw: "1 Jan, 2020"}
	fake.details[20] = details
	fake.mu.Unlock()
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.detailErrors[20] = errors.New("temporary")
	fake.mu.Unlock()
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM catalog_entries ce JOIN games g ON g.id=ce.game_id WHERE g.steam_app_id=20`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("known released wishlist membership after failed refresh = %d, want 0", count)
	}
}

func TestSyncStatusExposesCatalogDegradation(t *testing.T) {
	service, fake, _, userID := newTestService(t)
	if err := service.Run(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.detailErrors[20] = steam.ErrUnavailable
	fake.mu.Unlock()
	if err := service.catalog.RefreshKnown(context.Background()); err != nil {
		t.Fatal(err)
	}
	jobID, err := service.Start(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.GetStatus(context.Background(), userID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range status.Stages {
		if stage.Stage == "catalog:20" && stage.Status == "degraded" && stage.Availability == string(steam.Unavailable) {
			return
		}
	}
	t.Fatalf("catalog degradation missing from status: %#v", status.Stages)
}

type fakeSteam struct {
	mu                                                          sync.Mutex
	owned                                                       []steam.OwnedGame
	recent                                                      []steam.RecentGame
	wishlist                                                    []steam.WishlistItem
	ownedAvailability, recentAvailability, wishlistAvailability steam.Availability
	details                                                     map[int64]steam.GameDetails
	detailErrors                                                map[int64]error
	achievementAvailability                                     map[int64]steam.Availability
	achievementErrors                                           map[int64]error
	achievementSchemas                                          map[int64]steam.AchievementSchema
	achievementSchemaAvailability                               map[int64]steam.Availability
	achievementSchemaErrors                                     map[int64]error
	ownedCalls, probeOwnedCalls                                 int
	summary                                                     steam.PlayerSummary
	summaryErr, ownedErr, recentErr, wishlistErr                error
	summaryCalls, recentCalls, wishlistCalls                    int
}

func (f *fakeSteam) PlayerSummary(context.Context, string) (steam.PlayerSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summaryCalls++
	return f.summary, f.summaryErr
}

func (f *fakeSteam) OwnedGames(context.Context, string) ([]steam.OwnedGame, steam.Availability, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ownedCalls++
	return append([]steam.OwnedGame(nil), f.owned...), f.ownedAvailability, f.ownedErr
}
func (f *fakeSteam) ProbeOwnedGames(context.Context, string) ([]steam.OwnedGame, steam.Availability, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeOwnedCalls++
	return append([]steam.OwnedGame(nil), f.owned...), f.ownedAvailability, f.ownedErr
}
func (f *fakeSteam) RecentlyPlayed(context.Context, string) ([]steam.RecentGame, steam.Availability, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recentCalls++
	return append([]steam.RecentGame(nil), f.recent...), f.recentAvailability, f.recentErr
}
func (f *fakeSteam) Wishlist(context.Context, string) ([]steam.WishlistItem, steam.Availability, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wishlistCalls++
	return append([]steam.WishlistItem(nil), f.wishlist...), f.wishlistAvailability, f.wishlistErr
}
func (f *fakeSteam) Achievements(_ context.Context, _ string, appID int64) (steam.AchievementSummary, steam.Availability, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := f.achievementAvailability[appID]
	if state == "" {
		state = steam.Available
	}
	return steam.AchievementSummary{AppID: appID, GameName: "Game", Total: 10, Unlocked: 5}, state, f.achievementErrors[appID]
}
func (f *fakeSteam) AchievementSchema(_ context.Context, appID int64) (steam.AchievementSchema, steam.Availability, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := f.achievementSchemaAvailability[appID]
	if state == "" {
		state = steam.Available
	}
	return f.achievementSchemas[appID], state, f.achievementSchemaErrors[appID]
}
func (f *fakeSteam) GameDetails(_ context.Context, appID int64) (steam.GameDetails, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.detailErrors[appID]; err != nil {
		return steam.GameDetails{}, err
	}
	if d, ok := f.details[appID]; ok {
		return d, nil
	}
	return steam.GameDetails{}, steam.ErrUnavailable
}

func newTestService(t *testing.T, options ...ServiceOption) (*Service, *fakeSteam, *store.Store, uuid.UUID) {
	t.Helper()
	db := openSyncStore(t)
	ctx := context.Background()
	userID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO steam_accounts(user_id,steam_id) VALUES($1,'76561197960287930')`, userID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	fake := &fakeSteam{summary: steam.PlayerSummary{Visibility: steam.VisibilityPublic, DisplayName: "Player", AvatarURL: "https://avatars.cloudflare.steamstatic.com/avatar.jpg"}, owned: []steam.OwnedGame{{AppID: 10, Name: "Owned", PlaytimeForever: 600, LastPlayedAt: now}}, recent: []steam.RecentGame{{AppID: 10, Name: "Owned", PlaytimeForever: 600, PlaytimeTwoWeeks: 20, LastPlayedAt: now}}, wishlist: []steam.WishlistItem{{AppID: 20, Priority: 1}}, ownedAvailability: steam.Available, recentAvailability: steam.Available, wishlistAvailability: steam.Available, detailErrors: map[int64]error{}, achievementAvailability: map[int64]steam.Availability{}, achievementErrors: map[int64]error{}, details: map[int64]steam.GameDetails{
		10: {AppID: 10, Type: "game", Name: "Owned", ReleaseDate: steam.ReleaseDate{Raw: "1 Jan, 2020"}},
		20: {AppID: 20, Type: "game", Name: "Wish", ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "Nov 2026"}},
		30: {AppID: 30, Type: "game", Name: "Seed", ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "Nov 2026"}},
	}}
	cat, err := catalog.NewService(db, fake, catalog.Seed{Version: 1, ObservedDate: "2026-10-17", Entries: []catalog.SeedEntry{{AppID: 30, ReleaseWindow: "Nov 2026", SourceURL: "https://store.steampowered.com/app/30/", ObservedDate: "2026-10-17"}}})
	if err != nil {
		t.Fatal(err)
	}
	queue := jobs.NewQueue(db)
	service, err := NewService(db, queue, fake, cat, options...)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	return service, fake, db, userID
}

func openSyncStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	isolated := fmt.Sprintf("%s%ssearch_path=%s%%2Cpublic", baseURL, separator, schema)
	db, err := store.Open(context.Background(), isolated)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanup, err := pgx.Connect(context.Background(), baseURL)
		if err == nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	return db
}
