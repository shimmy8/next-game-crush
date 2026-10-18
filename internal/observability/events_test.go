package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLoggerSanitizesValuesClampsDurationsAndSelectsLevels(t *testing.T) {
	var output bytes.Buffer
	logger := New(slog.New(slog.NewJSONHandler(&output, nil)))
	correlation := Correlation{WorkflowID: uuid.New(), JobID: uuid.New(), ParentJobID: uuid.New()}

	logger.SyncRequest(context.Background(), SyncRequestEvent{Outcome: "hostile-canary", RequestID: "request-canary", HTTPStatus: 999, Duration: 25 * time.Hour, Reason: "reason-canary"})
	logger.JobAttempt(context.Background(), JobAttemptEvent{Stage: JobDead, Correlation: correlation, Kind: "kind-canary", LogicalStage: "stage-canary", Attempt: 2, MaxAttempts: 2, Required: true, Duration: -time.Second, RetryDelay: 25 * time.Hour, Reason: "reason-canary"})
	logger.SyncSource(context.Background(), SyncSourceEvent{Stage: "source-stage-canary", Correlation: correlation, Source: "source-canary", Availability: "availability-canary", Reason: "reason-canary", Duration: 25 * time.Hour})
	logger.Worker(context.Background(), WorkerEvent{Stage: "worker-stage-canary", Correlation: correlation, Reason: "reason-canary"})

	events := decodeEvents(t, output.String())
	if got := events[0]["outcome"]; got != "unknown" {
		t.Fatalf("request outcome = %v", got)
	}
	if got := events[0]["http_status"]; got != float64(500) {
		t.Fatalf("HTTP status = %v", got)
	}
	if got := events[0]["duration_ms"]; got != float64((24*time.Hour)/time.Millisecond) {
		t.Fatalf("duration = %v", got)
	}
	if got := events[0]["reason"]; got != "internal_error" {
		t.Fatalf("request reason = %v", got)
	}
	if got := events[1]["level"]; got != "ERROR" {
		t.Fatalf("required dead level = %v", got)
	}
	if got := events[1]["duration_ms"]; got != float64(0) {
		t.Fatalf("negative duration = %v", got)
	}
	if got := events[1]["retry_delay_ms"]; got != float64((24*time.Hour)/time.Millisecond) {
		t.Fatalf("retry delay = %v", got)
	}
	for _, event := range events {
		encoded, _ := json.Marshal(event)
		if strings.Contains(string(encoded), "canary") {
			t.Fatalf("untrusted value leaked: %s", encoded)
		}
	}
}

func TestQueueLaneDiagnosticsAreAllowlisted(t *testing.T) {
	for _, lane := range []string{"interactive", "catalog", "lane-canary"} {
		var output bytes.Buffer
		logger := New(slog.New(slog.NewJSONHandler(&output, nil)))
		logger.JobAttempt(context.Background(), JobAttemptEvent{Stage: JobStarted, Lane: lane})
		logger.Worker(context.Background(), WorkerEvent{Stage: WorkerStarted, Lane: lane})
		want := lane
		if lane == "lane-canary" {
			want = "unknown"
		}
		for _, event := range decodeEvents(t, output.String()) {
			if event["lane"] != want {
				t.Fatalf("lane=%v, want %s", event["lane"], want)
			}
		}
		if strings.Contains(output.String(), "canary") {
			t.Fatal("untrusted lane leaked")
		}
	}
}

func TestLoggerEmitsOnlyDeclaredFields(t *testing.T) {
	var output bytes.Buffer
	logger := New(slog.New(slog.NewJSONHandler(&output, nil)))
	correlation := Correlation{WorkflowID: uuid.New(), JobID: uuid.New(), ParentJobID: uuid.New()}
	logger.JobAttempt(context.Background(), JobAttemptEvent{Stage: JobRetryScheduled, Correlation: correlation, Kind: "steam.sync", LogicalStage: "steam", Attempt: 1, MaxAttempts: 3, Required: true, Duration: time.Second, RetryDelay: 2 * time.Second, Reason: ReasonTimeout})

	event := decodeEvents(t, output.String())[0]
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "event": true, "stage": true, "lane": true, "workflow_id": true, "job_id": true, "parent_job_id": true, "kind": true, "logical_stage": true, "attempt": true, "max_attempts": true, "required": true, "duration_ms": true, "retry_delay_ms": true, "reason": true}
	for key := range event {
		if !allowed[key] {
			t.Fatalf("unexpected field %q in %#v", key, event)
		}
	}
	if event["event"] != "job_attempt" || event["level"] != "WARN" {
		t.Fatalf("event = %#v", event)
	}
}

func TestLoggerAllowsSafeAIReasons(t *testing.T) {
	for _, reason := range []Reason{"invalid_evidence", "invalid_output"} {
		t.Run(string(reason), func(t *testing.T) {
			var output bytes.Buffer
			logger := New(slog.New(slog.NewJSONHandler(&output, nil)))
			logger.JobAttempt(context.Background(), JobAttemptEvent{Stage: JobRetryScheduled, Kind: "ai.features", LogicalStage: "ai_features", Reason: reason})

			event := decodeEvents(t, output.String())[0]
			if got := event["reason"]; got != string(reason) {
				t.Fatalf("reason = %v, want %q", got, reason)
			}
		})
	}
}

