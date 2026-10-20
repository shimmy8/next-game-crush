package app

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type appRunnerFunc func(context.Context) error

func (fn appRunnerFunc) Run(ctx context.Context) error { return fn(ctx) }

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

func TestAppStopsWhenEitherWorkerFails(t *testing.T) {
	for _, failing := range []string{"interactive", "catalog"} {
		t.Run(failing, func(t *testing.T) {
			want := errors.New(failing + " failed")
			serverDone := make(chan struct{})
			waiter := appRunnerFunc(func(ctx context.Context) error { <-ctx.Done(); return nil })
			failer := appRunnerFunc(func(context.Context) error { close(serverDone); return want })
			app := &App{server: nil, scheduler: waiter, serve: func() error { <-serverDone; return http.ErrServerClosed }}
			if failing == "interactive" {
				app.interactiveWorker, app.catalogWorker = failer, waiter
			} else {
				app.interactiveWorker, app.catalogWorker = waiter, failer
			}
			if err := app.Run(context.Background()); !errors.Is(err, want) {
				t.Fatalf("Run() error=%v, want %v", err, want)
			}
		})
	}
}

func TestAppRunsHTTPWithoutStartingJobsWhenDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	httpReady := make(chan struct{})
	runnerStarted := make(chan string, 3)
	jobRunner := func(name string) appRunnerFunc {
		return func(ctx context.Context) error {
			runnerStarted <- name
			<-ctx.Done()
			return nil
		}
	}
	application := &App{
		server:            nil,
		jobsDisabled:      true,
		interactiveWorker: jobRunner("interactive worker"),
		catalogWorker:     jobRunner("catalog worker"),
		scheduler:         jobRunner("catalog scheduler"),
		serve: func() error {
			close(httpReady)
			<-ctx.Done()
			return http.ErrServerClosed
		},
	}
	result := make(chan error, 1)
	go func() { result <- application.Run(ctx) }()

	select {
	case <-httpReady:
	case <-time.After(time.Second):
		t.Fatal("HTTP server did not start")
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not stop after cancellation")
	}
	select {
	case name := <-runnerStarted:
		t.Fatalf("%s started while jobs were disabled", name)
	default:
	}
}

func TestStartupCatchupDoesNotBlockHTTP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	schedulerStarted := make(chan struct{})
	schedulerRelease := make(chan struct{})
	httpReady := make(chan struct{})
	serverDone := make(chan struct{})
	waiter := appRunnerFunc(func(ctx context.Context) error { <-ctx.Done(); return nil })
	app := &App{
		server:            nil,
		interactiveWorker: waiter,
		catalogWorker:     waiter,
		scheduler: appRunnerFunc(func(context.Context) error {
			close(schedulerStarted)
			<-schedulerRelease
			return nil
		}),
		serve: func() error {
			close(httpReady)
			<-serverDone
			return http.ErrServerClosed
		},
	}
	result := make(chan error, 1)
	go func() { result <- app.Run(ctx) }()
	select {
	case <-schedulerStarted:
	case <-time.After(time.Second):
		t.Fatal("scheduler catch-up did not start")
	}
	select {
	case <-httpReady:
		// HTTP starts independently while startup catch-up is still blocked.
	case <-time.After(time.Second):
		t.Fatal("HTTP readiness was blocked by scheduler catch-up")
	}
	close(schedulerRelease)
	close(serverDone)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
