package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

// CurrentEligibilityReader is the single read-time admission gate shared by
// ranking, HTTP presentation, and digest delivery.
type CurrentEligibilityReader interface {
	Eligible(context.Context, uuid.UUID, time.Time) (bool, error)
	FilterEligibleGameIDs(context.Context, []uuid.UUID, time.Time) (map[uuid.UUID]bool, error)
}

type EligibilityRepository struct {
	store                     *store.Store
	lookbackDays, horizonDays int
}

func NewEligibilityRepository(database *store.Store) *EligibilityRepository {
	return &EligibilityRepository{store: database, lookbackDays: 30, horizonDays: 30}
}

func (r *EligibilityRepository) SetWindowDays(lookbackDays, horizonDays int) error {
	if lookbackDays < 1 || lookbackDays > 366 || horizonDays < 1 || horizonDays > 366 {
		return errors.New("catalog eligibility windows must be between 1 and 366 days")
	}
	r.lookbackDays, r.horizonDays = lookbackDays, horizonDays
	return nil
}

func (r *EligibilityRepository) Eligible(ctx context.Context, gameID uuid.UUID, now time.Time) (bool, error) {
	if r == nil || r.store == nil || r.store.Pool == nil || gameID == uuid.Nil || now.IsZero() {
		return false, errors.New("catalog eligibility requires store, game, and clock")
	}
	return r.store.Queries.IsCatalogGameEligible(ctx, dbgen.IsCatalogGameEligibleParams{
		GameID: pgtype.UUID{Bytes: gameID, Valid: true}, Today: pgtype.Date{Time: utcCalendarDay(now), Valid: true},
		LookbackDays: int32(r.lookbackDays), HorizonDays: int32(r.horizonDays),
	})
}

func (r *EligibilityRepository) FilterEligibleGameIDs(ctx context.Context, gameIDs []uuid.UUID, now time.Time) (map[uuid.UUID]bool, error) {
	result := make(map[uuid.UUID]bool, len(gameIDs))
	if len(gameIDs) == 0 {
		return result, nil
	}
	if r == nil || r.store == nil || r.store.Pool == nil || now.IsZero() {
		return nil, errors.New("catalog eligibility requires store and clock")
	}
	ids := make([]pgtype.UUID, 0, len(gameIDs))
	for _, id := range gameIDs {
		if id != uuid.Nil {
			ids = append(ids, pgtype.UUID{Bytes: id, Valid: true})
		}
	}
	rows, err := r.store.Queries.FilterCatalogEligibleGameIDs(ctx, dbgen.FilterCatalogEligibleGameIDsParams{
		GameIds: ids, Today: pgtype.Date{Time: utcCalendarDay(now), Valid: true},
		LookbackDays: int32(r.lookbackDays), HorizonDays: int32(r.horizonDays),
	})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		id, parseErr := uuid.Parse(row)
		if parseErr != nil {
			return nil, parseErr
		}
		result[id] = true
	}
	return result, nil
}
