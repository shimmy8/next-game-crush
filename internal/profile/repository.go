package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"
	"next-game-crush/internal/store"
	dbgen "next-game-crush/internal/store/sqlc"
)

type SQLRepository struct {
	store          *store.Store
	embeddingModel string
	now            func() time.Time
}

func NewSQLRepository(database *store.Store, embeddingModel string) (*SQLRepository, error) {
	if database == nil || embeddingModel == "" {
		return nil, errors.New("profile store and embedding model are required")
	}
	return &SQLRepository{store: database, embeddingModel: embeddingModel, now: time.Now}, nil
}

// LoadActivitySignals returns only observed activity. Wishlist membership and
// direct feedback are deliberately absent from this query.
func (r *SQLRepository) LoadActivitySignals(ctx context.Context, userID uuid.UUID) ([]Signal, error) {
	return r.loadActivitySignals(ctx, r.store.Pool, userID)
}

// LoadSignals is retained as a compatibility alias while downstream ranking is
// moved to the explicit activity-profile contract.
func (r *SQLRepository) LoadSignals(ctx context.Context, userID uuid.UUID) ([]Signal, error) {
	return r.LoadActivitySignals(ctx, userID)
}

type rowQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (r *SQLRepository) loadActivitySignals(ctx context.Context, queryer rowQueryer, userID uuid.UUID) ([]Signal, error) {
	rows, err := queryer.Query(ctx, `WITH relevant AS (
		SELECT game_id FROM user_games WHERE user_id=$1 AND (playtime_minutes>0 OR recently_played_minutes>0)
		UNION SELECT game_id FROM achievement_summaries WHERE user_id=$1 AND achieved>0
		UNION SELECT game_id FROM user_game_completion_projections WHERE user_id=$1 AND weight>0
	)
	SELECT rel.game_id,e.embedding,COALESCE(ug.playtime_minutes,0),COALESCE(ug.recently_played_minutes,0),ug.last_played_at,
		COALESCE(a.achieved,0),COALESCE(a.total,0),
		CASE WHEN COALESCE(a.total,0)>0 THEN a.achieved::float8/a.total ELSE 0 END,
		COALESCE(cp.ending_count,0),COALESCE(cp.hundred_percent_basis,false),COALESCE(cp.weight,0),
		cp.completion_at,cp.basis,cp.algorithm_version
	FROM relevant rel
	LEFT JOIN game_embeddings e ON e.game_id=rel.game_id AND e.model=$2
	LEFT JOIN user_games ug ON ug.user_id=$1 AND ug.game_id=rel.game_id
	LEFT JOIN achievement_summaries a ON a.user_id=$1 AND a.game_id=rel.game_id
	LEFT JOIN user_game_completion_projections cp ON cp.user_id=$1 AND cp.game_id=rel.game_id
	ORDER BY rel.game_id`, userID, r.embeddingModel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Signal{}
	for rows.Next() {
		var signal Signal
		var vector *pgvector.Vector
		var lastPlayedAt, completionAt pgtype.Timestamptz
		var completionBasis, completionAlgorithm pgtype.Text
		if err := rows.Scan(
			&signal.GameID, &vector, &signal.PlaytimeMinutes, &signal.RecentMinutes, &lastPlayedAt,
			&signal.Achieved, &signal.Total, &signal.AchievementRatio,
			&signal.CompletionEndingCount, &signal.CompletionHundredPercent, &signal.CompletionWeight,
			&completionAt, &completionBasis, &completionAlgorithm,
		); err != nil {
			return nil, err
		}
		if vector != nil {
			signal.Vector = append([]float32(nil), vector.Slice()...)
		}
		if lastPlayedAt.Valid {
			signal.LastPlayedAt = lastPlayedAt.Time.UTC()
		}
		if completionAt.Valid {
			signal.CompletionAt = completionAt.Time.UTC()
		}
		if completionBasis.Valid {
			signal.CompletionBasis = completionBasis.String
		}
		if completionAlgorithm.Valid {
			signal.CompletionAlgorithm = completionAlgorithm.String
		}
		result = append(result, signal)
	}
	return result, rows.Err()
}

func (r *SQLRepository) BuildDraft(ctx context.Context, request BuildRequest, asOf time.Time, algorithm string) (Profile, bool, error) {
	if request.UserID == uuid.Nil || request.SourceWorkflowID == uuid.Nil || strings.TrimSpace(request.BuildKey) == "" {
		return Profile{}, false, errors.New("user ID, source workflow ID, and build key are required")
	}
	canonicalKey := profileBuildKey(request.BuildKey+":"+request.SourceWorkflowID.String(), algorithm)
	// A transaction that waits for the user lock must see the writer that just
	// released it; read committed avoids retaining a pre-lock snapshot.
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Profile{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, `profile:`+request.UserID.String()); err != nil {
		return Profile{}, false, err
	}
	var liveUser uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 AND NOT EXISTS(SELECT 1 FROM deleted_user_tombstones WHERE user_id=$1) FOR UPDATE`, request.UserID).Scan(&liveUser); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Profile{}, false, errors.New("profile owner is unavailable")
		}
		return Profile{}, false, err
	}
	queries := r.store.Queries.WithTx(tx)
	existing, err := queries.GetActivityProfileByBuildKey(ctx, dbgen.GetActivityProfileByBuildKeyParams{UserID: uuidToPG(request.UserID), BuildKey: textToPG(canonicalKey)})
	if err == nil {
		profile, loadErr := r.profileFromRow(ctx, queries, existing)
		if loadErr != nil {
			return Profile{}, false, loadErr
		}
		if err = tx.Commit(ctx); err != nil {
			return Profile{}, false, err
		}
		return profile, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, false, err
	}
	signals, err := r.loadActivitySignals(ctx, tx, request.UserID)
	if err != nil {
		return Profile{}, false, err
	}
	vectors := BuildVectors(signals, asOf.UTC())
	var version int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM preference_profiles WHERE user_id=$1`, request.UserID).Scan(&version); err != nil {
		return Profile{}, false, err
	}
	state := PublicationBuilding
	if len(signals) == 0 {
		state = PublicationInsufficient
	}
	metadata, err := json.Marshal(map[string]any{
		"algorithmVersion":           algorithm,
		"completionAlgorithmVersion": CompletionAlgorithmVersion,
		"sourceWorkflowId":           request.SourceWorkflowID.String(),
		"confidence":                 vectors.Confidence,
		"evidenceCount":              vectors.Evidence,
		"positiveStrength":           vectors.PositiveStrength,
		"signalCounts":               vectors.SignalCounts,
	})
	if err != nil {
		return Profile{}, false, err
	}
	id, created := uuid.New(), asOf.UTC()
	row, err := queries.InsertActivityProfile(ctx, dbgen.InsertActivityProfileParams{
		ID: uuidToPG(id), UserID: uuidToPG(request.UserID), Version: version,
		AlgorithmMetadata: metadata, CreatedAt: timeToPG(created), BuildKey: textToPG(canonicalKey),
		PublicationState: string(state),
	})
	if err != nil {
		return Profile{}, false, err
	}
	for _, signal := range signals {
		params := dbgen.InsertActivityProfileEvidenceParams{
			ProfileID: uuidToPG(id), GameID: uuidToPG(signal.GameID),
			PlaytimeMinutes: signal.PlaytimeMinutes, RecentlyPlayedMinutes: signal.RecentMinutes,
			Achieved: signal.Achieved, Total: signal.Total,
			CompletionEndingCount:         signal.CompletionEndingCount,
			CompletionHundredPercentBasis: signal.CompletionHundredPercent,
			CompletionWeight:              signal.CompletionWeight,
		}
		if !signal.LastPlayedAt.IsZero() {
			params.LastPlayedAt = timeToPG(signal.LastPlayedAt)
		}
		if !signal.CompletionAt.IsZero() {
			params.CompletionAt = timeToPG(signal.CompletionAt)
		}
		if signal.CompletionBasis != "" {
			params.CompletionBasis = textToPG(signal.CompletionBasis)
		}
		if signal.CompletionAlgorithm != "" {
			params.CompletionAlgorithmVersion = textToPG(signal.CompletionAlgorithm)
		}
		if err := queries.InsertActivityProfileEvidence(ctx, params); err != nil {
			return Profile{}, false, err
		}
	}
	if vectors.Evidence > 0 && validVector(vectors.Positive) {
		if err := queries.InsertActivityProfileEmbedding(ctx, dbgen.InsertActivityProfileEmbeddingParams{
			ProfileID: uuidToPG(id), Polarity: "positive", Model: r.embeddingModel,
			InputContentHash: fmt.Sprintf("%s:%d", algorithm, version),
			Embedding:        pgvector.NewVector(vectors.Positive), Source: "deterministic",
			LastSuccessfulFetchAt: timeToPG(created),
		}); err != nil {
			return Profile{}, false, err
		}
	}
	profile, err := r.profileFromRow(ctx, queries, row)
	if err != nil {
		return Profile{}, false, err
	}
	if err := verifyProfileOwner(ctx, tx, request.UserID); err != nil {
		return Profile{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Profile{}, false, err
	}
	return profile, true, nil
}

// SaveProfile is a temporary compatibility seam for callers migrated in Task
// 5. It writes only the positive vector and never creates negative activity.
func (r *SQLRepository) SaveProfile(ctx context.Context, userID uuid.UUID, vectors Vectors, algorithm, buildKey string) (Profile, error) {
	request := BuildRequest{UserID: userID, SourceWorkflowID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(buildKey)), BuildKey: buildKey}
	if buildKey == "" {
		request.BuildKey = uuid.NewString()
	}
	profile, _, err := r.buildProvidedVectors(ctx, request, vectors, algorithm)
	return profile, err
}

