package jobs

import "time"

// RetryHint carries a provider delay without conflating a missing/malformed
// header with a valid delay. Workers with a retry schedule honor positive hints.
type RetryHint interface {
	RetryAfter() (time.Duration, bool)
}

type RetryJitter func(time.Duration) time.Duration

type catalogRetryError struct {
	cause      error
	reason     string
	retryAfter time.Duration
}

// NewCatalogRetryError preserves the original error for classification while
// exposing only an allowlisted reason to persistence and diagnostics.
func NewCatalogRetryError(cause error, safeReason string, retryAfter *time.Duration) error {
	switch safeReason {
	case "unavailable", "request_failed", "invalid_response", "rate_limited", "retry_exhausted", "malformed":
	default:
		safeReason = "request_failed"
	}
	err := &catalogRetryError{cause: cause, reason: safeReason}
	if retryAfter != nil && *retryAfter > 0 {
		err.retryAfter = *retryAfter
	}
	return err
}

func (e *catalogRetryError) Error() string                     { return e.reason }
func (e *catalogRetryError) SafeReason() string                { return e.reason }
func (e *catalogRetryError) Unwrap() error                     { return e.cause }
func (e *catalogRetryError) Retryable() bool                   { return true }
func (e *catalogRetryError) RetryAfter() (time.Duration, bool) { return e.retryAfter, e.retryAfter > 0 }
