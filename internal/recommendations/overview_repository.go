package recommendations

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// WishlistSourceState reads metadata only; published factors own classification.
// Persisted diagnostics are deliberately excluded from this read model.
func (r *SQLRepository) WishlistSourceState(ctx context.Context, userID uuid.UUID) (WishlistSourceState, error) {
	state := WishlistSourceState{Availability: "unknown"}
	var success pgtype.Timestamptz
	err := r.store.Pool.QueryRow(ctx, `SELECT availability,last_successful_fetch_at FROM user_source_states WHERE user_id=$1 AND source='wishlist' AND resource_key=''`, userID).Scan(&state.Availability, &success)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return WishlistSourceState{}, err
	}
	if success.Valid {
		stamp := success.Time.UTC()
		state.LastSuccessfulFetchAt = &stamp
	}
	switch state.Availability {
	case "available", "not_modified", "unavailable", "request_failed":
	default:
		state.Availability = "unknown"
	}
	return state, nil
}
