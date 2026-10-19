package preferences

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"
	dbgen "next-game-crush/internal/store/sqlc"
)

func (r *SQLRepository) LoadOverlaySnapshot(ctx context.Context, userID uuid.UUID) (OverlaySnapshot, error) {
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return OverlaySnapshot{}, err
	}
	defer tx.Rollback(ctx)
	snapshot, err := r.loadOverlaySnapshot(ctx, r.store.Queries.WithTx(tx), userID)
	if err != nil {
		return OverlaySnapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OverlaySnapshot{}, err
	}
	return snapshot, nil
}

func (r *SQLRepository) loadOverlaySnapshot(ctx context.Context, queries *dbgen.Queries, userID uuid.UUID) (OverlaySnapshot, error) {
	reactionRows, err := queries.ListEffectiveDirectReactions(ctx, uuidToPG(userID))
	if err != nil {
		return OverlaySnapshot{}, err
	}
	snapshot := OverlaySnapshot{Reactions: make([]DirectReaction, 0, len(reactionRows))}
	for _, row := range reactionRows {
		if !row.FeedbackID.Valid || !row.GameID.Valid || !row.CreatedAt.Valid {
			return OverlaySnapshot{}, errors.New("invalid stored direct reaction")
		}
		reaction := DirectReaction{FeedbackID: uuid.UUID(row.FeedbackID.Bytes), GameID: uuid.UUID(row.GameID.Bytes), Action: row.Action, CreatedAt: row.CreatedAt.Time.UTC()}
		if len(row.EmbeddingSnapshot) == 3 && row.EmbeddingSnapshot[0] != "" && row.EmbeddingSnapshot[1] != "" && row.EmbeddingSnapshot[2] != "" {
			var vector pgvector.Vector
			if err := vector.Scan(row.EmbeddingSnapshot[2]); err != nil {
				return OverlaySnapshot{}, fmt.Errorf("parse saved reaction embedding: %w", err)
			}
			reaction.EmbeddingModel = row.EmbeddingSnapshot[0]
			reaction.EmbeddingHash = row.EmbeddingSnapshot[1]
			reaction.Vector = append([]float32(nil), vector.Slice()...)
		}
		snapshot.Reactions = append(snapshot.Reactions, reaction)
	}
	adjustmentRows, err := queries.ListEffectivePreferenceAdjustments(ctx, uuidToPG(userID))
	if err != nil {
		return OverlaySnapshot{}, err
	}
	snapshot.Corrections = make([]Adjustment, 0, len(adjustmentRows))
	for _, row := range adjustmentRows {
		adjustment, err := adjustmentFromRow(row)
		if err != nil {
			return OverlaySnapshot{}, err
		}
		snapshot.Corrections = append(snapshot.Corrections, adjustment)
	}
	return snapshot, nil
}

func (r *SQLRepository) RebuildOverlay(ctx context.Context, userID uuid.UUID, sourceBuildKey string) (Overlay, error) {
	if userID == uuid.Nil {
		return Overlay{}, errors.New("overlay user is required")
	}
	for attempt := 0; attempt < 3; attempt++ {
		overlay, err := r.rebuildOverlayOnce(ctx, userID, sourceBuildKey)
		if err == nil || !retryOverlayTransaction(err) {
			return overlay, err
		}
	}
	return Overlay{}, errors.New("preference overlay transaction retry limit exceeded")
}

