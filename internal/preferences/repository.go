package preferences

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

type Adjustment struct {
	ID, UserID, SourceFeedbackID, SourceGameID uuid.UUID
	Dimension                                  string
	Target                                     Mode
	Direction                                  string
	Confidence, Strength                       float64
	Active                                     bool
	CreatedAt, UpdatedAt                       time.Time
	DisabledAt                                 *time.Time
	DisableRebuildJobID                        *uuid.UUID
	DisablePreviousRankingVersion              *int64
}

type SQLRepository struct{ store *store.Store }

func NewSQLRepository(database *store.Store) (*SQLRepository, error) {
	if database == nil || database.Queries == nil {
		return nil, errors.New("preference store is required")
	}
	return &SQLRepository{store: database}, nil
}

func (r *SQLRepository) List(ctx context.Context, userID uuid.UUID, includeInactive bool) ([]Adjustment, error) {
	rows, err := r.store.Queries.ListPreferenceAdjustments(ctx, dbgen.ListPreferenceAdjustmentsParams{UserID: uuidToPG(userID), IncludeInactive: includeInactive})
	if err != nil {
		return nil, err
	}
	result := make([]Adjustment, 0, len(rows))
	for _, row := range rows {
		adjustment, err := adjustmentFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, adjustment)
	}
	return result, nil
}

func (r *SQLRepository) ForFeedback(ctx context.Context, userID, feedbackID uuid.UUID) (*Adjustment, error) {
	row, err := r.store.Queries.GetPreferenceAdjustmentForFeedback(ctx, dbgen.GetPreferenceAdjustmentForFeedbackParams{UserID: uuidToPG(userID), SourceFeedbackID: uuidToPG(feedbackID)})
	if err != nil {
		return nil, err
	}
	adjustment, err := adjustmentFromRow(row)
	if err != nil {
		return nil, err
	}
	return &adjustment, nil
}

func adjustmentFromRow(row dbgen.PreferenceAdjustment) (Adjustment, error) {
	if !row.ID.Valid || !row.UserID.Valid || !row.SourceFeedbackID.Valid || !row.SourceGameID.Valid || !row.CreatedAt.Valid || !row.UpdatedAt.Valid || !ValidMode(Mode(row.Target)) {
		return Adjustment{}, errors.New("invalid stored preference adjustment")
	}
	value := Adjustment{ID: uuid.UUID(row.ID.Bytes), UserID: uuid.UUID(row.UserID.Bytes), SourceFeedbackID: uuid.UUID(row.SourceFeedbackID.Bytes), SourceGameID: uuid.UUID(row.SourceGameID.Bytes), Dimension: row.Dimension, Target: Mode(row.Target), Direction: row.Direction, Confidence: row.Confidence, Strength: row.Strength, Active: row.Active, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
	if row.DisabledAt.Valid {
		v := row.DisabledAt.Time
		value.DisabledAt = &v
	}
	if row.DisableRebuildJobID.Valid {
		v := uuid.UUID(row.DisableRebuildJobID.Bytes)
		value.DisableRebuildJobID = &v
	}
	if row.DisablePreviousRankingVersion.Valid {
		v := row.DisablePreviousRankingVersion.Int64
		value.DisablePreviousRankingVersion = &v
	}
	return value, nil
}

func uuidToPG(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
