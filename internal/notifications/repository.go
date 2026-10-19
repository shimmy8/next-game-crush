package notifications

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/store"
)

var ErrDeliveryOwned = errors.New("delivery is already owned by another attempt")

type SQLRepository struct {
	store       *store.Store
	eligibility catalog.CurrentEligibilityReader
	clock       func() time.Time
}

func NewSQLRepository(db *store.Store, eligibility catalog.CurrentEligibilityReader, clock func() time.Time) (*SQLRepository, error) {
	if db == nil || db.Pool == nil || eligibility == nil || clock == nil {
		return nil, errors.New("notification store, eligibility reader, and clock are required")
	}
	return &SQLRepository{store: db, eligibility: eligibility, clock: clock}, nil
}

func (r *SQLRepository) DigestInput(ctx context.Context, userID uuid.UUID) (DigestInput, error) {
	var displayName string
	if err := r.store.Pool.QueryRow(ctx, `SELECT COALESCE(display_name,'Steam player') FROM steam_accounts WHERE user_id=$1`, userID).Scan(&displayName); err != nil {
		return DigestInput{}, err
	}
	input := DigestInput{UserID: userID, DisplayName: displayName}
	sent := map[uuid.UUID]map[string]bool{}
	// Deliveries from the current ISO week remain candidates so repeated clicks
	// resolve to the same content hash and delivery. Older unchanged picks are
	// suppressed from the next weekly-style digest.
	rows, err := r.store.Pool.Query(ctx, `SELECT edr.game_id,edr.content_hash FROM email_delivery_recommendations edr JOIN email_deliveries ed ON ed.id=edr.delivery_id WHERE ed.user_id=$1 AND ed.status IN ('sent','ambiguous') AND ed.created_at < date_trunc('week',now())`, userID)
	if err != nil {
		return DigestInput{}, err
	}
	for rows.Next() {
		var gameID uuid.UUID
		var hash string
		if err := rows.Scan(&gameID, &hash); err != nil {
			rows.Close()
			return DigestInput{}, err
		}
		if sent[gameID] == nil {
			sent[gameID] = map[string]bool{}
		}
		sent[gameID][hash] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return DigestInput{}, err
	}
	rows.Close()
	rows, err = r.store.Pool.Query(ctx, `SELECT r.id,r.game_id,g.name,r.score,COALESCE(r.explanation,''),d.precision,d.release_date,d.raw_text,
		c.review_band,c.review_label,c.positive_percent,c.review_total
		FROM recommendations r JOIN games g ON g.id=r.game_id
		LEFT JOIN LATERAL (SELECT precision,release_date,raw_text FROM game_release_dates WHERE game_id=g.id ORDER BY observed_at DESC,id DESC LIMIT 1) d ON true
		LEFT JOIN catalog_search_current c ON c.steam_app_id=g.steam_app_id
		WHERE r.user_id=$1 AND r.ranking_version=(SELECT max(ranking_version) FROM recommendation_runs WHERE user_id=$1 AND published)
		ORDER BY r.score DESC,lower(g.name),g.id`, userID)
	if err != nil {
		return DigestInput{}, err
	}
	var recommendationCandidates []DigestRecommendation
	for rows.Next() {
		var item DigestRecommendation
		var precision *string
		var date *time.Time
		var raw *string
		var reviewBand, reviewLabel *string
		var reviewPercent *int32
		var reviewTotal *int64
		if err := rows.Scan(&item.ID, &item.GameID, &item.Name, &item.Score, &item.Explanation, &precision, &date, &raw,
			&reviewBand, &reviewLabel, &reviewPercent, &reviewTotal); err != nil {
			rows.Close()
			return DigestInput{}, err
		}
		item.Release = formatRelease(precision, date, raw)
		item.Reviews = validDigestReviewSummary(reviewBand, reviewLabel, reviewPercent, reviewTotal)
		recommendationCandidates = append(recommendationCandidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return DigestInput{}, err
	}
	rows.Close()
	recommendationIDs := make([]uuid.UUID, 0, len(recommendationCandidates))
	for _, item := range recommendationCandidates {
		recommendationIDs = append(recommendationIDs, item.GameID)
	}
	allowed, err := r.eligibility.FilterEligibleGameIDs(ctx, recommendationIDs, r.clock().UTC())
	if err != nil {
		return DigestInput{}, err
	}
	for _, item := range recommendationCandidates {
		if !allowed[item.GameID] {
			continue
		}
		item.ContentHash = digestRecommendationContentHash(item)
		if !sent[item.GameID][item.ContentHash] && len(input.Recommendations) < 5 {
			input.Recommendations = append(input.Recommendations, item)
		}
	}
	rows, err = r.store.Pool.Query(ctx, `SELECT g.id,g.name,d.precision,d.release_date,d.raw_text FROM wishlist_entries w JOIN games g ON g.id=w.game_id
		LEFT JOIN LATERAL (SELECT precision,release_date,raw_text FROM game_release_dates WHERE game_id=g.id ORDER BY observed_at DESC,id DESC LIMIT 1) d ON true
		WHERE w.user_id=$1 ORDER BY d.release_date NULLS LAST,lower(g.name),g.id`, userID)
	if err != nil {
		return DigestInput{}, err
	}
	defer rows.Close()
	var wishlistCandidates []DigestWishlist
	for rows.Next() {
		var item DigestWishlist
		var precision *string
		var date *time.Time
		var raw *string
		if err := rows.Scan(&item.GameID, &item.Name, &precision, &date, &raw); err != nil {
			return DigestInput{}, err
		}
		item.Release = formatRelease(precision, date, raw)
		wishlistCandidates = append(wishlistCandidates, item)
	}
	if err := rows.Err(); err != nil {
		return DigestInput{}, err
	}
	wishlistIDs := make([]uuid.UUID, 0, len(wishlistCandidates))
	for _, item := range wishlistCandidates {
		wishlistIDs = append(wishlistIDs, item.GameID)
	}
	allowed, err = r.eligibility.FilterEligibleGameIDs(ctx, wishlistIDs, r.clock().UTC())
	if err != nil {
		return DigestInput{}, err
	}
	for _, item := range wishlistCandidates {
		if allowed[item.GameID] && len(input.Wishlist) < 10 {
			input.Wishlist = append(input.Wishlist, item)
		}
	}
	return input, nil
}

func validDigestReviewSummary(band, label *string, percent *int32, total *int64) *DigestReviewSummary {
	if band == nil || label == nil || total == nil || strings.TrimSpace(*label) == "" || *total < 0 {
		return nil
	}
	reviews := &DigestReviewSummary{Band: *band, Label: strings.TrimSpace(*label), PositivePercent: percent, Total: *total}
	if digestReviewLabel(reviews) == "" {
		return nil
	}
	return reviews
}

func formatRelease(precision *string, date *time.Time, raw *string) string {
	if precision == nil {
		return "Release date unknown"
	}
	if raw != nil && strings.TrimSpace(*raw) != "" && *precision != "day" {
		return strings.TrimSpace(*raw)
	}
	if date != nil {
		switch *precision {
		case "day":
			return date.UTC().Format("2 Jan 2006")
		case "month":
			return date.UTC().Format("January 2006")
		case "year":
			return date.UTC().Format("2006")
		}
	}
	switch *precision {
	case "coming_soon":
		return "Coming Soon"
	case "quarter":
		if raw != nil {
			return *raw
		}
		return "Upcoming quarter"
	}
	return "Release date unknown"
}

func (r *SQLRepository) CreateDelivery(ctx context.Context, input NewDelivery) (Delivery, bool, error) {
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Delivery{}, false, err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `INSERT INTO email_deliveries(id,user_id,idempotency_key,status,kind,content_hash,recipient,subject,text_body,html_body) VALUES($1,$2,$3,'pending',$4,$5,$6,$7,$8,$9) ON CONFLICT(idempotency_key) DO NOTHING`, input.ID, input.UserID, input.IdempotencyKey, input.Kind, input.ContentHash, input.Recipient, input.Subject, input.Text, input.HTML)
	if err != nil {
		return Delivery{}, false, err
	}
	created := command.RowsAffected() == 1
	if created {
		for _, recommendation := range input.Recommendations {
			if _, err := tx.Exec(ctx, `INSERT INTO email_delivery_recommendations(delivery_id,recommendation_id,game_id,content_hash)VALUES($1,$2,$3,$4)`, input.ID, recommendation.RecommendationID, recommendation.GameID, recommendation.ContentHash); err != nil {
				return Delivery{}, false, err
			}
		}
		for _, action := range input.Actions {
			if _, err := tx.Exec(ctx, `INSERT INTO email_action_nonces(nonce,delivery_id,user_id,recommendation_id,action,issued_at,expires_at)VALUES($1,$2,$3,$4,$5,$6,$7)`, action.Nonce, input.ID, action.UserID, action.RecommendationID, action.Action, action.IssuedAt, action.ExpiresAt); err != nil {
				return Delivery{}, false, err
			}
		}
	}
	var delivery Delivery
	if created {
		delivery = input.Delivery
	} else {
		if err := tx.QueryRow(ctx, `SELECT id,user_id,status,recipient,subject,text_body,html_body FROM email_deliveries WHERE idempotency_key=$1`, input.IdempotencyKey).Scan(&delivery.ID, &delivery.UserID, &delivery.Status, &delivery.Recipient, &delivery.Subject, &delivery.Text, &delivery.HTML); err != nil {
			return Delivery{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Delivery{}, false, err
	}
	return delivery, created, nil
}

func (r *SQLRepository) ClaimDelivery(ctx context.Context, id uuid.UUID, worker string, token uuid.UUID) (Delivery, error) {
	var delivery Delivery
	if _, err := r.store.Pool.Exec(ctx, `UPDATE email_deliveries ed SET status='ambiguous',error='delivery attempt ended without durable SMTP outcome',attempt_worker_id=NULL,attempt_token=NULL,attempt_started_at=NULL,updated_at=now()
		WHERE ed.id=$1 AND ed.status='sending' AND NOT EXISTS (
			SELECT 1 FROM sync_jobs j WHERE j.idempotency_key='digest-delivery:'||ed.id::text AND j.status='running' AND j.fencing_token=ed.attempt_token AND j.lease_until>now()
		)`, id); err != nil {
		return Delivery{}, err
	}
	err := r.store.Pool.QueryRow(ctx, `UPDATE email_deliveries SET status='sending',attempt_worker_id=$2,attempt_token=$3,attempt_started_at=now(),error=NULL,updated_at=now() WHERE id=$1 AND status IN ('pending','failed') RETURNING id,user_id,status,recipient,subject,text_body,html_body`, id, worker, token).Scan(&delivery.ID, &delivery.UserID, &delivery.Status, &delivery.Recipient, &delivery.Subject, &delivery.Text, &delivery.HTML)
	if errors.Is(err, pgx.ErrNoRows) {
		if err = r.store.Pool.QueryRow(ctx, `SELECT id,user_id,status,recipient,subject,text_body,html_body FROM email_deliveries WHERE id=$1`, id).Scan(&delivery.ID, &delivery.UserID, &delivery.Status, &delivery.Recipient, &delivery.Subject, &delivery.Text, &delivery.HTML); err != nil {
			return Delivery{}, err
		}
		if delivery.Status == "sending" {
			return Delivery{}, ErrDeliveryOwned
		}
		return delivery, nil
	}
	return delivery, err
}
func (r *SQLRepository) MarkSent(ctx context.Context, id uuid.UUID, worker string, token uuid.UUID) error {
	return r.ownedUpdate(ctx, `UPDATE email_deliveries SET status='sent',accepted_at=now(),confirmation_token=$3,attempt_worker_id=NULL,attempt_token=NULL,attempt_started_at=NULL,error=NULL,updated_at=now() WHERE id=$1 AND status='sending' AND attempt_worker_id=$2 AND attempt_token=$3`, id, worker, token)
}
func (r *SQLRepository) MarkFailed(ctx context.Context, id uuid.UUID, worker string, token uuid.UUID, cause error) error {
	return r.ownedUpdate(ctx, `UPDATE email_deliveries SET status='failed',attempt_worker_id=NULL,attempt_token=NULL,attempt_started_at=NULL,error=$4,updated_at=now() WHERE id=$1 AND status='sending' AND attempt_worker_id=$2 AND attempt_token=$3`, id, worker, token, cause.Error())
}
func (r *SQLRepository) MarkAmbiguous(ctx context.Context, id uuid.UUID, worker string, token uuid.UUID, cause error) error {
	return r.ownedUpdate(ctx, `UPDATE email_deliveries SET status='ambiguous',accepted_at=COALESCE(accepted_at,now()),confirmation_token=$3,attempt_worker_id=NULL,attempt_token=NULL,attempt_started_at=NULL,error=$4,updated_at=now() WHERE id=$1 AND ((status='sending' AND attempt_worker_id=$2 AND attempt_token=$3) OR (status='sent' AND confirmation_token=$3))`, id, worker, token, cause.Error())
}
func (r *SQLRepository) ReviveFailedDeliveryJob(ctx context.Context, id uuid.UUID) error {
	_, err := r.store.Pool.Exec(ctx, `UPDATE sync_jobs j SET status='retryable',attempts=0,available_at=now(),worker_id=NULL,fencing_token=NULL,lease_until=NULL,last_error=NULL,updated_at=now()
		FROM email_deliveries ed WHERE ed.id=$1 AND ed.idempotency_key IS NOT NULL AND j.idempotency_key='digest-delivery:'||ed.id::text AND j.status='dead' AND ed.status IN ('pending','failed')`, id)
	return err
}
func (r *SQLRepository) ownedUpdate(ctx context.Context, query string, args ...any) error {
	result, err := r.store.Pool.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrDeliveryOwned
	}
	return nil
}

func (r *SQLRepository) ConsumeAction(ctx context.Context, action Action) (bool, error) {
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var userID, recommendationID uuid.UUID
	var storedAction string
	var issuedAt, expiresAt time.Time
	var consumedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT user_id,recommendation_id,action,issued_at,expires_at,consumed_at FROM email_action_nonces WHERE nonce=$1 FOR UPDATE`, action.Nonce).Scan(&userID, &recommendationID, &storedAction, &issuedAt, &expiresAt, &consumedAt); err != nil {
		return false, err
	}
	if userID != action.UserID || recommendationID != action.RecommendationID || storedAction != action.Action || !issuedAt.Equal(action.IssuedAt) || !expiresAt.Equal(action.ExpiresAt) {
		return false, ErrInvalidToken
	}
	if consumedAt != nil {
		return true, tx.Commit(ctx)
	}
	feedbackID := uuid.New()
	idempotencyKey := "email-action:" + action.Nonce.String()
	command, err := tx.Exec(ctx, `INSERT INTO feedback_events(id,recommendation_id,user_id,action,idempotency_key,recommendation_snapshot,previous_ranking_version)
		SELECT $1,r.id,$2,$3,$4,jsonb_build_object('recommendationId',r.id,'gameId',r.game_id,'score',r.score,'explanation',COALESCE(r.explanation,'')),r.ranking_version
		FROM recommendations r WHERE r.id=$5 AND r.user_id=$2 ON CONFLICT(idempotency_key) DO NOTHING`, feedbackID, action.UserID, action.Action, idempotencyKey, action.RecommendationID)
	if err != nil {
		return false, err
	}
	if command.RowsAffected() != 1 {
		return false, pgx.ErrNoRows
	}
	command, err = tx.Exec(ctx, `UPDATE email_action_nonces SET consumed_at=now(),feedback_event_id=$2 WHERE nonce=$1 AND consumed_at IS NULL`, action.Nonce, feedbackID)
	if err != nil {
		return false, err
	}
	if command.RowsAffected() != 1 {
		return false, ErrInvalidToken
	}
	return false, tx.Commit(ctx)
}

var _ Repository = (*SQLRepository)(nil)
