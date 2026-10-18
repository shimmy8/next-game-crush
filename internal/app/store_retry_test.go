package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type retryableStoreOpenError struct{}

func (retryableStoreOpenError) Error() string     { return "database is not ready" }
func (retryableStoreOpenError) SafeToRetry() bool { return true }

func TestWaitForDatabaseEventuallySucceeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	attempts := 0
	err := waitForDatabase(ctx, time.Millisecond, func(context.Context) error {
		attempts++
		if attempts < 3 {
			return retryableStoreOpenError{}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("waitForDatabase() error = %v", err)
	}
	if attempts != 3 {
		t.Fatalf("waitForDatabase() attempts = %d, want 3", attempts)
	}
}

func TestWaitForDatabaseStopsOnNonRetryableError(t *testing.T) {
	wantErr := errors.New("invalid migration")
	attempts := 0
	err := waitForDatabase(context.Background(), time.Millisecond, func(context.Context) error {
		attempts++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("waitForDatabase() error = %v, want %v", err, wantErr)
	}
	if attempts != 1 {
		t.Fatalf("waitForDatabase() attempts = %d, want 1", attempts)
	}
}

func TestWaitForDatabaseDoesNotRetryPostgresConfigurationError(t *testing.T) {
	wantErr := &pgconn.PgError{Code: "28P01", Message: "authentication failed"}
	attempts := 0
	err := waitForDatabase(context.Background(), time.Millisecond, func(context.Context) error {
		attempts++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("waitForDatabase() error = %v, want %v", err, wantErr)
	}
	if attempts != 1 {
		t.Fatalf("waitForDatabase() attempts = %d, want 1", attempts)
	}
}

func TestWaitForDatabaseRetriesPostgresStartupError(t *testing.T) {
	attempts := 0
	err := waitForDatabase(context.Background(), time.Millisecond, func(context.Context) error {
		attempts++
		if attempts == 1 {
			return &pgconn.PgError{Code: "57P03", Message: "database system is starting up"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("waitForDatabase() error = %v", err)
	}
	if attempts != 2 {
		t.Fatalf("waitForDatabase() attempts = %d, want 2", attempts)
	}
}

func TestWaitForDatabaseHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForDatabase(ctx, time.Hour, func(context.Context) error {
		return retryableStoreOpenError{}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForDatabase() error = %v, want context cancellation", err)
	}
}

func TestWaitForDatabaseRecognizesPGXConnectionFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := waitForDatabase(ctx, 5*time.Millisecond, func(ctx context.Context) error {
		connection, connectErr := pgx.Connect(ctx, "postgres://fixture:fixture@127.0.0.1:1/fixture?sslmode=disable")
		if connectErr != nil {
			return connectErr
		}
		return connection.Close(ctx)
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForDatabase() error = %v, want retry until context deadline", err)
	}
}
