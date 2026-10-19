package catalog

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/jobs"
	dbgen "next-game-crush/internal/store/sqlc"
)

const metadataReservationLease = 15 * time.Minute

var ErrNoMetadataWork = errors.New("no catalog metadata work is ready")

type MetadataIngress struct {
	Kind, Identity string
}

type MetadataWorkItem struct {
	RunID          uuid.UUID
	Priority       int
	Ingress        MetadataIngress
	Reservation    MetadataReservation
	BudgetGranted  bool
	NextEligibleAt time.Time
}

// EnqueueUserSyncCandidates routes user-derived candidates through the same
// AppDetails reservation and qualification protocol as scheduled discovery.
// A candidate not observed by Search is bootstrapped into the shared material
// revision domain first, so it retains the same fencing, TTL and throttle.
func (r *SearchRepository) EnqueueUserSyncCandidates(ctx context.Context, queue *jobs.Queue, cfg DiscoveryConfig, identity string, targets []RefreshTarget, now time.Time) (int, error) {
	if r == nil || r.store == nil || queue == nil || cfg.Validate() != nil || strings.TrimSpace(identity) == "" || now.IsZero() {
		return 0, errors.New("invalid user sync catalog ingress")
	}
	count := 0
	for _, target := range targets {
		// AppID is already part of the ingress key. Keep the remaining identity
		// stable across users and sync executions so shared catalog state neither
		// stores a user UUID nor accumulates one work item per interactive sync.
		ingress := MetadataIngress{Kind: "user_sync", Identity: "user_sync:v1"}
		rev, fingerprint, err := r.ensureUserSyncCandidate(ctx, ingress, target, now.UTC())
		if err != nil {
			return count, err
		}
		item, err := r.ReserveIngressMetadata(ctx, ingress, target.AppID,
			rev, fingerprint, cfg.AppDetailsHourlyBudget, now.UTC())
		if err != nil {
			return count, err
		}
		if !item.BudgetGranted || !item.Reservation.Granted {
			continue
		}
		payload := SearchQualificationPayload{Version: SearchQualificationPayloadVersion, AppID: target.AppID,
			CandidateRevision: item.Reservation.Revision, ReservationID: item.Reservation.ID,
			ReservationToken: item.Reservation.FencingToken, Priority: max(1, min(100, target.Priority))}
		spec, err := NewSearchQualificationSpec(payload, uuid.Nil)
		if err != nil {
			return count, err
		}
		if _, err := queue.Enqueue(ctx, spec); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func (r *SearchRepository) ensureUserSyncCandidate(ctx context.Context, in MetadataIngress, target RefreshTarget, now time.Time) (CandidateRevision, string, error) {
	if !validMetadataIngress(in) || target.AppID < 1 || target.AppID > math.MaxUint32 || target.Priority < 1 || target.Priority > 100 {
		return CandidateRevision{}, "", errors.New("invalid user sync candidate")
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return CandidateRevision{}, "", err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	if err := q.LockMetadataApp(ctx, target.AppID); err != nil {
		return CandidateRevision{}, "", err
	}
	var rev CandidateRevision
	var fingerprint string
	err = tx.QueryRow(ctx, `SELECT material_run_revision,material_observation_ordinal,material_fingerprint
		FROM catalog_metadata_heads WHERE steam_app_id=$1 FOR UPDATE`, target.AppID).
		Scan(&rev.RunRevision, &rev.ObservationOrdinal, &fingerprint)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return CandidateRevision{}, "", err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT material_run_revision,material_observation_ordinal,material_fingerprint
			FROM catalog_search_current WHERE steam_app_id=$1 FOR UPDATE`, target.AppID).
			Scan(&rev.RunRevision, &rev.ObservationOrdinal, &fingerprint)
		if errors.Is(err, pgx.ErrNoRows) {
			if err := tx.QueryRow(ctx, `SELECT nextval(pg_get_serial_sequence('catalog_search_runs','run_revision'))`).Scan(&rev.RunRevision); err != nil {
				return CandidateRevision{}, "", err
			}
			rev.ObservationOrdinal = 1
			fingerprint = "user_sync:v1"
			if _, err := tx.Exec(ctx, `INSERT INTO catalog_search_current
				(steam_app_id,run_revision,observation_ordinal,priority,fingerprint,last_seen_at,
				 material_run_revision,material_observation_ordinal,material_fingerprint)
				VALUES($1,$2,$3,$4,$5,$6,$2,$3,$5)`, target.AppID, rev.RunRevision,
				rev.ObservationOrdinal, target.Priority, fingerprint, now); err != nil {
				return CandidateRevision{}, "", err
			}
		} else if err != nil {
			return CandidateRevision{}, "", err
		}
		if _, err := q.UpsertMetadataCandidateHead(ctx, dbgen.UpsertMetadataCandidateHeadParams{
			SteamAppID: target.AppID, RunRevision: rev.RunRevision, ObservationOrdinal: rev.ObservationOrdinal,
			Fingerprint: fingerprint, NowAt: pgtype.Timestamptz{Time: now, Valid: true},
		}); err != nil {
			return CandidateRevision{}, "", err
		}
	}
	if err := q.RecordMetadataProvenance(ctx, dbgen.RecordMetadataProvenanceParams{SteamAppID: target.AppID,
		IngressKind: in.Kind, IngressIdentity: in.Identity, RunRevision: rev.RunRevision, ObservationOrdinal: rev.ObservationOrdinal,
		FirstSeenAt: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil {
		return CandidateRevision{}, "", err
	}
	if _, err := q.UpsertMetadataIngressWork(ctx, dbgen.UpsertMetadataIngressWorkParams{SteamAppID: target.AppID,
		IngressKind: in.Kind, IngressIdentity: in.Identity, RunRevision: rev.RunRevision, ObservationOrdinal: rev.ObservationOrdinal,
		Fingerprint: fingerprint, MaterialRunRevision: rev.RunRevision, MaterialObservationOrdinal: rev.ObservationOrdinal,
		MaterialFingerprint: fingerprint, Priority: int16(target.Priority), UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil {
		return CandidateRevision{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return CandidateRevision{}, "", err
	}
	return rev, fingerprint, nil
}

func (r *SearchRepository) EnqueueReadyIngressQualifications(ctx context.Context, queue *jobs.Queue, cfg DiscoveryConfig, now time.Time) (int, error) {
	if r == nil || queue == nil || cfg.Validate() != nil || now.IsZero() {
		return 0, errors.New("invalid metadata ingress producer")
	}
	count, err := r.enqueueReservedIngressQualifications(ctx, queue)
	if err != nil {
		return count, err
	}
	for {
		item, err := r.ClaimIngressMetadataWork(ctx, cfg.AppDetailsHourlyBudget, now.UTC())
		if errors.Is(err, ErrNoMetadataWork) {
			return count, nil
		}
		if err != nil {
			return count, err
		}
		if !item.BudgetGranted || !item.Reservation.Granted {
			continue
		}
		payload := SearchQualificationPayload{Version: SearchQualificationPayloadVersion, AppID: item.Reservation.AppID,
			CandidateRevision: item.Reservation.Revision, ReservationID: item.Reservation.ID,
			ReservationToken: item.Reservation.FencingToken, Priority: max(1, min(100, item.Priority))}
		spec, err := NewSearchQualificationSpec(payload, uuid.Nil)
		if err != nil {
			return count, err
		}
		if _, err := queue.Enqueue(ctx, spec); err != nil {
			return count, err
		}
		count++
	}
}

func (r *SearchRepository) enqueueReservedIngressQualifications(ctx context.Context, queue *jobs.Queue) (int, error) {
	rows, err := r.store.Pool.Query(ctx, `SELECT w.steam_app_id,w.priority,r.run_revision,r.observation_ordinal,
		r.id,r.fencing_token,r.retry_generation
		FROM catalog_metadata_ingress_work w
		JOIN catalog_metadata_heads h USING(steam_app_id)
		JOIN catalog_store_reservations r USING(steam_app_id)
		WHERE w.status='reserved' AND r.status='reserved'
		  AND (w.material_run_revision,w.material_observation_ordinal,w.material_fingerprint)=
		      (h.material_run_revision,h.material_observation_ordinal,h.material_fingerprint)
		  AND (r.run_revision,r.observation_ordinal,r.fingerprint)=
		      (h.material_run_revision,h.material_observation_ordinal,h.material_fingerprint)
		ORDER BY w.priority DESC,w.created_at,w.steam_app_id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	payloads := make([]SearchQualificationPayload, 0)
	for rows.Next() {
		var payload SearchQualificationPayload
		var priority int16
		var reservationID, token pgtype.UUID
		var generation int32
		if err := rows.Scan(&payload.AppID, &priority, &payload.CandidateRevision.RunRevision,
			&payload.CandidateRevision.ObservationOrdinal, &reservationID, &token, &generation); err != nil {
			return 0, err
		}
		payload.Version, payload.Priority, payload.RetryGeneration = SearchQualificationPayloadVersion, int(priority), int(generation)
		payload.ReservationID, payload.ReservationToken = uuid.UUID(reservationID.Bytes), uuid.UUID(token.Bytes)
		payloads = append(payloads, payload)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	count := 0
	for _, payload := range payloads {
		spec, err := NewSearchQualificationSpec(payload, uuid.Nil)
		if err != nil {
			return count, err
		}
		if _, err := queue.Enqueue(ctx, spec); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// MetadataRefreshTTL returns the default appdetails freshness interval. Search
// review snapshots remain independently refreshable and are intentionally not
// inputs to this policy.
func MetadataRefreshTTL(window ReleaseWindow, tombstone string, retryGeneration int, now time.Time) time.Duration {
	switch tombstone {
	case "mixed_or_below", "unrated", "unrated_grace_expired", "unrated_not_popular_new", "outside_window", "not_game":
		return 30 * 24 * time.Hour
	case "unavailable", "malformed", "review_unknown":
		if retryGeneration >= 2 {
			return 7 * 24 * time.Hour
		}
		return 24 * time.Hour
	}
	if window.Precision == PrecisionDay && window.Date != nil {
		delta := utcCalendarDay(*window.Date).Sub(utcCalendarDay(now))
		if delta >= -3*24*time.Hour && delta <= 3*24*time.Hour {
			return 24 * time.Hour
		}
		return 72 * time.Hour
	}
	switch window.Precision {
	case PrecisionMonth, PrecisionQuarter:
		return 72 * time.Hour
	default:
		return 7 * 24 * time.Hour
	}
}

// ReserveMetadata fences appdetails work by AppID and monotonic candidate
// revision. The search projection remains the authority for freshness, while
// the reservation fingerprint makes material search changes wake immediately.
func (r *SearchRepository) ReserveMetadata(ctx context.Context, appID int64, rev CandidateRevision, fingerprint string, now time.Time) (MetadataReservation, error) {
	if r == nil || r.store == nil || appID < 1 || rev.RunRevision < 1 || rev.ObservationOrdinal < 1 || len(fingerprint) > 128 || strings.TrimSpace(fingerprint) != fingerprint || now.IsZero() {
		return MetadataReservation{}, errors.New("invalid metadata reservation")
	}
	now = now.UTC()
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return MetadataReservation{}, err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	current, err := lockMetadataCandidate(ctx, q, appID)
	if err != nil {
		return MetadataReservation{}, err
	}
	head, err := upsertMetadataCandidateHead(ctx, q, appID, rev, fingerprint, now)
	if err != nil {
		return MetadataReservation{}, err
	}
	if _, err := q.SupersedeStaleMetadataWorkForApp(ctx, dbgen.SupersedeStaleMetadataWorkForAppParams{SteamAppID: appID, RunRevision: head.MaterialRunRevision, ObservationOrdinal: head.MaterialObservationOrdinal}); err != nil {
		return MetadataReservation{}, err
	}
	if _, err := q.SupersedeStaleMetadataIngressWork(ctx, dbgen.SupersedeStaleMetadataIngressWorkParams{SteamAppID: appID, RunRevision: head.MaterialRunRevision, ObservationOrdinal: head.MaterialObservationOrdinal, Fingerprint: head.MaterialFingerprint, NowAt: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil {
		return MetadataReservation{}, err
	}
	if head.LatestRunRevision != rev.RunRevision || head.LatestObservationOrdinal != rev.ObservationOrdinal || head.LatestFingerprint != fingerprint {
		if err := tx.Commit(ctx); err != nil {
			return MetadataReservation{}, err
		}
		return MetadataReservation{AppID: appID, Revision: rev, Reason: "stale"}, nil
	}
	rev = CandidateRevision{RunRevision: head.MaterialRunRevision, ObservationOrdinal: head.MaterialObservationOrdinal}
	fingerprint = head.MaterialFingerprint
	reservation, err := reserveMetadataWithQueries(ctx, q, appID, rev, fingerprint, now)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return MetadataReservation{}, err
		}
		return reservation, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return MetadataReservation{}, err
	}
	result := MetadataReservation{AppID: appID, Revision: rev, Reason: "unchanged"}
	existing, getErr := q.GetStoreReservation(ctx, appID)
	if getErr == nil {
		result.ID = uuid.UUID(existing.ID.Bytes)
		result.FencingToken = uuid.UUID(existing.FencingToken.Bytes)
		if existing.Status == "reserved" && existing.LeaseUntil.Time.After(now) {
			result.Reason, result.NextEligibleAt = "leased", existing.LeaseUntil.Time.UTC()
		}
	} else if !errors.Is(getErr, pgx.ErrNoRows) {
		return MetadataReservation{}, getErr
	}
	if current != nil {
		if (CandidateRevision{RunRevision: head.MaterialRunRevision, ObservationOrdinal: head.MaterialObservationOrdinal}).newerThan(rev) {
			result.Reason = "stale"
		} else if current.NextRefreshAt.Valid && current.NextRefreshAt.Time.After(now) && result.Reason != "leased" {
			result.NextEligibleAt = current.NextRefreshAt.Time.UTC()
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return MetadataReservation{}, err
	}
	return result, nil
}

func reserveMetadataWithQueries(ctx context.Context, q *dbgen.Queries, appID int64, rev CandidateRevision, fingerprint string, now time.Time) (MetadataReservation, error) {
	id, token := uuid.New(), uuid.New()
	row, err := q.AcquireStoreReservation(ctx, dbgen.AcquireStoreReservationParams{SteamAppID: appID, ID: catalogUUID(id), RunRevision: rev.RunRevision, ObservationOrdinal: rev.ObservationOrdinal, Fingerprint: fingerprint, FencingToken: catalogUUID(token), LeaseUntil: pgtype.Timestamptz{Time: now.Add(metadataReservationLease), Valid: true}, NowAt: pgtype.Timestamptz{Time: now, Valid: true}})
	if err != nil {
		return MetadataReservation{}, err
	}
	return MetadataReservation{ID: uuid.UUID(row.ID.Bytes), AppID: appID, Revision: rev, Granted: true, Reason: "reserved", NextEligibleAt: row.LeaseUntil.Time.UTC(), FencingToken: uuid.UUID(row.FencingToken.Bytes)}, nil
}

func validMetadataIngress(in MetadataIngress) bool {
	switch in.Kind {
	case "scheduled", "manual", "initial", "reconciliation", "user_sync":
	default:
		return false
	}
	return strings.TrimSpace(in.Identity) != "" && len(in.Identity) <= 256
}

func lockMetadataCandidate(ctx context.Context, q *dbgen.Queries, appID int64) (*dbgen.CatalogSearchCurrent, error) {
	if err := q.LockMetadataApp(ctx, appID); err != nil {
		return nil, err
	}
	if _, err := q.LockMetadataCandidateHead(ctx, appID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	current, err := q.LockCurrentMetadataCandidate(ctx, appID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &current, nil
}

func upsertMetadataCandidateHead(ctx context.Context, q *dbgen.Queries, appID int64, rev CandidateRevision, fingerprint string, now time.Time) (dbgen.CatalogMetadataHead, error) {
	head, err := q.UpsertMetadataCandidateHead(ctx, dbgen.UpsertMetadataCandidateHeadParams{
		SteamAppID: appID, RunRevision: rev.RunRevision, ObservationOrdinal: rev.ObservationOrdinal,
		Fingerprint: fingerprint, NowAt: pgtype.Timestamptz{Time: now.UTC(), Valid: true},
	})
	if err != nil {
		return dbgen.CatalogMetadataHead{}, err
	}
	if _, err := q.SyncCurrentMaterialCandidate(ctx, dbgen.SyncCurrentMaterialCandidateParams{RunRevision: pgtype.Int8{Int64: head.MaterialRunRevision, Valid: true}, ObservationOrdinal: pgtype.Int8{Int64: head.MaterialObservationOrdinal, Valid: true}, Fingerprint: pgtype.Text{String: head.MaterialFingerprint, Valid: true}, SteamAppID: appID}); err != nil {
		return dbgen.CatalogMetadataHead{}, err
	}
	return head, nil
}

func processMetadataCandidate(ctx context.Context, q *dbgen.Queries, appID int64, rev CandidateRevision, fingerprint string, budgetLimit int, now time.Time) (MetadataReservation, bool, time.Time, error) {
	needed, err := q.MetadataReservationNeeded(ctx, dbgen.MetadataReservationNeededParams{RunRevision: rev.RunRevision, ObservationOrdinal: rev.ObservationOrdinal, SteamAppID: appID, CandidateFingerprint: fingerprint, NowAt: pgtype.Timestamptz{Time: now, Valid: true}})
	if err != nil {
		return MetadataReservation{}, false, time.Time{}, err
	}
	if !needed {
		return MetadataReservation{AppID: appID, Revision: rev, Reason: "deduplicated"}, false, time.Time{}, nil
	}
	identity := fmt.Sprintf("appdetails:%d:%d:%d:%s", appID, rev.RunRevision, rev.ObservationOrdinal, fingerprint)
	reuseBudget := false
	if previous, previousErr := q.GetStoreReservation(ctx, appID); previousErr == nil {
		reuseBudget = previous.Status == "reserved" && previous.RunRevision == rev.RunRevision && previous.ObservationOrdinal == rev.ObservationOrdinal && previous.Fingerprint == fingerprint && previous.BudgetHour.Valid && previous.BudgetIdentityHash.Valid
	} else if !errors.Is(previousErr, pgx.ErrNoRows) {
		return MetadataReservation{}, false, time.Time{}, previousErr
	}
	next := time.Time{}
	if !reuseBudget {
		granted, deferredUntil, err := reserveBudgetWithQueries(ctx, q, "appdetails", identity, budgetLimit, now)
		if err != nil {
			return MetadataReservation{}, false, time.Time{}, err
		}
		if !granted {
			return MetadataReservation{AppID: appID, Revision: rev, Reason: "budget_deferred", NextEligibleAt: deferredUntil}, false, deferredUntil, nil
		}
		next = deferredUntil
	}
	reservation, err := reserveMetadataWithQueries(ctx, q, appID, rev, fingerprint, now)
	if err != nil {
		return MetadataReservation{}, false, time.Time{}, err
	}
	if !reuseBudget {
		if _, err := q.AttachStoreReservationBudget(ctx, dbgen.AttachStoreReservationBudgetParams{BudgetHour: pgtype.Timestamptz{Time: now.Truncate(time.Hour), Valid: true}, IdentityHash: pgtype.Text{String: catalogBudgetHash(identity), Valid: true}, NowAt: pgtype.Timestamptz{Time: now, Valid: true}, SteamAppID: appID, ID: catalogUUID(reservation.ID), FencingToken: catalogUUID(reservation.FencingToken)}); err != nil {
			return MetadataReservation{}, false, time.Time{}, err
		}
	}
	return reservation, true, next, nil
}

// ReserveIngressMetadata is the production seam for non-search contenders,
// notably user sync. Provenance, reservation ownership and the unique Store
// budget charge commit atomically.
func (r *SearchRepository) ReserveIngressMetadata(ctx context.Context, in MetadataIngress, appID int64, rev CandidateRevision, fingerprint string, budgetLimit int, now time.Time) (MetadataWorkItem, error) {
	if r == nil || r.store == nil || !validMetadataIngress(in) || appID < 1 || rev.RunRevision < 1 || rev.ObservationOrdinal < 1 || strings.TrimSpace(fingerprint) != fingerprint || len(fingerprint) > 128 || budgetLimit < 1 || budgetLimit > 100000 || now.IsZero() {
		return MetadataWorkItem{}, errors.New("invalid metadata ingress")
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return MetadataWorkItem{}, err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	if _, err := lockMetadataCandidate(ctx, q, appID); err != nil {
		return MetadataWorkItem{}, err
	}
	if err := q.RecordMetadataProvenance(ctx, dbgen.RecordMetadataProvenanceParams{SteamAppID: appID, IngressKind: in.Kind, IngressIdentity: in.Identity, RunRevision: rev.RunRevision, ObservationOrdinal: rev.ObservationOrdinal, FirstSeenAt: pgtype.Timestamptz{Time: now.UTC(), Valid: true}}); err != nil {
		return MetadataWorkItem{}, err
	}
	head, err := upsertMetadataCandidateHead(ctx, q, appID, rev, fingerprint, now)
	if err != nil {
		return MetadataWorkItem{}, err
	}
	if head.LatestRunRevision != rev.RunRevision || head.LatestObservationOrdinal != rev.ObservationOrdinal || head.LatestFingerprint != fingerprint {
		if err := tx.Commit(ctx); err != nil {
			return MetadataWorkItem{}, err
		}
		return MetadataWorkItem{Ingress: in, Reservation: MetadataReservation{AppID: appID, Revision: rev, Reason: "stale"}}, nil
	}
	materialRev := CandidateRevision{RunRevision: head.MaterialRunRevision, ObservationOrdinal: head.MaterialObservationOrdinal}
	if _, err := q.SupersedeStaleMetadataWorkForApp(ctx, dbgen.SupersedeStaleMetadataWorkForAppParams{SteamAppID: appID, RunRevision: materialRev.RunRevision, ObservationOrdinal: materialRev.ObservationOrdinal}); err != nil {
		return MetadataWorkItem{}, err
	}
	if _, err := q.SupersedeStaleMetadataIngressWork(ctx, dbgen.SupersedeStaleMetadataIngressWorkParams{SteamAppID: appID, RunRevision: materialRev.RunRevision, ObservationOrdinal: materialRev.ObservationOrdinal, Fingerprint: head.MaterialFingerprint, NowAt: pgtype.Timestamptz{Time: now.UTC(), Valid: true}}); err != nil {
		return MetadataWorkItem{}, err
	}
	work, err := q.UpsertMetadataIngressWork(ctx, dbgen.UpsertMetadataIngressWorkParams{SteamAppID: appID, IngressKind: in.Kind, IngressIdentity: in.Identity, RunRevision: rev.RunRevision, ObservationOrdinal: rev.ObservationOrdinal, Fingerprint: fingerprint, MaterialRunRevision: materialRev.RunRevision, MaterialObservationOrdinal: materialRev.ObservationOrdinal, MaterialFingerprint: head.MaterialFingerprint, Priority: 90, UpdatedAt: pgtype.Timestamptz{Time: now.UTC(), Valid: true}})
	if err != nil {
		return MetadataWorkItem{}, err
	}
	work, err = q.ClaimMetadataIngressWork(ctx, dbgen.ClaimMetadataIngressWorkParams{ID: work.ID, NowAt: pgtype.Timestamptz{Time: now.UTC(), Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return MetadataWorkItem{}, err
		}
		return MetadataWorkItem{Ingress: in, Reservation: MetadataReservation{AppID: appID, Revision: rev, Reason: "deduplicated"}}, nil
	}
	if err != nil {
		return MetadataWorkItem{}, err
	}
	rev = CandidateRevision{RunRevision: work.MaterialRunRevision, ObservationOrdinal: work.MaterialObservationOrdinal}
	fingerprint = work.MaterialFingerprint
	reservation, granted, next, err := processMetadataCandidate(ctx, q, appID, rev, fingerprint, budgetLimit, now.UTC())
	if err != nil {
		return MetadataWorkItem{}, err
	}
	state := "superseded"
	nextValue := pgtype.Timestamptz{}
	if granted {
		state = "reserved"
		if _, err := q.SupersedeStaleMetadataWorkForApp(ctx, dbgen.SupersedeStaleMetadataWorkForAppParams{SteamAppID: appID, RunRevision: rev.RunRevision, ObservationOrdinal: rev.ObservationOrdinal}); err != nil {
			return MetadataWorkItem{}, err
		}
		if _, err := q.SupersedeStaleMetadataIngressWork(ctx, dbgen.SupersedeStaleMetadataIngressWorkParams{SteamAppID: appID, RunRevision: rev.RunRevision, ObservationOrdinal: rev.ObservationOrdinal, Fingerprint: fingerprint, NowAt: pgtype.Timestamptz{Time: now.UTC(), Valid: true}}); err != nil {
			return MetadataWorkItem{}, err
		}
	} else if !next.IsZero() {
		state = "deferred"
		nextValue = pgtype.Timestamptz{Time: next, Valid: true}
	}
	if _, err := q.SetMetadataIngressWorkState(ctx, dbgen.SetMetadataIngressWorkStateParams{Status: state, NextEligibleAt: nextValue, NowAt: pgtype.Timestamptz{Time: now.UTC(), Valid: true}, ID: work.ID}); err != nil {
		return MetadataWorkItem{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MetadataWorkItem{}, err
	}
	return MetadataWorkItem{Ingress: in, Reservation: reservation, BudgetGranted: granted, NextEligibleAt: next}, nil
}

func (r *SearchRepository) ClaimIngressMetadataWork(ctx context.Context, budgetLimit int, now time.Time) (MetadataWorkItem, error) {
	if r == nil || r.store == nil || budgetLimit < 1 || now.IsZero() {
		return MetadataWorkItem{}, errors.New("invalid ingress work claim")
	}
	now = now.UTC()
	for {
		item, retry, err := r.claimIngressMetadataWorkOnce(ctx, budgetLimit, now)
		if !retry {
			return item, err
		}
		if err := ctx.Err(); err != nil {
			return MetadataWorkItem{}, err
		}
	}
}

func (r *SearchRepository) claimIngressMetadataWorkOnce(ctx context.Context, budgetLimit int, now time.Time) (MetadataWorkItem, bool, error) {
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return MetadataWorkItem{}, false, err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	peek, err := q.PeekNextMetadataIngressWork(ctx, pgtype.Timestamptz{Time: now, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return MetadataWorkItem{}, false, ErrNoMetadataWork
	}
	if err != nil {
		return MetadataWorkItem{}, false, err
	}
	if _, err := lockMetadataCandidate(ctx, q, peek.SteamAppID); err != nil {
		return MetadataWorkItem{}, false, err
	}
	work, err := q.ClaimMetadataIngressWork(ctx, dbgen.ClaimMetadataIngressWorkParams{ID: peek.ID, NowAt: pgtype.Timestamptz{Time: now, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return MetadataWorkItem{}, true, nil
	}
	if err != nil {
		return MetadataWorkItem{}, false, err
	}
	in := MetadataIngress{Kind: work.IngressKind, Identity: work.IngressIdentity}
	rev := CandidateRevision{work.MaterialRunRevision, work.MaterialObservationOrdinal}
	reservation, granted, next, err := processMetadataCandidate(ctx, q, work.SteamAppID, rev, work.MaterialFingerprint, budgetLimit, now)
	if err != nil {
		return MetadataWorkItem{}, false, err
	}
	state := "superseded"
	nextValue := pgtype.Timestamptz{}
	if granted {
		state = "reserved"
		if _, err := q.SupersedeStaleMetadataWorkForApp(ctx, dbgen.SupersedeStaleMetadataWorkForAppParams{SteamAppID: work.SteamAppID, RunRevision: rev.RunRevision, ObservationOrdinal: rev.ObservationOrdinal}); err != nil {
			return MetadataWorkItem{}, false, err
		}
		if _, err := q.SupersedeStaleMetadataIngressWork(ctx, dbgen.SupersedeStaleMetadataIngressWorkParams{SteamAppID: work.SteamAppID, RunRevision: rev.RunRevision, ObservationOrdinal: rev.ObservationOrdinal, Fingerprint: work.MaterialFingerprint, NowAt: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil {
			return MetadataWorkItem{}, false, err
		}
	} else if !next.IsZero() {
		state = "deferred"
		nextValue = pgtype.Timestamptz{Time: next, Valid: true}
	}
	if _, err := q.SetMetadataIngressWorkState(ctx, dbgen.SetMetadataIngressWorkStateParams{Status: state, NextEligibleAt: nextValue, NowAt: pgtype.Timestamptz{Time: now, Valid: true}, ID: work.ID}); err != nil {
		return MetadataWorkItem{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MetadataWorkItem{}, false, err
	}
	return MetadataWorkItem{Ingress: in, Priority: int(work.Priority), Reservation: reservation, BudgetGranted: granted, NextEligibleAt: next}, false, nil
}

// ClaimMetadataWork atomically orders and transitions the durable search
// worklist. Retry-due items precede ordinary search, which precedes weekly
// reconciliation. It intentionally does not enqueue Task 9's future handler.
func (r *SearchRepository) ClaimMetadataWork(ctx context.Context, budgetLimit int, now time.Time) (MetadataWorkItem, error) {
	if r == nil || r.store == nil || budgetLimit < 1 || now.IsZero() {
		return MetadataWorkItem{}, errors.New("invalid metadata work claim")
	}
	now = now.UTC()
	for {
		item, retry, err := r.claimMetadataWorkOnce(ctx, budgetLimit, now)
		if !retry {
			return item, err
		}
		if err := ctx.Err(); err != nil {
			return MetadataWorkItem{}, err
		}
	}
}

func (r *SearchRepository) claimMetadataWorkOnce(ctx context.Context, budgetLimit int, now time.Time) (MetadataWorkItem, bool, error) {
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return MetadataWorkItem{}, false, err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	peek, err := q.PeekNextMetadataWork(ctx, pgtype.Timestamptz{Time: now, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return MetadataWorkItem{}, false, commitErr
		}
		return MetadataWorkItem{}, false, ErrNoMetadataWork
	}
	if err != nil {
		return MetadataWorkItem{}, false, err
	}
	if _, err := lockMetadataCandidate(ctx, q, peek.SteamAppID); err != nil {
		return MetadataWorkItem{}, false, err
	}
	row, err := q.ClaimMetadataWork(ctx, dbgen.ClaimMetadataWorkParams{RunID: peek.RunID, SteamAppID: peek.SteamAppID, NowAt: pgtype.Timestamptz{Time: now, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return MetadataWorkItem{}, true, nil
	}
	if err != nil {
		return MetadataWorkItem{}, false, err
	}
	in := MetadataIngress{Kind: row.Kind, Identity: uuid.UUID(row.RunID.Bytes).String()}
	rev := CandidateRevision{RunRevision: row.RunRevision, ObservationOrdinal: row.ObservationOrdinal}
	if err := q.RecordMetadataProvenance(ctx, dbgen.RecordMetadataProvenanceParams{SteamAppID: row.SteamAppID, IngressKind: in.Kind, IngressIdentity: in.Identity, RunRevision: rev.RunRevision, ObservationOrdinal: rev.ObservationOrdinal, FirstSeenAt: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil {
		return MetadataWorkItem{}, false, err
	}
	reservation, granted, next, err := processMetadataCandidate(ctx, q, row.SteamAppID, rev, row.Fingerprint, budgetLimit, now)
	if err != nil {
		return MetadataWorkItem{}, false, err
	}
	if !granted && next.IsZero() {
		if _, err := q.MarkMetadataWorkSuperseded(ctx, dbgen.MarkMetadataWorkSupersededParams{RunID: row.RunID, SteamAppID: row.SteamAppID}); err != nil {
			return MetadataWorkItem{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return MetadataWorkItem{}, false, err
		}
		return MetadataWorkItem{RunID: uuid.UUID(row.RunID.Bytes), Priority: int(row.Priority), Ingress: in, Reservation: MetadataReservation{AppID: row.SteamAppID, Revision: rev, Reason: "deduplicated"}}, false, nil
	}
	if granted {
		if _, err := q.MarkMetadataWorkReserved(ctx, dbgen.MarkMetadataWorkReservedParams{RunID: row.RunID, SteamAppID: row.SteamAppID}); err != nil {
			return MetadataWorkItem{}, false, err
		}
		if _, err := q.SupersedeStaleMetadataIngressWork(ctx, dbgen.SupersedeStaleMetadataIngressWorkParams{SteamAppID: row.SteamAppID, RunRevision: rev.RunRevision, ObservationOrdinal: rev.ObservationOrdinal, Fingerprint: row.Fingerprint, NowAt: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil {
			return MetadataWorkItem{}, false, err
		}
	} else {
		if _, err := q.DeferMetadataWork(ctx, dbgen.DeferMetadataWorkParams{NextRetryAt: pgtype.Timestamptz{Time: next, Valid: true}, RunID: row.RunID, SteamAppID: row.SteamAppID}); err != nil {
			return MetadataWorkItem{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return MetadataWorkItem{}, false, err
	}
	return MetadataWorkItem{RunID: uuid.UUID(row.RunID.Bytes), Priority: int(row.Priority), Ingress: in, Reservation: reservation, BudgetGranted: granted, NextEligibleAt: next}, false, nil
}

func (revision CandidateRevision) newerThan(other CandidateRevision) bool {
	return revision.RunRevision > other.RunRevision || revision.RunRevision == other.RunRevision && revision.ObservationOrdinal > other.ObservationOrdinal
}

// ReleaseMetadata gives up an owned lease without changing the last successful
// candidate state. A stale worker cannot release a newer reservation.
func (r *SearchRepository) ReleaseMetadata(ctx context.Context, reservation MetadataReservation, now time.Time) error {
	if r == nil || r.store == nil || !reservation.Granted || reservation.AppID < 1 || reservation.ID == uuid.Nil || reservation.FencingToken == uuid.Nil || now.IsZero() {
		return errors.New("invalid metadata release")
	}
	tx, err := r.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := r.store.Queries.WithTx(tx)
	if _, err := lockMetadataCandidate(ctx, q, reservation.AppID); err != nil {
		return err
	}
	owned, err := q.GetStoreReservation(ctx, reservation.AppID)
	if err != nil {
		return err
	}
	if uuid.UUID(owned.ID.Bytes) != reservation.ID || uuid.UUID(owned.FencingToken.Bytes) != reservation.FencingToken {
		return errors.New("metadata reservation lease lost")
	}
	rows, err := q.ReleaseStoreReservation(ctx, dbgen.ReleaseStoreReservationParams{NowAt: pgtype.Timestamptz{Time: now.UTC(), Valid: true}, SteamAppID: reservation.AppID, ID: catalogUUID(reservation.ID), FencingToken: catalogUUID(reservation.FencingToken)})
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("metadata reservation lease lost")
	}
	head, err := q.GetCurrentMaterialCandidate(ctx, reservation.AppID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) || head.MaterialRunRevision != reservation.Revision.RunRevision || head.MaterialObservationOrdinal != reservation.Revision.ObservationOrdinal || head.MaterialFingerprint != owned.Fingerprint {
		if !errors.Is(err, pgx.ErrNoRows) {
			if _, err := q.SupersedeStaleMetadataWorkForApp(ctx, dbgen.SupersedeStaleMetadataWorkForAppParams{SteamAppID: reservation.AppID, RunRevision: head.MaterialRunRevision, ObservationOrdinal: head.MaterialObservationOrdinal}); err != nil {
				return err
			}
			if _, err := q.SupersedeStaleMetadataIngressWork(ctx, dbgen.SupersedeStaleMetadataIngressWorkParams{SteamAppID: reservation.AppID, RunRevision: head.MaterialRunRevision, ObservationOrdinal: head.MaterialObservationOrdinal, Fingerprint: head.MaterialFingerprint, NowAt: pgtype.Timestamptz{Time: now.UTC(), Valid: true}}); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}
	params := dbgen.RequeueMetadataWorkParams{SteamAppID: reservation.AppID, RunRevision: reservation.Revision.RunRevision, ObservationOrdinal: reservation.Revision.ObservationOrdinal}
	if _, err := q.RequeueMetadataWork(ctx, params); err != nil {
		return err
	}
	if _, err := q.RequeueMetadataIngressWork(ctx, dbgen.RequeueMetadataIngressWorkParams{SteamAppID: reservation.AppID, RunRevision: reservation.Revision.RunRevision, ObservationOrdinal: reservation.Revision.ObservationOrdinal, Fingerprint: head.MaterialFingerprint}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
