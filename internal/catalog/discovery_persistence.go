package catalog

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/jobs"
)

func lockDiscoveryLease(ctx context.Context, tx pgx.Tx, lease jobs.Lease) ([]byte, error) {
	var result []byte
	err := tx.QueryRow(ctx, `SELECT result FROM sync_jobs WHERE id=$1 AND status='running' AND worker_id=$2 AND fencing_token=$3 AND lease_until>clock_timestamp() FOR UPDATE`, lease.ID, lease.WorkerID, lease.FencingToken).Scan(&result)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, jobs.ErrLeaseLost
	}
	return result, err
}

func commitDiscoveryLease(ctx context.Context, tx pgx.Tx, lease jobs.Lease) error {
	var owned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sync_jobs WHERE id=$1 AND status='running' AND worker_id=$2 AND fencing_token=$3 AND lease_until>clock_timestamp())`, lease.ID, lease.WorkerID, lease.FencingToken).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return jobs.ErrLeaseLost
	}
	return tx.Commit(ctx)
}

func lockDiscoveryApp(ctx context.Context, tx pgx.Tx, appID int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, appID)
	return err
}
