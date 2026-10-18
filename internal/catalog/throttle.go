package catalog

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

const (
	throttleRecoverySuccesses = 4
)

// PersistentHostThrottle shares adaptive catalog slowdown through PostgreSQL.
// It is intentionally attached only to dedicated catalog clients.
type PersistentHostThrottle struct {
	store    *store.Store
	now      func() time.Time
	maxLimit int
}

func NewPersistentHostThrottle(db *store.Store, maxLimit int) (*PersistentHostThrottle, error) {
	if db == nil || maxLimit < 1 || maxLimit > 16 {
		return nil, errors.New("invalid catalog host throttle")
	}
	return &PersistentHostThrottle{store: db, now: time.Now, maxLimit: maxLimit}, nil
}

func validCatalogHost(host string) bool {
	switch strings.ToLower(host) {
	case "store.steampowered.com", "api.steampowered.com", "api.openai.com":
		return true
	default:
		return false
	}
}

func (t *PersistentHostThrottle) ensure(ctx context.Context, host string) (dbgen.CatalogHostThrottle, error) {
	host = strings.ToLower(host)
	if t == nil || t.store == nil || !validCatalogHost(host) {
		return dbgen.CatalogHostThrottle{}, errors.New("invalid catalog throttle host")
	}
	return t.store.Queries.EnsureCatalogHostThrottle(ctx, dbgen.EnsureCatalogHostThrottleParams{Host: host, DefaultLimit: int32(t.maxLimit)})
}

func (t *PersistentHostThrottle) Before(ctx context.Context, host string) error {
	row, err := t.ensure(ctx, host)
	if err != nil {
		return err
	}
	if row.NextEligibleAt.Valid {
		wait := row.NextEligibleAt.Time.Sub(t.now().UTC())
		if wait > 0 {
			return &steam.ThrottleDeferred{At: row.NextEligibleAt.Time.UTC()}
		}
	}
	return nil
}

func (t *PersistentHostThrottle) Limit(host string) int {
	if t == nil || t.store == nil || !validCatalogHost(host) {
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	row, err := t.ensure(ctx, host)
	if err != nil {
		return 1
	}
	return int(row.ConcurrencyLimit)
}

func (t *PersistentHostThrottle) Observe(host string, status int, retryAfter *time.Duration, observedErr error) {
	if t == nil || t.store == nil || !validCatalogHost(host) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	now := t.now().UTC()
	if errors.Is(observedErr, context.Canceled) {
		return
	}
	if status >= 200 && status < 400 && observedErr == nil {
		_, _ = t.ensure(ctx, host)
		_, _ = t.store.Queries.RecordCatalogThrottleSuccess(ctx, dbgen.RecordCatalogThrottleSuccessParams{SuccessesToRecover: throttleRecoverySuccesses, MaxLimit: int32(t.maxLimit), NowAt: pgtype.Timestamptz{Time: now, Valid: true}, Host: strings.ToLower(host)})
		return
	}
	reduce := status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500 && status <= 599
	if !reduce && observedErr != nil {
		var request *steam.RequestError
		reduce = errors.Is(observedErr, context.DeadlineExceeded) || errors.As(observedErr, &request) && request.Temporary
	}
	if !reduce {
		return
	}
	delay := time.Second
	decrement := int32(1)
	if status == http.StatusTooManyRequests {
		decrement = 2
	}
	if retryAfter != nil && *retryAfter > 0 {
		delay = min(*retryAfter, 24*time.Hour)
	}
	_, _ = t.store.Queries.RecordCatalogThrottleFailure(ctx, dbgen.RecordCatalogThrottleFailureParams{Host: strings.ToLower(host), DefaultLimit: int32(t.maxLimit), NextEligibleAt: pgtype.Timestamptz{Time: now.Add(delay), Valid: true}, NowAt: pgtype.Timestamptz{Time: now, Valid: true}, LimitDecrement: decrement})
}

func (t *PersistentHostThrottle) AcquireSlot(ctx context.Context, host string) (string, error) {
	if _, err := t.ensure(ctx, host); err != nil {
		return "", err
	}
	now := t.now().UTC()
	token := uuid.New()
	leaseUntil := now.Add(30 * time.Second)
	if deadline, ok := ctx.Deadline(); ok && deadline.Add(5*time.Second).After(leaseUntil) {
		leaseUntil = deadline.Add(5 * time.Second).UTC()
	}
	tx, err := t.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	q := t.store.Queries.WithTx(tx)
	state, err := q.LockCatalogHostThrottle(ctx, strings.ToLower(host))
	if err != nil {
		return "", err
	}
	if state.NextEligibleAt.Valid && state.NextEligibleAt.Time.After(now) {
		return "", &steam.ThrottleDeferred{At: state.NextEligibleAt.Time.UTC()}
	}
	row, err := q.AcquireCatalogHostSlot(ctx, dbgen.AcquireCatalogHostSlotParams{Host: strings.ToLower(host), NowAt: pgtype.Timestamptz{Time: now, Valid: true}, FencingToken: catalogUUID(token), LeaseUntil: pgtype.Timestamptz{Time: leaseUntil, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", &steam.ThrottleDeferred{At: now.Add(time.Second)}
	}
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return uuid.UUID(row.FencingToken.Bytes).String(), nil
}

func (t *PersistentHostThrottle) ReleaseSlot(host, rawToken string) {
	token, err := uuid.Parse(rawToken)
	if err != nil || !validCatalogHost(host) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = t.store.Queries.ReleaseCatalogHostSlot(ctx, dbgen.ReleaseCatalogHostSlotParams{NowAt: pgtype.Timestamptz{Time: t.now().UTC(), Valid: true}, Host: strings.ToLower(host), FencingToken: catalogUUID(token)})
}
