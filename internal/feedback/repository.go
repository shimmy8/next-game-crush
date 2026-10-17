package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/store"
)

type SQLRepository struct{ store *store.Store }

func NewSQLRepository(database *store.Store) (*SQLRepository, error) {
	if database == nil {
		return nil, errors.New("feedback store is required")
	}
	return &SQLRepository{store: database}, nil
}

func (r *SQLRepository) Record(ctx context.Context, command Command) (Event, bool, error) {
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Event{}, false, err
	}
	defer tx.Rollback(ctx)
	if existing, existingErr := scanEvent(tx.QueryRow(ctx, `SELECT id,recommendation_id,user_id,action,reason,free_text,idempotency_key,recommendation_snapshot,previous_ranking_version,resulting_ranking_version,rebuild_job_id FROM feedback_events WHERE idempotency_key=$1`, command.IdempotencyKey)); existingErr == nil {
		if err := tx.Commit(ctx); err != nil {
			return Event{}, false, err
		}
		return existing, false, nil
	} else if !errors.Is(existingErr, pgx.ErrNoRows) {
		return Event{}, false, existingErr
	}
	var snapshot []byte
	var rankingVersion int64
	err = tx.QueryRow(ctx, `SELECT jsonb_build_object(
		'id',rec.id,'gameId',rec.game_id,'name',g.name,'profileVersion',p.version,'rankingVersion',rec.ranking_version,
		'algorithmVersion',rec.algorithm_version,'score',rec.score,'confidence',COALESCE((rf.factors->>'confidence')::float8,0),
		'factors',rf.factors,'weights',rf.weights,'explanation',COALESCE(rec.explanation,''),'evidence',rec.explanation_evidence,
		'generatedExplanation',rec.explanation_generated,'release',jsonb_build_object('precision',COALESCE(rd.precision,'unknown'),'exactDate',CASE WHEN rd.precision='day' THEN rd.release_date END,'raw',rd.raw_text),'createdAt',rec.created_at), rec.ranking_version
		FROM recommendations rec JOIN games g ON g.id=rec.game_id JOIN preference_profiles p ON p.id=rec.profile_id
		JOIN recommendation_factors rf ON rf.recommendation_id=rec.id
		LEFT JOIN LATERAL(SELECT precision,release_date,raw_text FROM game_release_dates WHERE game_id=rec.game_id ORDER BY observed_at DESC,id DESC LIMIT 1)rd ON true
		WHERE rec.id=$1 AND rec.user_id=$2 AND rec.ranking_version=(SELECT max(ranking_version) FROM recommendation_runs WHERE user_id=$2 AND published)`, command.RecommendationID, command.UserID).Scan(&snapshot, &rankingVersion)
	if err != nil {
		return Event{}, false, err
	}
	id := uuid.New()
	commandTag, err := tx.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,reason,free_text,idempotency_key,recommendation_snapshot,previous_ranking_version)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(idempotency_key) DO NOTHING`, id, command.RecommendationID, command.UserID, command.Action, nullable(command.Reason), nullable(command.FreeText), command.IdempotencyKey, snapshot, rankingVersion)
	if err != nil {
		return Event{}, false, err
	}
	created := commandTag.RowsAffected() == 1
	event, err := scanEvent(tx.QueryRow(ctx, `SELECT id,recommendation_id,user_id,action,reason,free_text,idempotency_key,recommendation_snapshot,previous_ranking_version,resulting_ranking_version,rebuild_job_id FROM feedback_events WHERE idempotency_key=$1`, command.IdempotencyKey))
	if err != nil {
		return Event{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Event{}, false, err
	}
	return event, created, nil
}

func (r *SQLRepository) LinkRebuildJob(ctx context.Context, eventID, jobID uuid.UUID) error {
	tag, err := r.store.Pool.Exec(ctx, `UPDATE feedback_events SET rebuild_job_id=$2 WHERE id=$1 AND (rebuild_job_id IS NULL OR rebuild_job_id=$2)`, eventID, jobID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *SQLRepository) Event(ctx context.Context, id uuid.UUID) (Event, error) {
	return scanEvent(r.store.Pool.QueryRow(ctx, `SELECT id,recommendation_id,user_id,action,reason,free_text,idempotency_key,recommendation_snapshot,previous_ranking_version,resulting_ranking_version,rebuild_job_id FROM feedback_events WHERE id=$1`, id))
}

type rowScanner interface{ Scan(...any) error }

func scanEvent(row rowScanner) (Event, error) {
	var event Event
	var reason, text *string
	var resultingVersion *int64
	var jobID *uuid.UUID
	if err := row.Scan(&event.ID, &event.RecommendationID, &event.UserID, &event.Action, &reason, &text, &event.IdempotencyKey, &event.Snapshot, &event.PreviousRankingVersion, &resultingVersion, &jobID); err != nil {
		return Event{}, err
	}
	if reason != nil {
		event.Reason = *reason
	}
	if text != nil {
		event.FreeText = *text
	}
	if jobID != nil {
		event.RebuildJobID = *jobID
	}
	if resultingVersion != nil {
		event.ResultingRankingVersion = *resultingVersion
	}
	return event, nil
}

func (r *SQLRepository) Classification(ctx context.Context, eventID uuid.UUID) (Classification, error) {
	var value Classification
	err := r.store.Pool.QueryRow(ctx, `SELECT feedback_id,taxonomy,polarity,confidence,actionable,model,input_content_hash,schema_version FROM feedback_classifications WHERE feedback_id=$1`, eventID).Scan(&value.FeedbackID, &value.Taxonomy, &value.Polarity, &value.Confidence, &value.Actionable, &value.Model, &value.InputContentHash, &value.SchemaVersion)
	return value, err
}

func (r *SQLRepository) SaveClassification(ctx context.Context, value Classification) error {
	_, err := r.store.Pool.Exec(ctx, `INSERT INTO feedback_classifications(feedback_id,taxonomy,polarity,confidence,actionable,model,input_content_hash,schema_version)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(feedback_id) DO NOTHING`, value.FeedbackID, value.Taxonomy, value.Polarity, value.Confidence, value.Actionable, value.Model, value.InputContentHash, value.SchemaVersion)
	return err
}

func (r *SQLRepository) FinalizeRanking(ctx context.Context, eventID uuid.UUID) (int64, error) {
	var action string
	var previous int64
	if err := r.store.Pool.QueryRow(ctx, `SELECT action,previous_ranking_version FROM feedback_events WHERE id=$1`, eventID).Scan(&action, &previous); err != nil {
		return 0, err
	}
	version := previous
	if action != ActionRemind {
		var raw []byte
		if err := r.store.Pool.QueryRow(ctx, `SELECT result FROM sync_jobs WHERE idempotency_key=$1 AND status='succeeded'`, "feedback:"+eventID.String()+":ranking:v1").Scan(&raw); err != nil {
			return 0, err
		}
		var result struct {
			RankingVersion int64 `json:"rankingVersion"`
		}
		if json.Unmarshal(raw, &result) != nil || result.RankingVersion < 1 {
			return 0, errors.New("recommendation rebuild did not record a ranking version")
		}
		version = result.RankingVersion
	}
	tag, err := r.store.Pool.Exec(ctx, `UPDATE feedback_events SET resulting_ranking_version=$2 WHERE id=$1 AND (resulting_ranking_version IS NULL OR resulting_ranking_version=$2)`, eventID, version)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() != 1 {
		return 0, pgx.ErrNoRows
	}
	return version, nil
}

type rankedRecommendation struct{ ID, GameID uuid.UUID }

func (r *SQLRepository) RankingDiff(ctx context.Context, userID uuid.UUID, previousVersion int64, jobID uuid.UUID) (RankingDiff, error) {
	result := RankingDiff{RebuildStatus: "queued", PreviousVersion: previousVersion, Added: []uuid.UUID{}, Removed: []uuid.UUID{}, Reordered: []uuid.UUID{}}
	var status string
	var resultingVersion *int64
	var classTaxonomy, classPolarity *string
	var classConfidence *float64
	var classActionable *bool
	var err error
	if jobID != uuid.Nil {
		err = r.store.Pool.QueryRow(ctx, `SELECT j.status,f.resulting_ranking_version,fc.taxonomy,fc.polarity,fc.confidence,fc.actionable FROM feedback_events f JOIN sync_jobs j ON j.id=f.rebuild_job_id LEFT JOIN feedback_classifications fc ON fc.feedback_id=f.id WHERE f.user_id=$1 AND f.previous_ranking_version=$2 AND f.rebuild_job_id=$3`, userID, previousVersion, jobID).Scan(&status, &resultingVersion, &classTaxonomy, &classPolarity, &classConfidence, &classActionable)
	} else {
		err = r.store.Pool.QueryRow(ctx, `SELECT j.status,f.resulting_ranking_version,fc.taxonomy,fc.polarity,fc.confidence,fc.actionable FROM feedback_events f JOIN sync_jobs j ON j.id=f.rebuild_job_id LEFT JOIN feedback_classifications fc ON fc.feedback_id=f.id WHERE f.user_id=$1 AND f.previous_ranking_version=$2 ORDER BY f.created_at DESC,f.id DESC LIMIT 1`, userID, previousVersion).Scan(&status, &resultingVersion, &classTaxonomy, &classPolarity, &classConfidence, &classActionable)
	}
	if err != nil {
		return RankingDiff{}, err
	}
	result.RebuildStatus = status
	if err := r.store.Pool.QueryRow(ctx, `SELECT COALESCE(max(ranking_version),0) FROM recommendation_runs WHERE user_id=$1 AND published`, userID).Scan(&result.CurrentVersion); err != nil {
		return RankingDiff{}, err
	}
	if resultingVersion != nil {
		result.CurrentVersion = *resultingVersion
	}
	if classTaxonomy != nil && classPolarity != nil && classConfidence != nil && classActionable != nil {
		result.Classification = &ClassificationResult{Taxonomy: *classTaxonomy, Polarity: *classPolarity, Confidence: *classConfidence, Actionable: *classActionable}
	}
	previous, err := r.ranking(ctx, userID, previousVersion)
	if err != nil {
		return RankingDiff{}, err
	}
	current, err := r.ranking(ctx, userID, result.CurrentVersion)
	if err != nil {
		return RankingDiff{}, err
	}
	previousByGame, currentByGame := map[uuid.UUID]int{}, map[uuid.UUID]int{}
	for index, item := range previous {
		previousByGame[item.GameID] = index
	}
	for index, item := range current {
		currentByGame[item.GameID] = index
		if _, ok := previousByGame[item.GameID]; !ok {
			result.Added = append(result.Added, item.ID)
		}
	}
	for _, item := range previous {
		if _, ok := currentByGame[item.GameID]; !ok {
			result.Removed = append(result.Removed, item.ID)
		}
	}
	for index, item := range current {
		if old, ok := previousByGame[item.GameID]; ok && old != index {
			result.Reordered = append(result.Reordered, item.ID)
		}
	}
	return result, nil
}

func (r *SQLRepository) ranking(ctx context.Context, userID uuid.UUID, version int64) ([]rankedRecommendation, error) {
	if version < 1 {
		return []rankedRecommendation{}, nil
	}
	rows, err := r.store.Pool.Query(ctx, `SELECT r.id,r.game_id FROM recommendations r JOIN games g ON g.id=r.game_id WHERE r.user_id=$1 AND r.ranking_version=$2 AND EXISTS(SELECT 1 FROM recommendation_runs run WHERE run.user_id=$1 AND run.ranking_version=$2 AND run.published) ORDER BY r.score DESC,lower(g.name),g.id`, userID, version)
	if err != nil {
		return nil, fmt.Errorf("load ranking version: %w", err)
	}
	defer rows.Close()
	result := []rankedRecommendation{}
	for rows.Next() {
		var item rankedRecommendation
		if err := rows.Scan(&item.ID, &item.GameID); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

var _ Repository = (*SQLRepository)(nil)
