package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
	dbgen "next-game-crush/internal/store/sqlc"
)

// EnqueueReadySearchQualifications moves each ready worklist row from pending
// to reserved and publishes its immutable queue identity in the same database
// transaction. A crash can therefore expose both or neither.
func (r *SearchRepository) EnqueueReadySearchQualifications(ctx context.Context, queue *jobs.Queue, cfg DiscoveryConfig, now time.Time) (int, error) {
	return r.enqueueReadySearchQualifications(ctx, queue, cfg, now, false)
}

func (r *SearchRepository) EnqueueReadyIndependentSearchQualifications(ctx context.Context, queue *jobs.Queue, cfg DiscoveryConfig, now time.Time) (int, error) {
	return r.enqueueReadySearchQualifications(ctx, queue, cfg, now, true)
}

func (r *SearchRepository) enqueueReadySearchQualifications(ctx context.Context, queue *jobs.Queue, cfg DiscoveryConfig, now time.Time, independentOnly bool) (int, error) {
	if r == nil || r.store == nil || queue == nil || cfg.Validate() != nil || now.IsZero() {
		return 0, errors.New("invalid search qualification producer")
	}
	count := 0
	for {
		enqueued, retry, err := r.enqueueOneSearchQualification(ctx, cfg, now.UTC(), independentOnly)
		if err != nil {
			return count, err
		}
		if retry {
			continue
		}
		if !enqueued {
			return count, nil
		}
		count++
	}
}

type searchQualificationPeek struct {
	RunID              pgtype.UUID
	SteamAppID         int64
	RunRevision        int64
	ObservationOrdinal int64
	Priority           int16
	Fingerprint        string
}