func (r *SQLRepository) rebuildOverlayOnce(ctx context.Context, userID uuid.UUID, sourceBuildKey string) (Overlay, error) {
	connection, err := r.store.Pool.Acquire(ctx)
	if err != nil {
		return Overlay{}, err
	}
	defer connection.Release()
	if _, err = connection.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1::text,0))`, userID); err != nil {
		return Overlay{}, err
	}
	defer func() {
		_, _ = connection.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1::text,0))`, userID)
	}()
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return Overlay{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, `overlay:`+userID.String()); err != nil {
		return Overlay{}, err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1) AND NOT EXISTS(SELECT 1 FROM deleted_user_tombstones WHERE user_id=$1)`, userID).Scan(&exists); err != nil {
		return Overlay{}, err
	}
	if !exists {
		return Overlay{}, pgx.ErrNoRows
	}
	queries := r.store.Queries.WithTx(tx)
	snapshot, err := r.loadOverlaySnapshot(ctx, queries, userID)
	if err != nil {
		return Overlay{}, err
	}
	snapshot = coherentOverlaySnapshot(snapshot)
	vectors := BuildOverlay(snapshot)
	buildKey := overlayBuildKey(sourceBuildKey, vectors.InputContentHash)
	if existingRow, lookupErr := queries.GetPreferenceOverlayByBuildKey(ctx, dbgen.GetPreferenceOverlayByBuildKeyParams{UserID: uuidToPG(userID), BuildKey: buildKey}); lookupErr == nil {
		if existingRow.InputContentHash != vectors.InputContentHash || existingRow.AlgorithmVersion != OverlayAlgorithmVersion {
			return Overlay{}, errors.New("preference overlay build key collision")
		}
		existing, err := loadOverlayRow(ctx, queries, existingRow)
		if err != nil {
			return Overlay{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Overlay{}, err
		}
		return existing, nil
	} else if !errors.Is(lookupErr, pgx.ErrNoRows) {
		return Overlay{}, lookupErr
	}
	if latestRow, lookupErr := queries.GetLatestPreferenceOverlay(ctx, uuidToPG(userID)); lookupErr == nil && latestRow.AlgorithmVersion == OverlayAlgorithmVersion && latestRow.InputContentHash == vectors.InputContentHash {
		existing, err := loadOverlayRow(ctx, queries, latestRow)
		if err != nil {
			return Overlay{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Overlay{}, err
		}
		return existing, nil
	} else if lookupErr != nil && !errors.Is(lookupErr, pgx.ErrNoRows) {
		return Overlay{}, lookupErr
	}
	var version int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM preference_overlays WHERE user_id=$1`, userID).Scan(&version); err != nil {
		return Overlay{}, err
	}
	createdAt := time.Now().UTC()
	metadata := OverlayMetadata{PositiveSources: vectors.PositiveSources, NegativeSources: vectors.NegativeSources, MissingVectors: vectors.MissingVectors, EffectiveCorrections: vectors.EffectiveCorrections}
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return Overlay{}, err
	}
	id := uuid.New()
	row, err := queries.InsertPreferenceOverlay(ctx, dbgen.InsertPreferenceOverlayParams{
		ID: uuidToPG(id), UserID: uuidToPG(userID), Version: version, AlgorithmVersion: OverlayAlgorithmVersion,
		BuildKey: buildKey, InputContentHash: vectors.InputContentHash, AlgorithmMetadata: encodedMetadata,
		CreatedAt: pgtype.Timestamptz{Time: createdAt, Valid: true},
	})
	if err != nil {
		return Overlay{}, err
	}
	for _, item := range []struct {
		polarity string
		vector   []float32
	}{
		{polarity: "positive", vector: vectors.Positive},
		{polarity: "negative", vector: vectors.Negative},
	} {
		if item.vector == nil {
			continue
		}
		model, err := overlayModel(snapshot.Reactions, item.polarity)
		if err != nil {
			return Overlay{}, err
		}
		if err := queries.InsertPreferenceOverlayEmbedding(ctx, dbgen.InsertPreferenceOverlayEmbeddingParams{
			OverlayID: uuidToPG(id), Polarity: item.polarity, Model: model, InputContentHash: vectors.InputContentHash,
			Embedding: pgvector.NewVector(item.vector), Source: "deterministic", LastSuccessfulFetchAt: pgtype.Timestamptz{Time: createdAt, Valid: true},
		}); err != nil {
			return Overlay{}, err
		}
	}
	result, err := loadOverlayRow(ctx, queries, row)
	if err != nil {
		return Overlay{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Overlay{}, err
	}
	return result, nil
}

func retryOverlayTransaction(err error) bool {
	var databaseError *pgconn.PgError
	return errors.As(err, &databaseError) && (databaseError.Code == "23505" || databaseError.Code == "40001")
}

