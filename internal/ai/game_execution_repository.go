package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	pgvector "github.com/pgvector/pgvector-go"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
	dbgen "next-game-crush/internal/store/sqlc"
)

func gameEvidenceFromSnapshot(name string, snapshot []byte) StoredGameEvidence {
	result := StoredGameEvidence{Name: strings.Join(strings.Fields(name), " ")}
	var details steam.GameDetails
	if len(snapshot) > 0 && json.Unmarshal(snapshot, &details) == nil {
		details = steam.NormalizeGameContent(details)
		result.Description = details.ShortDescription
		result.ContentHash = steam.GameContentHash(details)
		for _, genre := range details.Genres {
			result.Genres = append(result.Genres, genre.Description)
		}
		for _, category := range details.Categories {
			result.Categories = append(result.Categories, category.Description)
		}
	}
	return result
}

func (repo *SQLRepository) LoadGameExecution(ctx context.Context, id uuid.UUID, plan GameExecution, lease *jobs.Lease) (StoredGameEvidence, bool, error) {
	tx, err := repo.store.Pool.Begin(ctx)
	if err != nil {
		return StoredGameEvidence{}, false, err
	}
	defer tx.Rollback(ctx)
	stored, current, err := repo.lockGameExecution(ctx, tx, id, plan, lease)
	if err != nil || !current {
		return stored, current, err
	}
	if err = tx.Commit(ctx); err != nil {
		return stored, false, err
	}
	return stored, true, nil
}

func (repo *SQLRepository) lockGameExecution(ctx context.Context, tx pgx.Tx, id uuid.UUID, plan GameExecution, lease *jobs.Lease) (StoredGameEvidence, bool, error) {
	q := repo.store.Queries.WithTx(tx)
	game, err := q.LockGameEnrichmentEvidence(ctx, uuidPG(id))
	if err != nil {
		return StoredGameEvidence{}, false, err
	}
	stored := gameEvidenceFromSnapshot(game.Name, game.SourceSnapshot)
	if plan.ContentHash != "" && stored.ContentHash != plan.ContentHash {
		return stored, false, nil
	}
	if plan.Key != "" && plan.ContentHash == "" {
		// Persisted pre-plan catalog payloads still encode their logical content
		// and model identity in the key. Derive it from the trusted snapshot rather
		// than allowing an old queued job to process an unrelated latest snapshot.
		legacy := plan
		legacy.ContentHash = stored.ContentHash
		if !validGameExecution(id, legacy) {
			return stored, false, nil
		}
	}
	if plan.Key != "" {
		prefix := "catalog:enrichment:v1:" + id.String() + ":"
		if err = q.LockCatalogEnrichmentExecution(ctx, prefix); err != nil {
			return stored, false, err
		}
		latest, e := q.GetLatestCatalogEnrichmentExecution(ctx, prefix)
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return stored, false, e
		}
		if e == nil && (latest.IdempotencyKey != plan.Key || latest.Generation != plan.Generation || latest.Status == "dead" || latest.Status == "succeeded") {
			return stored, false, nil
		}
		if errors.Is(e, pgx.ErrNoRows) && plan.Generation > 0 {
			return stored, false, nil
		}
	}
	// Lock order matches catalog writers: game, execution namespace, job, feature.
	// The game SHARE lock freezes canonical metadata until conditional publication.
	if err = lockGameExecutionLease(ctx, tx, lease); err != nil {
		return stored, false, err
	}
	feature, err := q.LockGameEnrichmentFeatures(ctx, dbgen.LockGameEnrichmentFeaturesParams{GameID: uuidPG(id), Model: plan.TextModel})
	if err == nil {
		stored.StoredFeatures = feature.Features
		stored.FeatureInputHash = feature.InputContentHash
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return stored, false, err
	}
	return stored, true, nil
}

func lockGameExecutionLease(ctx context.Context, tx pgx.Tx, lease *jobs.Lease) error {
	if lease == nil || lease.ID == uuid.Nil {
		return nil
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM sync_jobs WHERE id=$1 AND status='running' AND worker_id=$2 AND fencing_token=$3 AND lease_until>clock_timestamp() FOR UPDATE`, lease.ID, lease.WorkerID, lease.FencingToken).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.ErrLeaseLost
	}
	return err
}

func commitGameExecution(ctx context.Context, tx pgx.Tx, lease *jobs.Lease) error {
	if lease != nil && lease.ID != uuid.Nil {
		var owned bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sync_jobs WHERE id=$1 AND status='running' AND worker_id=$2 AND fencing_token=$3 AND lease_until>clock_timestamp())`, lease.ID, lease.WorkerID, lease.FencingToken).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return jobs.ErrLeaseLost
		}
	}
	return tx.Commit(ctx)
}

func (repo *SQLRepository) SaveGameFeaturesForExecution(ctx context.Context, id uuid.UUID, plan GameExecution, lease *jobs.Lease, hash string, value GameFeatures) (bool, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	tx, err := repo.store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	stored, current, err := repo.lockGameExecution(ctx, tx, id, plan, lease)
	if err != nil || !current {
		return false, err
	}
	actual, _, err := featureEvidenceHash(stored, plan.TextModel)
	if err != nil {
		return false, err
	}
	if actual != hash {
		return false, nil
	}
	q := repo.store.Queries.WithTx(tx)
	if err = q.UpsertGameFeature(ctx, dbgen.UpsertGameFeatureParams{GameID: uuidPG(id), Model: plan.TextModel, InputContentHash: hash, Features: encoded}); err != nil {
		return false, err
	}
	if err = commitGameExecution(ctx, tx, lease); err != nil {
		return false, err
	}
	return true, nil
}

func (repo *SQLRepository) SaveGameEmbeddingForExecution(ctx context.Context, id uuid.UUID, plan GameExecution, lease *jobs.Lease, hash string, vector []float32) (bool, error) {
	tx, err := repo.store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	stored, current, err := repo.lockGameExecution(ctx, tx, id, plan, lease)
	if err != nil || !current {
		return false, err
	}
	actual, err := ContentHash(EmbeddingJobKind, SchemaVersion, "canonical-game-embedding-v1", plan.EmbeddingModel, canonicalEmbeddingText(stored))
	if err != nil {
		return false, err
	}
	if actual != hash {
		return false, nil
	}
	if plan.ContentHash != "" {
		featureHash, _, e := featureEvidenceHash(stored, plan.TextModel)
		if e != nil {
			return false, e
		}
		if stored.FeatureInputHash != featureHash {
			return false, nil
		}
	}
	if len(vector) != EmbeddingDimensions {
		return false, ErrInvalidEmbedding
	}
	q := repo.store.Queries.WithTx(tx)
	if err = q.PutGameEmbedding(ctx, dbgen.PutGameEmbeddingParams{GameID: uuidPG(id), Model: plan.EmbeddingModel, InputContentHash: hash, Embedding: pgvector.NewVector(vector)}); err != nil {
		return false, err
	}
	if err = commitGameExecution(ctx, tx, lease); err != nil {
		return false, err
	}
	return true, nil
}