func (r *SearchRepository) enqueueOneSearchQualification(ctx context.Context, cfg DiscoveryConfig, now time.Time, independentOnly bool) (enqueued, retry bool, err error) {
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	var peek searchQualificationPeek
	if independentOnly {
		today := utcCalendarDay(now)
		row, peekErr := q.PeekNextIndependentMetadataWork(ctx, dbgen.PeekNextIndependentMetadataWorkParams{
			NowAt:       pgtype.Timestamptz{Time: now, Valid: true},
			WindowStart: pgtype.Date{Time: today.AddDate(0, 0, -cfg.LookbackDays), Valid: true},
			WindowEnd:   pgtype.Date{Time: today.AddDate(0, 0, cfg.HorizonDays), Valid: true},
			Today:       pgtype.Date{Time: today, Valid: true},
		})
		err = peekErr
		peek = searchQualificationPeek{RunID: row.RunID, SteamAppID: row.SteamAppID, RunRevision: row.RunRevision,
			ObservationOrdinal: row.ObservationOrdinal, Priority: row.Priority, Fingerprint: row.Fingerprint}
	} else {
		row, peekErr := q.PeekNextMetadataWork(ctx, pgtype.Timestamptz{Time: now, Valid: true})
		err = peekErr
		peek = searchQualificationPeek{RunID: row.RunID, SteamAppID: row.SteamAppID, RunRevision: row.RunRevision,
			ObservationOrdinal: row.ObservationOrdinal, Priority: row.Priority, Fingerprint: row.Fingerprint}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, tx.Commit(ctx)
	}
	if err != nil {
		return false, false, err
	}
	if _, err := lockMetadataCandidate(ctx, q, peek.SteamAppID); err != nil {
		return false, false, err
	}
	work, err := q.ClaimMetadataWork(ctx, dbgen.ClaimMetadataWorkParams{RunID: peek.RunID, SteamAppID: peek.SteamAppID, NowAt: pgtype.Timestamptz{Time: now, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, true, nil
	}
	if err != nil {
		return false, false, err
	}
	revision := CandidateRevision{RunRevision: work.RunRevision, ObservationOrdinal: work.ObservationOrdinal}
	handled, err := r.resolveSearchCandidateWithoutStore(ctx, tx, q, work.RunID, work.SteamAppID, revision, cfg, now)
	if err != nil {
		return false, false, err
	}
	if handled {
		if err := tx.Commit(ctx); err != nil {
			return false, false, err
		}
		return false, true, nil
	}
	reservation, granted, next, err := processMetadataCandidate(ctx, q, work.SteamAppID, revision, work.Fingerprint, cfg.AppDetailsHourlyBudget, now)
	if err != nil {
		return false, false, err
	}
	if !granted {
		if next.IsZero() {
			_, err = q.MarkMetadataWorkSuperseded(ctx, dbgen.MarkMetadataWorkSupersededParams{RunID: work.RunID, SteamAppID: work.SteamAppID})
		} else {
			_, err = q.DeferMetadataWork(ctx, dbgen.DeferMetadataWorkParams{NextRetryAt: pgtype.Timestamptz{Time: next, Valid: true}, RunID: work.RunID, SteamAppID: work.SteamAppID})
		}
		if err != nil {
			return false, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, false, err
		}
		return false, next.IsZero(), nil
	}
	if _, err := q.MarkMetadataWorkReserved(ctx, dbgen.MarkMetadataWorkReservedParams{RunID: work.RunID, SteamAppID: work.SteamAppID}); err != nil {
		return false, false, err
	}
	reserved, err := q.GetStoreReservation(ctx, work.SteamAppID)
	if err != nil {
		return false, false, err
	}
	payload := SearchQualificationPayload{Version: SearchQualificationPayloadVersion, AppID: work.SteamAppID,
		CandidateRevision: revision, ReservationID: reservation.ID, ReservationToken: reservation.FencingToken,
		RetryGeneration: int(reserved.RetryGeneration), Priority: int(work.Priority)}
	spec, err := NewSearchQualificationSpec(payload, uuid.Nil)
	if err != nil {
		return false, false, err
	}
	if _, err := jobs.EnqueueInTx(ctx, q, spec); err != nil {
		return false, false, err
	}
	return true, false, tx.Commit(ctx)
}

func (r *SearchRepository) resolveSearchCandidateWithoutStore(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, runID pgtype.UUID, appID int64, revision CandidateRevision, cfg DiscoveryConfig, now time.Time) (bool, error) {
	current, err := q.LockCurrentMetadataCandidate(ctx, appID)
	if err != nil {
		return false, err
	}
	window := ParseReleaseDate(current.ReleaseText, now)
	finish := func(reason string, next time.Time) (bool, error) {
		if game, err := q.GetGameByAppID(ctx, appID); err == nil {
			changed, err := setSharedSource(ctx, tx, uuid.UUID(game.ID.Bytes), "steam_discovery", false, now)
			if err != nil {
				return false, err
			}
			if changed {
				if err := NewDiscoveryRepository(r.store).MarkCandidateSetDirtyAt(ctx, tx, EligibilityChange{GameID: uuid.UUID(game.ID.Bytes), Kind: EligibilityChangeMembership}, now); err != nil {
					return false, err
				}
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE catalog_search_current SET active=false,tombstone_reason=$2,
			next_refresh_at=$3,next_eligibility_at=NULL WHERE steam_app_id=$1
			AND material_run_revision=$4 AND material_observation_ordinal=$5`, appID, reason, next, revision.RunRevision, revision.ObservationOrdinal); err != nil {
			return false, err
		}
		if _, err := q.CompleteSearchQualificationCandidate(ctx, dbgen.CompleteSearchQualificationCandidateParams{RunID: runID, SteamAppID: appID}); err != nil {
			return false, err
		}
		return true, nil
	}
	// A successful canonical snapshot may be re-evaluated from fresh review or
	// membership data without another Store call. Release/review presentation
	// changes never create another semantic AI identity.
	if current.LastSuccessfulAppdetailsAt.Valid && current.NextRefreshAt.Valid && current.NextRefreshAt.Time.After(now) {
		game, err := q.GetGameByAppID(ctx, appID)
		if err == nil {
			details, valid := validatedStoredSteamDetails(game)
			if valid {
				popularUpcoming, popularNew, wishlisted, err := searchEligibilityProvenance(ctx, tx, appID)
				if err != nil {
					return false, err
				}
				canonicalWindow := ParseReleaseDate(details.ReleaseDate.Raw, now)
				if details.ReleaseDate.ComingSoon && strings.TrimSpace(details.ReleaseDate.Raw) == "" {
					canonicalWindow.Precision = PrecisionComingSoon
				}
				decision := EvaluateEligibility(EligibilityInput{Now: now, Window: canonicalWindow, ComingSoon: details.ReleaseDate.ComingSoon,
					Review: reviewFromCurrent(current), PopularUpcoming: popularUpcoming, PopularNew: popularNew, Wishlisted: wishlisted}, cfg)
				if decision.Active && !current.Active {
					var enrichmentKnown bool
					prefix := "catalog:enrichment:v1:" + uuid.UUID(game.ID.Bytes).String() + ":"
					if err := tx.QueryRow(ctx, `SELECT
						EXISTS(SELECT 1 FROM game_features WHERE game_id=$1 AND input_content_hash=$2)
						OR EXISTS(SELECT 1 FROM sync_jobs WHERE parent_job_id IS NULL AND idempotency_key LIKE $3||'%')`,
						game.ID, current.SemanticContentHash, prefix).Scan(&enrichmentKnown); err != nil {
						return false, err
					}
					if !enrichmentKnown {
						// A candidate first seen behind a hard review tombstone has no
						// cached AI execution to reuse. Make Store due so the normal
						// fenced handler can publish canonical state and plan AI once.
						if _, err := tx.Exec(ctx, `UPDATE catalog_search_current SET next_refresh_at=$2 WHERE steam_app_id=$1`, appID, now); err != nil {
							return false, err
						}
						return false, nil
					}
				}
				changed, err := setSharedSource(ctx, tx, uuid.UUID(game.ID.Bytes), "steam_discovery", decision.Active, now)
				if err != nil {
					return false, err
				}
				nextEligibility := pgtype.Timestamptz{}
				if decision.NextChangeAt != nil {
					nextEligibility = pgtype.Timestamptz{Time: decision.NextChangeAt.UTC(), Valid: true}
				}
				if _, err := tx.Exec(ctx, `UPDATE catalog_search_current SET active=$2,tombstone_reason=$3,next_eligibility_at=$4
					WHERE steam_app_id=$1`, appID, decision.Active, inactiveReason(decision), nextEligibility); err != nil {
					return false, err
				}
				if changed {
					if err := NewDiscoveryRepository(r.store).MarkCandidateSetDirtyAt(ctx, tx, EligibilityChange{GameID: uuid.UUID(game.ID.Bytes), Kind: EligibilityChangeReviewEligibility, NextChangeAt: decision.NextChangeAt}, now); err != nil {
						return false, err
					}
				}
				if _, err := q.CompleteSearchQualificationCandidate(ctx, dbgen.CompleteSearchQualificationCandidateParams{RunID: runID, SteamAppID: appID}); err != nil {
					return false, err
				}
				return true, nil
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	}
	today := utcCalendarDay(now)
	start, end, known := window.Interval()
	if known && (end.Before(today.AddDate(0, 0, -cfg.LookbackDays)) || start.After(today.AddDate(0, 0, cfg.HorizonDays))) {
		return finish("outside_window", now.Add(30*24*time.Hour))
	}
	if known && end.Before(today) {
		switch reviewFromCurrent(current).Band {
		case steam.ReviewMixedOrBelow:
			return finish("mixed_or_below", now.Add(30*24*time.Hour))
		case steam.ReviewUnknown:
			return finish("review_unknown", now.Add(24*time.Hour))
		case steam.ReviewUnrated:
			if window.Precision != PrecisionDay || window.Date == nil || today.After(utcCalendarDay(*window.Date).AddDate(0, 0, 7)) {
				return finish("unrated_grace_expired", now.Add(30*24*time.Hour))
			}
			var popularNew bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_search_memberships
				WHERE steam_app_id=$1 AND feed='popular_new' AND active)`, appID).Scan(&popularNew); err != nil {
				return false, err
			}
			if !popularNew {
				return finish("unrated_not_popular_new", now.Add(30*24*time.Hour))
			}
		}
	}
	return false, nil
}

func (r *DiscoveryRepository) persistSearchQualificationFailure(ctx context.Context, lease jobs.Lease, payload SearchQualificationPayload, reason string, now time.Time) error {
	if reason != "unavailable" && reason != "request_failed" && reason != "malformed" {
		return errors.New("invalid search qualification failure")
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := lockDiscoveryLease(ctx, tx, lease); err != nil {
		return err
	}
	owned, current, err := lockSearchQualificationState(ctx, tx, payload)
	if err != nil {
		return err
	}
	if !owned {
		return commitDiscoveryLease(ctx, tx, lease)
	}
	terminal := lease.Attempts >= lease.MaxAttempts
	q := r.store.Queries.WithTx(tx)
	hasLastKnownGood := false
	if current {
		state, stateErr := q.LockCurrentMetadataCandidate(ctx, payload.AppID)
		if stateErr != nil {
			return stateErr
		}
		hasLastKnownGood = state.LastSuccessfulAppdetailsAt.Valid
		if !hasLastKnownGood {
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM games
				WHERE steam_app_id=$1 AND source_snapshot<>'{}'::jsonb)`, payload.AppID).Scan(&hasLastKnownGood); err != nil {
				return err
			}
		}
	}
	due := now.Add(searchQualificationRetryDelay(payload.RetryGeneration))
	nextGeneration := payload.RetryGeneration + 1
	if current {
		_, err = tx.Exec(ctx, `UPDATE catalog_search_current SET retry_attempts=GREATEST(retry_attempts,$2),
			retry_generation=CASE WHEN $5 THEN GREATEST(retry_generation,$3) ELSE retry_generation END,safe_error=$4,
			next_retry_at=CASE WHEN $5 THEN $6 ELSE next_retry_at END,
			active=CASE WHEN $5 AND NOT $7 AND $4 IN ('unavailable','malformed') THEN false ELSE active END,
			tombstone_reason=CASE WHEN $5 AND NOT $7 AND $4 IN ('unavailable','malformed') THEN $4 ELSE tombstone_reason END
			WHERE steam_app_id=$1`, payload.AppID, lease.Attempts, nextGeneration, safeSearchFailure(reason), terminal, due, hasLastKnownGood)
		if err != nil {
			return err
		}
	}
	if terminal {
		if !hasLastKnownGood && (reason == "unavailable" || reason == "malformed") {
			if game, getErr := q.GetGameByAppID(ctx, payload.AppID); getErr == nil {
				changed, sourceErr := setSharedSource(ctx, tx, uuid.UUID(game.ID.Bytes), "steam_discovery", false, now)
				if sourceErr != nil {
					return sourceErr
				}
				if changed {
					if err := r.MarkCandidateSetDirtyAt(ctx, tx, EligibilityChange{GameID: uuid.UUID(game.ID.Bytes), Kind: EligibilityChangeMembership}, now); err != nil {
						return err
					}
				}
			} else if !errors.Is(getErr, pgx.ErrNoRows) {
				return getErr
			}
		}
		rows, err := q.FailSearchQualificationReservation(ctx, dbgen.FailSearchQualificationReservationParams{
			RetryGeneration: int32(nextGeneration), NextRetryAt: timestamptz(due), LeaseUntil: timestamptz(due.Add(metadataReservationLease)),
			NowAt: timestamptz(now), SteamAppID: payload.AppID, ID: catalogUUID(payload.ReservationID), FencingToken: catalogUUID(payload.ReservationToken)})
		if err != nil {
			return err
		}
		if rows != 1 {
			return errors.New("search qualification reservation retry fence lost")
		}
		rows, err = q.FailSearchQualificationWork(ctx, dbgen.FailSearchQualificationWorkParams{NextRetryAt: timestamptz(due), SteamAppID: payload.AppID,
			RunRevision: payload.CandidateRevision.RunRevision, ObservationOrdinal: payload.CandidateRevision.ObservationOrdinal})
		if err != nil {
			return err
		}
		if rows < 1 {
			return errors.New("search qualification work retry fence lost")
		}
		nextPayload := payload
		nextPayload.RetryGeneration = nextGeneration
		spec, err := NewSearchQualificationSpec(nextPayload, uuid.Nil)
		if err != nil {
			return err
		}
		spec.AvailableAt = due
		if _, err := jobs.EnqueueInTx(ctx, q, spec); err != nil {
			return err
		}
	}
	return commitDiscoveryLease(ctx, tx, lease)
}

func searchQualificationRetryDelay(generation int) time.Duration {
	if generation < 0 {
		generation = 0
	}
	if generation >= 5 {
		return 24 * time.Hour
	}
	return time.Hour * time.Duration(1<<generation)
}

func safeSearchFailure(reason string) string {
	if reason == "malformed" || reason == "unavailable" {
		return reason
	}
	return "request_failed"
}

func lockSearchQualificationState(ctx context.Context, tx pgx.Tx, payload SearchQualificationPayload) (owned, current bool, err error) {
	if err = lockDiscoveryApp(ctx, tx, payload.AppID); err != nil {
		return false, false, err
	}
	var reservationID, token uuid.UUID
	var runRevision, ordinal int64
	var status, fingerprint string
	var retryGeneration int32
	row, queryErr := rQueries(tx).LockSearchQualificationReservation(ctx, payload.AppID)
	err = queryErr
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	reservationID, token = uuid.UUID(row.ID.Bytes), uuid.UUID(row.FencingToken.Bytes)
	runRevision, ordinal, status, fingerprint = row.RunRevision, row.ObservationOrdinal, row.Status, row.Fingerprint
	if reservationID != payload.ReservationID || token != payload.ReservationToken || status != "reserved" ||
		runRevision != payload.CandidateRevision.RunRevision || ordinal != payload.CandidateRevision.ObservationOrdinal {
		return false, false, nil
	}
	retryGeneration = row.RetryGeneration
	if int(retryGeneration) != payload.RetryGeneration {
		return false, false, nil
	}
	var headRun, headOrdinal int64
	var headFingerprint string
	if err = tx.QueryRow(ctx, `SELECT material_run_revision,material_observation_ordinal,material_fingerprint
		FROM catalog_metadata_heads WHERE steam_app_id=$1 FOR UPDATE`, payload.AppID).Scan(&headRun, &headOrdinal, &headFingerprint); err != nil {
		return false, false, err
	}
	if headRun != runRevision || headOrdinal != ordinal || headFingerprint != fingerprint {
		return false, false, nil
	}
	var currentRun, currentOrdinal int64
	var currentFingerprint string
	err = tx.QueryRow(ctx, `SELECT material_run_revision,material_observation_ordinal,material_fingerprint
		FROM catalog_search_current WHERE steam_app_id=$1 FOR UPDATE`, payload.AppID).
		Scan(&currentRun, &currentOrdinal, &currentFingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	current = currentRun == runRevision && currentOrdinal == ordinal && currentFingerprint == fingerprint
	return current, current, nil
}

func (r *DiscoveryRepository) persistSearchQualification(ctx context.Context, lease jobs.Lease, payload SearchQualificationPayload, details steam.GameDetails, now time.Time, cfg DiscoveryConfig, planner SearchDiscoveryEnrichmentPlanner) error {
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := lockDiscoveryLease(ctx, tx, lease); err != nil {
		return err
	}
	owned, _, err := lockSearchQualificationState(ctx, tx, payload)
	if err != nil {
		return err
	}
	if !owned {
		return commitDiscoveryLease(ctx, tx, lease)
	}
	q := r.store.Queries.WithTx(tx)
	current, err := q.LockCurrentMetadataCandidate(ctx, payload.AppID)
	if err != nil {
		return err
	}
	review := reviewFromCurrent(current)
	popularUpcoming, popularNew, wishlisted, err := searchEligibilityProvenance(ctx, tx, payload.AppID)
	if err != nil {
		return err
	}
	window := ParseReleaseDate(details.ReleaseDate.Raw, now)
	if details.ReleaseDate.ComingSoon && strings.TrimSpace(details.ReleaseDate.Raw) == "" {
		window.Precision = PrecisionComingSoon
	}
	decision := EligibilityDecision{Reason: "not_game", Phase: "released"}
	if strings.EqualFold(strings.TrimSpace(details.Type), "game") {
		decision = EvaluateEligibility(EligibilityInput{Now: now, Window: window, ComingSoon: details.ReleaseDate.ComingSoon,
			Review: review, PopularUpcoming: popularUpcoming, PopularNew: popularNew, Wishlisted: wishlisted}, cfg)
	}
	semanticHash := SemanticContentHash(details)
	var gameID uuid.UUID
	if strings.EqualFold(strings.TrimSpace(details.Type), "game") {
		gameID, err = r.persistSearchCanonicalGame(ctx, tx, q, details, window, semanticHash, now)
		if err != nil {
			return err
		}
		changed, err := setSharedSource(ctx, tx, gameID, "steam_discovery", decision.Active, now)
		if err != nil {
			return err
		}
		if decision.Active && planner != nil {
			if err := enqueueSearchDiscoveryFeatures(ctx, q, planner, gameID, details, payload.Priority); err != nil {
				return err
			}
		}
		if changed {
			if err := r.MarkCandidateSetDirtyAt(ctx, tx, EligibilityChange{GameID: gameID, Kind: EligibilityChangeMembership, NextChangeAt: decision.NextChangeAt}, now); err != nil {
				return err
			}
		} else if current.SemanticContentHash != "" && current.SemanticContentHash != semanticHash {
			if err := r.MarkCandidateSetDirtyAt(ctx, tx, EligibilityChange{GameID: gameID, Kind: EligibilityChangeSemanticContent, NextChangeAt: decision.NextChangeAt}, now); err != nil {
				return err
			}
		}
	} else if old, getErr := q.GetGameByAppID(ctx, payload.AppID); getErr == nil {
		changed, sourceErr := setSharedSource(ctx, tx, uuid.UUID(old.ID.Bytes), "steam_discovery", false, now)
		if sourceErr != nil {
			return sourceErr
		}
		if changed {
			if err := r.MarkCandidateSetDirtyAt(ctx, tx, EligibilityChange{GameID: uuid.UUID(old.ID.Bytes), Kind: EligibilityChangeMembership}, now); err != nil {
				return err
			}
		}
	} else if !errors.Is(getErr, pgx.ErrNoRows) {
		return getErr
	}
	precision, releaseDate, year, month, quarter := releaseWindowColumns(window)
	nextEligibility := pgtype.Timestamptz{}
	if decision.NextChangeAt != nil {
		nextEligibility = pgtype.Timestamptz{Time: decision.NextChangeAt.UTC(), Valid: true}
	}
	ttl := MetadataRefreshTTL(window, inactiveReason(decision), payload.RetryGeneration, now)
	_, err = tx.Exec(ctx, `UPDATE catalog_search_current SET coming_soon=$2,precision=$3,release_date=$4,
		release_year=$5,release_month=$6,release_quarter=$7,last_successful_appdetails_at=$8,
		next_refresh_at=$9,semantic_content_hash=$10,active=$11,tombstone_reason=$12,
		retry_generation=0,retry_attempts=0,next_retry_at=NULL,next_eligibility_at=$13,safe_error=''
		WHERE steam_app_id=$1 AND material_run_revision=$14 AND material_observation_ordinal=$15`,
		payload.AppID, details.ReleaseDate.ComingSoon, precision, releaseDate, year, month, quarter,
		now, now.Add(ttl), semanticHash, decision.Active, inactiveReason(decision), nextEligibility,
		payload.CandidateRevision.RunRevision, payload.CandidateRevision.ObservationOrdinal)
	if err != nil {
		return err
	}
	rows, err := q.CompleteSearchQualificationReservation(ctx, dbgen.CompleteSearchQualificationReservationParams{NowAt: timestamptz(now), SteamAppID: payload.AppID,
		ID: catalogUUID(payload.ReservationID), FencingToken: catalogUUID(payload.ReservationToken)})
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("search qualification completion reservation fence lost")
	}
	rows, err = q.CompleteSearchQualificationWork(ctx, dbgen.CompleteSearchQualificationWorkParams{SteamAppID: payload.AppID,
		RunRevision: payload.CandidateRevision.RunRevision, ObservationOrdinal: payload.CandidateRevision.ObservationOrdinal})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("search qualification completion work fence lost")
	}
	return commitDiscoveryLease(ctx, tx, lease)
}

func reviewFromCurrent(current dbgen.CatalogSearchCurrent) steam.ReviewSummary {
	result := steam.ReviewSummary{Band: steam.ReviewBand(current.ReviewBand), Label: current.ReviewLabel, Total: current.ReviewTotal}
	if current.PositivePercent.Valid {
		value := int(current.PositivePercent.Int32)
		result.PositivePercent = &value
	}
	return result
}

func searchEligibilityProvenance(ctx context.Context, tx pgx.Tx, appID int64) (popularUpcoming, popularNew, wishlisted bool, err error) {
	err = tx.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM catalog_search_memberships WHERE steam_app_id=$1 AND feed='popular_upcoming' AND active),
		EXISTS(SELECT 1 FROM catalog_search_memberships WHERE steam_app_id=$1 AND feed='popular_new' AND active),
		EXISTS(SELECT 1 FROM wishlist_entries w JOIN games g ON g.id=w.game_id WHERE g.steam_app_id=$1)`, appID).
		Scan(&popularUpcoming, &popularNew, &wishlisted)
	return
}

func (r *DiscoveryRepository) persistSearchCanonicalGame(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, details steam.GameDetails, window ReleaseWindow, semanticHash string, now time.Time) (uuid.UUID, error) {
	snapshot, err := json.Marshal(details)
	if err != nil {
		return uuid.Nil, errors.New("invalid search qualification snapshot")
	}
	gameID := uuid.New()
	if old, err := q.GetCatalogGameByAppIDForUpdate(ctx, details.AppID); err == nil {
		gameID = uuid.UUID(old.ID.Bytes)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	fullHash := fmt.Sprintf("%x", sha256.Sum256(snapshot))
	game, err := q.UpsertCatalogGame(ctx, dbgen.UpsertCatalogGameParams{ID: uuidPG(gameID), SteamAppID: details.AppID,
		Name: details.Name, Source: "steam_store", LastSuccessfulFetchAt: timestamptz(now), SourceSnapshot: snapshot, ContentHash: text(fullHash)})
	if err != nil {
		return uuid.Nil, err
	}
	if err := replaceSteamModes(ctx, q, gameID, details.Categories, fullHash, now); err != nil {
		return uuid.Nil, err
	}
	if err := q.UpsertGamePlatforms(ctx, dbgen.UpsertGamePlatformsParams{GameID: game.ID, Windows: details.Platforms.Windows,
		Mac: details.Platforms.Mac, Linux: details.Platforms.Linux, PlatformsLastSuccessfulFetchAt: timestamptz(now)}); err != nil {
		return uuid.Nil, err
	}
	if err := q.UpsertCatalogFetchState(ctx, dbgen.UpsertCatalogFetchStateParams{GameID: game.ID, Availability: string(steam.Available),
		LastAttemptAt: timestamptz(now), LastSuccessfulFetchAt: timestamptz(now)}); err != nil {
		return uuid.Nil, err
	}
	if err := observeReleaseWithQueries(ctx, q, game.ID, window, "steam_store", now); err != nil {
		return uuid.Nil, err
	}
	_ = semanticHash
	return gameID, nil
}

func rQueries(tx pgx.Tx) *dbgen.Queries { return dbgen.New(tx) }

func releaseWindowColumns(window ReleaseWindow) (string, pgtype.Date, pgtype.Int4, pgtype.Int4, pgtype.Int4) {
	date, year, month, quarter := pgtype.Date{}, pgtype.Int4{}, pgtype.Int4{}, pgtype.Int4{}
	if window.Date != nil {
		date = pgtype.Date{Time: window.Date.UTC(), Valid: true}
	}
	if window.Year > 0 {
		year = pgtype.Int4{Int32: int32(window.Year), Valid: true}
	}
	if window.Month > 0 {
		month = pgtype.Int4{Int32: int32(window.Month), Valid: true}
	}
	if window.Quarter > 0 {
		quarter = pgtype.Int4{Int32: int32(window.Quarter), Valid: true}
	}
	return string(window.Precision), date, year, month, quarter
}

func inactiveReason(decision EligibilityDecision) string {
	if decision.Active {
		return ""
	}
	switch decision.Reason {
	case "outside_window", "mixed_or_below", "unrated_grace_expired", "unrated_not_popular_new", "review_unknown", "not_game", "malformed":
		return decision.Reason
	default:
		return "malformed"
	}
}

func enqueueSearchDiscoveryFeatures(ctx context.Context, q *dbgen.Queries, planner SearchDiscoveryEnrichmentPlanner, gameID uuid.UUID, details steam.GameDetails, priority int) error {
	spec, err := planner.PlanCatalogGameFeatureJob(ctx, gameID, SemanticContentHash(details), priority)
	if err != nil {
		return err
	}
	if spec.Kind == "" {
		return nil
	}
	prefix := "catalog:enrichment:v1:" + gameID.String() + ":"
	if err := q.LockCatalogEnrichmentExecution(ctx, prefix); err != nil {
		return err
	}
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(spec.Kind+":"+spec.IdempotencyKey)))
	latest, err := q.GetLatestCatalogEnrichmentExecution(ctx, prefix)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	generation := int64(1)
	if err == nil {
		if strings.HasSuffix(latest.IdempotencyKey, ":"+identity) {
			// A matching nonterminal execution on either lane is the one in-flight
			// unit. Succeeded executions are the semantic cache. A dead execution
			// gets a new bounded queue generation only on a later canonical pass.
			if latest.Status != "dead" {
				return nil
			}
		}
		if latest.Generation == math.MaxInt64 {
			return errors.New("catalog enrichment execution generation exhausted")
		}
		generation = latest.Generation + 1
	}
	spec.IdempotencyKey = fmt.Sprintf("%s%d:%s", prefix, generation, identity)
	if binder, ok := planner.(interface {
		BindGameFeatureExecution(jobs.Spec, int64, string) (jobs.Spec, error)
	}); ok {
		spec, err = binder.BindGameFeatureExecution(spec, generation, spec.IdempotencyKey)
		if err != nil {
			return err
		}
	}
	_, err = jobs.EnqueueInTx(ctx, q, spec)
	return err
}
