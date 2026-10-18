package observability

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

const maxDuration = 24 * time.Hour

type RequestOutcome string

const (
	RequestAccepted RequestOutcome = "accepted"
	RequestFailed   RequestOutcome = "failed"
)

type JobStage string

const (
	JobStarted        JobStage = "started"
	JobHeartbeat      JobStage = "heartbeat"
	JobDeferred       JobStage = "deferred"
	JobSucceeded      JobStage = "succeeded"
	JobRetryScheduled JobStage = "retry_scheduled"
	JobDead           JobStage = "dead"
	JobCancelled      JobStage = "cancelled"
	JobLeaseLost      JobStage = "lease_lost"
)

type SourceStage string

const (
	SourceStarted  SourceStage = "started"
	SourceFinished SourceStage = "finished"
)

type WorkerStage string

const (
	WorkerStarted             WorkerStage = "started"
	WorkerStopped             WorkerStage = "stopped"
	WorkerClaimFailed         WorkerStage = "claim_failed"
	WorkerCompletionFailed    WorkerStage = "completion_failed"
	WorkerFailureRecordFailed WorkerStage = "failure_record_failed"
	WorkerRenewalFailed       WorkerStage = "renewal_failed"
)

type Reason string

const (
	ReasonTimeout             Reason = "timeout"
	ReasonRateLimited         Reason = "rate_limited"
	ReasonUpstreamUnavailable Reason = "upstream_unavailable"
	ReasonUpstreamRejected    Reason = "upstream_rejected"
	ReasonMalformedResponse   Reason = "malformed_response"
	ReasonInvalidEvidence     Reason = "invalid_evidence"
	ReasonInvalidOutput       Reason = "invalid_output"
	ReasonLeaseLost           Reason = "lease_lost"
	ReasonPersistenceFailed   Reason = "persistence_failed"
	ReasonInternalError       Reason = "internal_error"
)

type Correlation struct {
	WorkflowID  uuid.UUID
	JobID       uuid.UUID
	ParentJobID uuid.UUID
}

type SyncRequestEvent struct {
	Outcome    RequestOutcome
	RequestID  string
	WorkflowID uuid.UUID
	HTTPStatus int
	Duration   time.Duration
	Reason     Reason
}

type JobAttemptEvent struct {
	Stage        JobStage
	Correlation  Correlation
	Kind         string
	LogicalStage string
	Attempt      int
	MaxAttempts  int
	Required     bool
	Duration     time.Duration
	RetryDelay   time.Duration
	Reason       Reason
}

type SyncSourceEvent struct {
	Stage            SourceStage
	Correlation      Correlation
	Source           string
	Availability     string
	Reason           Reason
	Duration         time.Duration
	TotalCount       int
	AvailableCount   int
	UnavailableCount int
	FailedCount      int
	UpstreamStatus   int
}

type WorkerEvent struct {
	Stage       WorkerStage
	Correlation Correlation
	Reason      Reason
}

type Logger struct {
	logger *slog.Logger
}

func New(logger *slog.Logger) *Logger {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Logger{logger: logger}
}

type correlationKey struct{}

func WithCorrelation(ctx context.Context, correlation Correlation) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, correlationKey{}, correlation)
}

func CorrelationFromContext(ctx context.Context) Correlation {
	if ctx == nil {
		return Correlation{}
	}
	correlation, _ := ctx.Value(correlationKey{}).(Correlation)
	return correlation
}

func (l *Logger) SyncRequest(ctx context.Context, event SyncRequestEvent) {
	if l == nil || l.logger == nil {
		return
	}
	outcome := allow(string(event.Outcome), requestOutcomes)
	level := slog.LevelInfo
	if outcome != string(RequestAccepted) {
		level = slog.LevelWarn
	}
	status := event.HTTPStatus
	if status < 100 || status > 599 {
		status = 500
	}
	attrs := []slog.Attr{slog.String("event", "sync_request"), slog.String("outcome", outcome)}
	if requestID, err := uuid.Parse(event.RequestID); err == nil {
		attrs = append(attrs, slog.String("request_id", requestID.String()))
	}
	if event.WorkflowID != uuid.Nil {
		attrs = append(attrs, slog.String("workflow_id", event.WorkflowID.String()))
	}
	attrs = append(attrs, slog.Int("http_status", status), durationAttr("duration_ms", event.Duration))
	if reason := safeReason(event.Reason); reason != "" {
		attrs = append(attrs, slog.String("reason", reason))
	}
	l.logger.LogAttrs(safeContext(ctx), level, "sync request", attrs...)
}

func (l *Logger) JobAttempt(ctx context.Context, event JobAttemptEvent) {
	if l == nil || l.logger == nil {
		return
	}
	stage := allow(string(event.Stage), jobStages)
	level := slog.LevelInfo
	switch JobStage(stage) {
	case JobRetryScheduled, JobLeaseLost:
		level = slog.LevelWarn
	case JobDead:
		level = slog.LevelWarn
		if event.Required {
			level = slog.LevelError
		}
	}
	attrs := []slog.Attr{slog.String("event", "job_attempt"), slog.String("stage", stage)}
	attrs = appendCorrelation(attrs, event.Correlation)
	attrs = append(attrs,
		slog.String("kind", allow(event.Kind, jobKinds)),
		slog.String("logical_stage", allow(event.LogicalStage, logicalStages)),
		slog.Int("attempt", nonnegative(event.Attempt)),
		slog.Int("max_attempts", nonnegative(event.MaxAttempts)),
		slog.Bool("required", event.Required),
		durationAttr("duration_ms", event.Duration),
		durationAttr("retry_delay_ms", event.RetryDelay),
	)
	if reason := safeReason(event.Reason); reason != "" {
		attrs = append(attrs, slog.String("reason", reason))
	}
	l.logger.LogAttrs(safeContext(ctx), level, "job attempt", attrs...)
}