func TestAchievementClassificationJobObservabilityIsAllowlistedAndSanitized(t *testing.T) {
	var output bytes.Buffer
	logger := New(slog.New(slog.NewJSONHandler(&output, nil)))
	logger.JobAttempt(context.Background(), JobAttemptEvent{Stage: JobRetryScheduled, Kind: "achievement.classify", LogicalStage: "achievement_classification", Attempt: 1, MaxAttempts: 3, Reason: ReasonInvalidOutput})
	event := decodeEvents(t, output.String())[0]
	if event["kind"] != "achievement.classify" || event["logical_stage"] != "achievement_classification" || event["reason"] != "invalid_output" {
		t.Fatalf("event=%#v", event)
	}
	encoded, _ := json.Marshal(event)
	for _, canary := range []string{"steam_app_id", "api_name", "description", "prompt", "provider_body"} {
		if strings.Contains(string(encoded), canary) {
			t.Fatalf("sensitive classification field leaked: %s", encoded)
		}
	}
}

func TestCompletionProjectionJobObservabilityIsAllowlistedAndPrivate(t *testing.T) {
	var output bytes.Buffer
	logger := New(slog.New(slog.NewJSONHandler(&output, nil)))
	logger.JobAttempt(context.Background(), JobAttemptEvent{Stage: JobRetryScheduled, Kind: "completion.project", LogicalStage: "completion_projection", Attempt: 1, MaxAttempts: 3, Reason: ReasonPersistenceFailed})
	event := decodeEvents(t, output.String())[0]
	if event["kind"] != "completion.project" || event["logical_stage"] != "completion_projection" || event["reason"] != "persistence_failed" {
		t.Fatalf("event=%#v", event)
	}
	encoded, _ := json.Marshal(event)
	for _, canary := range []string{"steam_id", "user_id", "game_id", "app_id", "api_name", "achievement_description", "payload", "completion_weight", "provider_body"} {
		if strings.Contains(string(encoded), canary) {
			t.Fatalf("sensitive completion projection field leaked: %s", encoded)
		}
	}
}

func TestSyncSourceLogsOnlyBoundedUpstreamStatusOnFinishedEvents(t *testing.T) {
	for _, test := range []struct {
		name       string
		stage      SourceStage
		status     int
		wantStatus bool
	}{
		{name: "minimum safe status", stage: SourceFinished, status: 400, wantStatus: true},
		{name: "maximum safe status", stage: SourceFinished, status: 599, wantStatus: true},
		{name: "success status", stage: SourceFinished, status: 200},
		{name: "below range", stage: SourceFinished, status: 399},
		{name: "above range", stage: SourceFinished, status: 600},
		{name: "started event", stage: SourceStarted, status: 429},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := New(slog.New(slog.NewJSONHandler(&output, nil)))
			logger.SyncSource(context.Background(), SyncSourceEvent{Stage: test.stage, Source: "owned_games", UpstreamStatus: test.status})

			event := decodeEvents(t, output.String())[0]
			got, ok := event["upstream_status"]
			if ok != test.wantStatus {
				t.Fatalf("upstream_status present=%v, want %v: %#v", ok, test.wantStatus, event)
			}
			if test.wantStatus && got != float64(test.status) {
				t.Fatalf("upstream_status=%v, want %d", got, test.status)
			}
		})
	}
}

func TestCorrelationContextRoundTrip(t *testing.T) {
	want := Correlation{WorkflowID: uuid.New(), JobID: uuid.New(), ParentJobID: uuid.New()}
	if got := CorrelationFromContext(WithCorrelation(context.Background(), want)); got != want {
		t.Fatalf("correlation = %#v, want %#v", got, want)
	}
	if got := CorrelationFromContext(nil); got != (Correlation{}) {
		t.Fatalf("nil context correlation = %#v", got)
	}
}

func TestNilLoggerAndNilSinkDoNotPanic(t *testing.T) {
	New(nil).Worker(context.Background(), WorkerEvent{Stage: WorkerStarted})
	var logger *Logger
	logger.JobAttempt(context.Background(), JobAttemptEvent{})
}

func TestPublicEventsCannotCarryRawErrorsPayloadsOrAttributes(t *testing.T) {
	for _, value := range []any{SyncRequestEvent{}, JobAttemptEvent{}, SyncSourceEvent{}, WorkerEvent{}, CatalogDiscoveryEvent{}} {
		typeOf := reflect.TypeOf(value)
		for i := 0; i < typeOf.NumField(); i++ {
			field := typeOf.Field(i)
			name := strings.ToLower(field.Name)
			if strings.Contains(name, "error") || strings.Contains(name, "payload") || field.Type == reflect.TypeOf(slog.Attr{}) {
				t.Fatalf("%s exposes unsafe field %s", typeOf.Name(), field.Name)
			}
		}
	}
}

func TestDiscoveryLogsOmitSensitiveInputs(t *testing.T) {
	var output bytes.Buffer
	logger := New(slog.New(slog.NewJSONHandler(&output, nil)))
	logger.CatalogDiscovery(context.Background(), CatalogDiscoveryEvent{Source: "steam-key-secret", Mode: "raw-payload", Availability: "request_failed", Discovered: 2, Evaluated: 1, Failed: 1, Reason: ReasonUpstreamUnavailable})
	raw := output.String()
	for _, secret := range []string{"steam-key-secret", "raw-payload"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("sensitive discovery value escaped: %s", raw)
		}
	}
	event := decodeEvents(t, raw)[0]
	if event["source"] != "unknown" || event["mode"] != "unknown" || event["failed"] != float64(1) {
		t.Fatalf("event=%#v", event)
	}
}

func decodeEvents(t *testing.T, raw string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	events := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode log %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}
