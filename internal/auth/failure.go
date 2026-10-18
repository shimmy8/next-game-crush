package auth

import "errors"

// Diagnostic is a deliberately small, non-sensitive classification suitable
// for structured logs. It must never contain values supplied by a browser,
// Steam, a database, or an underlying error.
type Diagnostic struct {
	Stage          string
	Reason         string
	Infrastructure bool
}

type diagnosticError struct {
	diagnostic Diagnostic
	cause      error
}

func (e *diagnosticError) Error() string { return e.cause.Error() }
func (e *diagnosticError) Unwrap() error { return e.cause }

func failure(cause error, stage, reason string, infrastructure bool) error {
	if cause == nil {
		cause = errors.New("authentication failed")
	}
	return &diagnosticError{
		diagnostic: Diagnostic{Stage: stage, Reason: reason, Infrastructure: infrastructure},
		cause:      cause,
	}
}

// Diagnose maps an authentication error to a safe, bounded classification.
// Unknown errors deliberately collapse to one generic infrastructure failure.
func Diagnose(err error) Diagnostic {
	var classified *diagnosticError
	if errors.As(err, &classified) {
		return classified.diagnostic
	}
	return Diagnostic{Stage: "internal", Reason: "internal_failure", Infrastructure: true}
}