func (l *Logger) SyncSource(ctx context.Context, event SyncSourceEvent) {
	if l == nil || l.logger == nil {
		return
	}
	stage := allow(string(event.Stage), sourceStages)
	availability := allow(event.Availability, availabilities)
	level := slog.LevelInfo
	if stage == string(SourceFinished) && availability != "available" && availability != "not_modified" {
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{slog.String("event", "sync_source"), slog.String("stage", stage)}
	attrs = appendCorrelation(attrs, event.Correlation)
	attrs = append(attrs,
		slog.String("source", allow(event.Source, sources)),
		slog.String("availability", availability),
		durationAttr("duration_ms", event.Duration),
		slog.Int("total_count", nonnegative(event.TotalCount)),
		slog.Int("available_count", nonnegative(event.AvailableCount)),
		slog.Int("unavailable_count", nonnegative(event.UnavailableCount)),
		slog.Int("failed_count", nonnegative(event.FailedCount)),
	)
	if reason := safeReason(event.Reason); reason != "" {
		attrs = append(attrs, slog.String("reason", reason))
	}
	if stage == string(SourceFinished) && event.UpstreamStatus >= 400 && event.UpstreamStatus <= 599 {
		attrs = append(attrs, slog.Int("upstream_status", event.UpstreamStatus))
	}
	l.logger.LogAttrs(safeContext(ctx), level, "sync source", attrs...)
}

func (l *Logger) Worker(ctx context.Context, event WorkerEvent) {
	if l == nil || l.logger == nil {
		return
	}
	stage := allow(string(event.Stage), workerStages)
	level := slog.LevelInfo
	if stage != string(WorkerStarted) && stage != string(WorkerStopped) {
		level = slog.LevelError
		if safeReason(event.Reason) == string(ReasonLeaseLost) {
			level = slog.LevelWarn
		}
	}
	attrs := []slog.Attr{slog.String("event", "worker"), slog.String("stage", stage)}
	attrs = appendCorrelation(attrs, event.Correlation)
	if reason := safeReason(event.Reason); reason != "" {
		attrs = append(attrs, slog.String("reason", reason))
	}
	l.logger.LogAttrs(safeContext(ctx), level, "worker lifecycle", attrs...)
}

func appendCorrelation(attrs []slog.Attr, correlation Correlation) []slog.Attr {
	if correlation.WorkflowID != uuid.Nil {
		attrs = append(attrs, slog.String("workflow_id", correlation.WorkflowID.String()))
	}
	if correlation.JobID != uuid.Nil {
		attrs = append(attrs, slog.String("job_id", correlation.JobID.String()))
	}
	if correlation.ParentJobID != uuid.Nil {
		attrs = append(attrs, slog.String("parent_job_id", correlation.ParentJobID.String()))
	}
	return attrs
}

func safeReason(reason Reason) string {
	if reason == "" {
		return ""
	}
	if _, ok := reasons[string(reason)]; ok {
		return string(reason)
	}
	return string(ReasonInternalError)
}

func durationAttr(key string, value time.Duration) slog.Attr {
	if value < 0 {
		value = 0
	}
	if value > maxDuration {
		value = maxDuration
	}
	return slog.Int64(key, value.Milliseconds())
}

func nonnegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func safeContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func allow(value string, allowed map[string]struct{}) string {
	if _, ok := allowed[value]; ok {
		return value
	}
	return "unknown"
}

func values(items ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(items))
	for _, item := range items {
		result[item] = struct{}{}
	}
	return result
}

var requestOutcomes = values("accepted", "failed")
var jobStages = values("started", "heartbeat", "deferred", "succeeded", "retry_scheduled", "dead", "cancelled", "lease_lost")
var sourceStages = values("started", "finished")
var workerStages = values("started", "stopped", "claim_failed", "completion_failed", "failure_record_failed", "renewal_failed")
var reasons = values("timeout", "rate_limited", "upstream_unavailable", "upstream_rejected", "malformed_response", "invalid_evidence", "invalid_output", "lease_lost", "persistence_failed", "internal_error")
var jobKinds = values("steam.sync", "catalog.refresh", "achievements.sync", "ai.features", "ai.embedding", "ai.profile_summary", "profile.rebuild", "recommendations.rebuild", "feedback.classify", "notifications.digest.send")
var logicalStages = values("steam", "catalog", "achievements", "ai_features", "ai_embedding", "profile_summary", "profile", "recommendations", "feedback", "digest")
var sources = values("owned_games", "recently_played", "wishlist", "achievements")
var availabilities = values("available", "unavailable", "request_failed", "not_modified")
