package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	dbgen "next-game-crush/internal/store/sqlc"
)

// ReserveBudget charges one unique identity in a UTC-hour bucket. Repeating an
// identity, including a transport retry after restart, is always idempotent.
func (r *SearchRepository) ReserveBudget(ctx context.Context, kind, identity string, limit int, now time.Time) (bool, time.Time, error) {
	if r == nil || r.store == nil || (kind != "appdetails" && kind != "ai") || strings.TrimSpace(identity) == "" || limit < 1 || limit > 100000 || now.IsZero() {
		return false, time.Time{}, errors.New("invalid catalog budget reservation")
	}
	hour := now.UTC().Truncate(time.Hour)
	_, err := r.store.Queries.ReserveCatalogBudget(ctx, dbgen.ReserveCatalogBudgetParams{Kind: kind, HourAt: pgtype.Timestamptz{Time: hour, Valid: true}, IdentityHash: catalogBudgetHash(identity), BudgetLimit: int32(limit)})
	if err == nil {
		return true, time.Time{}, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, hour.Add(time.Hour), nil
	}
	return false, time.Time{}, err
}

func catalogBudgetHash(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func reserveBudgetWithQueries(ctx context.Context, q *dbgen.Queries, kind, identity string, limit int, now time.Time) (bool, time.Time, error) {
	hour := now.UTC().Truncate(time.Hour)
	_, err := q.ReserveCatalogBudget(ctx, dbgen.ReserveCatalogBudgetParams{Kind: kind, HourAt: pgtype.Timestamptz{Time: hour, Valid: true}, IdentityHash: catalogBudgetHash(identity), BudgetLimit: int32(limit)})
	if err == nil {
		return true, time.Time{}, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, hour.Add(time.Hour), nil
	}
	return false, time.Time{}, err
}

func (r *SearchRepository) PruneGateHistory(ctx context.Context, before, now time.Time) error {
	if r == nil || r.store == nil || before.IsZero() || now.IsZero() || !before.Before(now) {
		return errors.New("invalid catalog gate prune window")
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	if _, err := q.PruneCatalogHostSlots(ctx, pgtype.Timestamptz{Time: now.UTC(), Valid: true}); err != nil {
		return err
	}
	if _, err := q.PruneCatalogReservations(ctx, pgtype.Timestamptz{Time: before.UTC(), Valid: true}); err != nil {
		return err
	}
	if _, err := q.PruneCatalogBudgets(ctx, pgtype.Timestamptz{Time: before.UTC(), Valid: true}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