func (r *SQLRepository) buildProvidedVectors(ctx context.Context, request BuildRequest, vectors Vectors, algorithm string) (Profile, bool, error) {
	canonicalKey := profileBuildKey(request.BuildKey+":"+request.SourceWorkflowID.String(), algorithm)
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Profile{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, `profile:`+request.UserID.String()); err != nil {
		return Profile{}, false, err
	}
	queries := r.store.Queries.WithTx(tx)
	if existing, getErr := queries.GetActivityProfileByBuildKey(ctx, dbgen.GetActivityProfileByBuildKeyParams{UserID: uuidToPG(request.UserID), BuildKey: textToPG(canonicalKey)}); getErr == nil {
		profile, loadErr := r.profileFromRow(ctx, queries, existing)
		if loadErr != nil {
			return Profile{}, false, loadErr
		}
		if err = tx.Commit(ctx); err != nil {
			return Profile{}, false, err
		}
		return profile, false, nil
	} else if !errors.Is(getErr, pgx.ErrNoRows) {
		return Profile{}, false, getErr
	}
	var version int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM preference_profiles WHERE user_id=$1`, request.UserID).Scan(&version); err != nil {
		return Profile{}, false, err
	}
	created := r.now().UTC()
	metadata, _ := json.Marshal(map[string]any{"algorithmVersion": algorithm, "completionAlgorithmVersion": CompletionAlgorithmVersion, "confidence": vectors.Confidence, "evidenceCount": vectors.Evidence, "positiveStrength": vectors.PositiveStrength, "signalCounts": vectors.SignalCounts})
	row, err := queries.InsertActivityProfile(ctx, dbgen.InsertActivityProfileParams{ID: uuidToPG(uuid.New()), UserID: uuidToPG(request.UserID), Version: version, AlgorithmMetadata: metadata, CreatedAt: timeToPG(created), BuildKey: textToPG(canonicalKey), PublicationState: string(PublicationBuilding)})
	if err != nil {
		return Profile{}, false, err
	}
	if validVector(vectors.Positive) {
		if err := queries.InsertActivityProfileEmbedding(ctx, dbgen.InsertActivityProfileEmbeddingParams{ProfileID: row.ID, Polarity: "positive", Model: r.embeddingModel, InputContentHash: fmt.Sprintf("%s:%d", algorithm, version), Embedding: pgvector.NewVector(vectors.Positive), Source: "deterministic", LastSuccessfulFetchAt: timeToPG(created)}); err != nil {
			return Profile{}, false, err
		}
	}
	profile, err := r.profileFromRow(ctx, queries, row)
	if err != nil {
		return Profile{}, false, err
	}
	if err := verifyProfileOwner(ctx, tx, request.UserID); err != nil {
		return Profile{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Profile{}, false, err
	}
	return profile, true, nil
}

func (r *SQLRepository) LatestVectorComplete(ctx context.Context, userID uuid.UUID) (Profile, error) {
	row, err := r.store.Queries.GetLatestActivityProfileState(ctx, uuidToPG(userID))
	if err != nil {
		return Profile{}, err
	}
	return r.profileFromRow(ctx, r.store.Queries, row)
}

// LatestProfile is retained as an internal compatibility alias while ranking
// callers move to the explicit vector-complete name.
func (r *SQLRepository) LatestProfile(ctx context.Context, userID uuid.UUID) (Profile, error) {
	return r.LatestVectorComplete(ctx, userID)
}

// LatestPublishedProfile implements the public stale-while-revalidate view.
// It inspects the newest state first so an authoritative insufficient snapshot
// never resurrects an older document.
func (r *SQLRepository) LatestPublishedProfile(ctx context.Context, userID uuid.UUID) (Profile, error) {
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Profile{}, err
	}
	defer tx.Rollback(ctx)
	queries := r.store.Queries.WithTx(tx)
	newest, err := queries.GetLatestActivityProfileState(ctx, uuidToPG(userID))
	if err != nil {
		return Profile{}, err
	}
	var visible Profile
	switch PublicationState(newest.PublicationState) {
	case PublicationPublished:
		visible, err = r.profileFromRow(ctx, queries, newest)
		if err == nil {
			var boundaryActive bool
			if activeErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sync_jobs
				WHERE user_id=$1 AND kind='profile_overlay.boundary_backfill'
				  AND status IN ('queued','running','waiting','retryable'))`, userID).Scan(&boundaryActive); activeErr != nil {
				return Profile{}, activeErr
			}
			visible.Updating = boundaryActive
		}
	case PublicationInsufficient:
		return Profile{}, pgx.ErrNoRows
	case PublicationBuilding, PublicationFailed:
		published, loadErr := queries.GetLatestPublishedActivityProfile(ctx, uuidToPG(userID))
		if loadErr != nil {
			return Profile{}, loadErr
		}
		visible, loadErr = r.profileFromRow(ctx, queries, published)
		if loadErr != nil {
			return Profile{}, loadErr
		}
		visible.Updating = PublicationState(newest.PublicationState) == PublicationBuilding
	default:
		return Profile{}, fmt.Errorf("unsupported profile publication state %q", newest.PublicationState)
	}
	if err != nil {
		return Profile{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Profile{}, err
	}
	return visible, nil
}