func (r *SQLRepository) OverlayByBuildKey(ctx context.Context, userID uuid.UUID, sourceBuildKey string) (Overlay, error) {
	if userID == uuid.Nil || strings.TrimSpace(sourceBuildKey) == "" {
		return Overlay{}, pgx.ErrNoRows
	}
	row, err := r.store.Queries.GetPreferenceOverlayByBuildKey(ctx, dbgen.GetPreferenceOverlayByBuildKeyParams{
		UserID: uuidToPG(userID), BuildKey: overlayBuildKey(sourceBuildKey, ""),
	})
	if err != nil {
		return Overlay{}, err
	}
	return loadOverlayRow(ctx, r.store.Queries, row)
}

func (r *SQLRepository) LatestOverlay(ctx context.Context, userID uuid.UUID) (Overlay, error) {
	row, err := r.store.Queries.GetLatestPreferenceOverlay(ctx, uuidToPG(userID))
	if err != nil {
		return Overlay{}, err
	}
	return loadOverlayRow(ctx, r.store.Queries, row)
}

func loadOverlayRow(ctx context.Context, queries *dbgen.Queries, row dbgen.PreferenceOverlay) (Overlay, error) {
	if !row.ID.Valid || !row.UserID.Valid || !row.CreatedAt.Valid {
		return Overlay{}, errors.New("invalid stored preference overlay")
	}
	result := Overlay{ID: uuid.UUID(row.ID.Bytes), UserID: uuid.UUID(row.UserID.Bytes), Version: row.Version, AlgorithmVersion: row.AlgorithmVersion, BuildKey: row.BuildKey, InputContentHash: row.InputContentHash, CreatedAt: row.CreatedAt.Time.UTC()}
	if err := json.Unmarshal(row.AlgorithmMetadata, &result.Metadata); err != nil {
		return Overlay{}, fmt.Errorf("decode preference overlay metadata: %w", err)
	}
	embeddings, err := queries.GetPreferenceOverlayEmbeddings(ctx, row.ID)
	if err != nil {
		return Overlay{}, err
	}
	for _, embedding := range embeddings {
		vector := append([]float32(nil), embedding.Embedding.Slice()...)
		if !validOverlayVector(vector) {
			return Overlay{}, errors.New("invalid stored preference overlay embedding")
		}
		switch embedding.Polarity {
		case "positive":
			result.Positive = vector
		case "negative":
			result.Negative = vector
		default:
			return Overlay{}, errors.New("invalid stored preference overlay polarity")
		}
	}
	return result, nil
}

func overlayModel(reactions []DirectReaction, polarity string) (string, error) {
	model := ""
	for _, reaction := range reactions {
		matches := polarity == "positive" && reaction.Action == "like" || polarity == "negative" && (reaction.Action == "dislike" || reaction.Action == "exclude")
		if !matches || !validOverlayVector(reaction.Vector) {
			continue
		}
		if reaction.EmbeddingModel == "" {
			return "", errors.New("preference overlay embedding model is missing")
		}
		if model == "" {
			model = reaction.EmbeddingModel
		} else if model != reaction.EmbeddingModel {
			return "", errors.New("preference overlay source embedding models differ")
		}
	}
	if model == "" {
		return "", errors.New("preference overlay embedding model is unavailable")
	}
	return model, nil
}

func overlayBuildKey(source, contentHash string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return "content:" + contentHash
	}
	return source + ":" + OverlayAlgorithmVersion
}

func coherentOverlaySnapshot(snapshot OverlaySnapshot) OverlaySnapshot {
	counts := map[string]int{}
	for _, reaction := range snapshot.Reactions {
		if validOverlayVector(reaction.Vector) && strings.TrimSpace(reaction.EmbeddingModel) != "" {
			counts[reaction.EmbeddingModel]++
		}
	}
	selected, selectedCount := "", 0
	for model, count := range counts {
		if count > selectedCount || count == selectedCount && (selected == "" || model < selected) {
			selected, selectedCount = model, count
		}
	}
	result := OverlaySnapshot{Reactions: append([]DirectReaction(nil), snapshot.Reactions...), Corrections: append([]Adjustment(nil), snapshot.Corrections...)}
	for index := range result.Reactions {
		if result.Reactions[index].EmbeddingModel == selected && validOverlayVector(result.Reactions[index].Vector) {
			continue
		}
		result.Reactions[index].EmbeddingModel = ""
		result.Reactions[index].EmbeddingHash = ""
		result.Reactions[index].Vector = nil
	}
	return result
}