type profileQueries interface {
	GetProfileEmbeddings(context.Context, pgtype.UUID) ([]dbgen.ProfileEmbedding, error)
}

func (r *SQLRepository) profileFromRow(ctx context.Context, queries profileQueries, row dbgen.PreferenceProfile) (Profile, error) {
	profile := Profile{ID: uuid.UUID(row.ID.Bytes), UserID: uuid.UUID(row.UserID.Bytes), Version: row.Version, BuildKey: row.BuildKey.String, PublicationState: PublicationState(row.PublicationState)}
	if row.Summary.Valid {
		profile.Summary = row.Summary.String
	}
	if row.SummarySource.Valid {
		profile.SummarySource = SummarySource(row.SummarySource.String)
	}
	if row.CreatedAt.Valid {
		profile.CreatedAt = row.CreatedAt.Time.UTC()
	}
	if row.PublishedAt.Valid {
		published := row.PublishedAt.Time.UTC()
		profile.PublishedAt = &published
	}
	var values struct {
		Confidence       float64        `json:"confidence"`
		EvidenceCount    int            `json:"evidenceCount"`
		PositiveStrength *float64       `json:"positiveStrength"`
		SignalCounts     map[string]int `json:"signalCounts"`
	}
	_ = json.Unmarshal(row.AlgorithmMetadata, &values)
	profile.Confidence, profile.EvidenceCount, profile.SignalCounts = finite01(values.Confidence), values.EvidenceCount, values.SignalCounts
	profile.PositiveStrength = 1
	if values.PositiveStrength != nil {
		profile.PositiveStrength = finite01(*values.PositiveStrength)
	}
	embeddings, err := queries.GetProfileEmbeddings(ctx, row.ID)
	if err != nil {
		return Profile{}, err
	}
	for _, embedding := range embeddings {
		if embedding.Model == r.embeddingModel && embedding.Polarity == "positive" {
			profile.Positive = append([]float32(nil), embedding.Embedding.Slice()...)
		}
	}
	return WithSummaryState(profile), nil
}

func finite01(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func profileBuildKey(source, algorithm string) string {
	if source == "" {
		return ""
	}
	return source + ":" + algorithm + ":" + CompletionAlgorithmVersion
}

func uuidToPG(value uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: value, Valid: true} }
func timeToPG(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
func textToPG(value string) pgtype.Text { return pgtype.Text{String: value, Valid: value != ""} }

func verifyProfileOwner(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1) AND NOT EXISTS(SELECT 1 FROM deleted_user_tombstones WHERE user_id=$1)`, userID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errors.New("profile owner is unavailable")
	}
	return nil
}

var _ Repository = (*SQLRepository)(nil)
