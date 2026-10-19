package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
	dbgen "next-game-crush/internal/store/sqlc"
)

const reconciliationWatermarkOverlap = 5 * time.Minute

type reconciliationManifest struct {
	Version          int                   `json:"version"`
	Apps             []steam.DiscoveredApp `json:"apps"`
	NextAppID        uint32                `json:"next_app_id"`
	HasMore          bool                  `json:"has_more"`
	ResumeAfterAppID uint32                `json:"resume_after_app_id,omitempty"`
	PartialReason    string                `json:"partial_reason,omitempty"`
	Continuation     bool                  `json:"continuation,omitempty"`
	ResumePage       int                   `json:"resume_page,omitempty"`
	NewAppsUsed      int                   `json:"new_apps_used,omitempty"`
}

func reconciliationPageKey(p ReconciliationPagePayload) string {
	return fmt.Sprintf("catalog.reconciliation.page:v1:%s:after:%d:retry:%d", p.RunID, p.AfterAppID, p.PageGeneration)
}

func reconciliationPageSpec(p ReconciliationPagePayload) jobs.Spec {
	raw, _ := json.Marshal(p)
	return jobs.Spec{Lane: jobs.LaneCatalog, Kind: ReconciliationPageJobKind, Stage: "catalog_reconciliation_page",
		IdempotencyKey: reconciliationPageKey(p), Payload: raw, Priority: 10, MaxAttempts: 4}
}

func validReconciliationPagePayload(p ReconciliationPagePayload) bool {
	return p.Version == 1 && p.RunID != uuid.Nil && p.RunRevision > 0 && p.PageGeneration >= 0 && p.Page >= 0 &&
		p.NewAppCount >= 0 && !p.ScanStartedAt.IsZero() && p.ScanStartedAt.Equal(p.ScanStartedAt.UTC()) &&
		(p.Since.IsZero() || p.Since.Equal(p.Since.UTC()))
}

func (s *SearchScanService) handleReconciliationPage(ctx context.Context, lease jobs.Lease) error {
	var p ReconciliationPagePayload
	if strictDiscoveryPayload(lease.Payload, &p) != nil || !validReconciliationPagePayload(p) ||
		lease.Kind != ReconciliationPageJobKind || lease.Lane != jobs.LaneCatalog || lease.IdempotencyKey != reconciliationPageKey(p) {
		return errors.New("invalid reconciliation page payload")
	}
	var stored []byte
	var runStatus string
	var runGeneration int
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT j.result,r.status,r.retry_generation FROM sync_jobs j
		JOIN catalog_search_runs r ON r.id=$2 WHERE j.id=$1 AND r.run_revision=$3`, lease.ID, p.RunID, p.RunRevision).
		Scan(&stored, &runStatus, &runGeneration); err != nil {
		return err
	}
	if runGeneration != p.PageGeneration {
		return jobs.ErrLeaseLost
	}
	manifest := reconciliationManifest{}
	if len(stored) > 0 && string(stored) != "{}" {
		if strictDiscoveryPayload(stored, &manifest) != nil || manifest.Version != 1 {
			return errors.New("invalid reconciliation manifest")
		}
	}
	if runStatus == "complete" || (runStatus == "partial" && manifest.Continuation) {
		return s.enqueueReconciliationQualifications(ctx)
	}
	if runStatus != "running" {
		return jobs.ErrLeaseLost
	}
	if manifest.Version == 0 {
		source, ok := s.source.(interface {
			ListChangedGames(context.Context, time.Time, uint32, int) (steam.DiscoveryPage, error)
		})
		if !ok {
			return errors.New("reconciliation source is required")
		}
		page, err := source.ListChangedGames(ctx, p.Since, p.AfterAppID, s.cfg.ReconciliationPageSize)
		if err != nil {
			return s.reconciliationFailure(ctx, lease, p, err)
		}
		if !validReconciliationPage(page, p, s.cfg.ReconciliationPageSize) {
			return s.reconciliationFailure(ctx, lease, p, steam.ErrInvalidResponse)
		}
		used, countErr := s.reconciliationNewAppsUsed(ctx, p)
		if countErr != nil {
			return countErr
		}
		manifest = reconciliationManifest{Version: 1, Apps: page.Apps, NextAppID: page.NextAppID, HasMore: page.HasMore,
			ResumeAfterAppID: p.AfterAppID, Continuation: true, ResumePage: p.Page, NewAppsUsed: used}
		if err = s.recordReconciliationManifest(ctx, lease, p, manifest); err != nil {
			return err
		}
	}
	lastApplied := p.AfterAppID
	for _, app := range manifest.Apps {
		_, capped, err := s.persistReconciliationCandidate(ctx, p, app)
		if err != nil {
			return err
		}
		if capped {
			manifest.ResumeAfterAppID = lastApplied
			manifest.PartialReason = "new_app_limit"
			manifest.Continuation = true
			manifest.ResumePage = p.Page
			manifest.NewAppsUsed, err = s.reconciliationNewAppsUsed(ctx, p)
			if err != nil {
				return err
			}
			if err := s.recordReconciliationManifest(ctx, lease, p, manifest); err != nil {
				return err
			}
			return s.finishReconciliationAndQualify(ctx, p, true)
		}
		lastApplied = uint32(app.AppID)
		manifest.ResumeAfterAppID = lastApplied
		manifest.ResumePage = p.Page
		manifest.NewAppsUsed, err = s.reconciliationNewAppsUsed(ctx, p)
		if err != nil {
			return err
		}
		if err = s.recordReconciliationManifest(ctx, lease, p, manifest); err != nil {
			return err
		}
	}
	if manifest.HasMore && p.Page+1 >= s.cfg.SearchMaxPagesPerFeed {
		manifest.ResumeAfterAppID = manifest.NextAppID
		manifest.PartialReason = "page_limit"
		manifest.Continuation = true
		manifest.ResumePage = 0
		manifest.NewAppsUsed = 0
		if err := s.recordReconciliationManifest(ctx, lease, p, manifest); err != nil {
			return err
		}
		return s.finishReconciliationAndQualify(ctx, p, true)
	}
	if manifest.HasMore {
		next := p
		next.AfterAppID = manifest.NextAppID
		next.Page++
		manifest.ResumeAfterAppID = next.AfterAppID
		manifest.ResumePage = next.Page
		var err error
		manifest.NewAppsUsed, err = s.reconciliationNewAppsUsed(ctx, p)
		if err != nil {
			return err
		}
		next.NewAppCount = manifest.NewAppsUsed
		if err = s.recordReconciliationManifest(ctx, lease, p, manifest); err != nil {
			return err
		}
		return s.enqueueReconciliationSuccessor(ctx, p, next)
	}
	return s.finishReconciliationAndQualify(ctx, p, false)
}

func (s *SearchScanService) recordReconciliationManifest(ctx context.Context, lease jobs.Lease, p ReconciliationPagePayload, manifest reconciliationManifest) error {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode reconciliation manifest: %w", err)
	}
	tx, err := s.repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockReconciliationRun(ctx, tx, p, "running"); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE sync_jobs SET result=$1,updated_at=now()
		WHERE id=$2 AND status='running' AND worker_id=$3 AND fencing_token=$4 AND lease_until>now()`,
		encoded, lease.ID, lease.WorkerID, lease.FencingToken)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return jobs.ErrLeaseLost
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func validReconciliationPage(page steam.DiscoveryPage, p ReconciliationPagePayload, limit int) bool {
	if len(page.Apps) > limit || page.NextAppID < p.AfterAppID || (page.HasMore && (len(page.Apps) == 0 || page.NextAppID <= p.AfterAppID)) {
		return false
	}
	previous := int64(p.AfterAppID)
	for _, app := range page.Apps {
		if app.AppID <= previous || app.AppID > math.MaxUint32 || app.LastModified.IsZero() {
			return false
		}
		previous = app.AppID
	}
	return !page.HasMore || uint32(previous) <= page.NextAppID
}

func (s *SearchScanService) enqueueReconciliationSuccessor(ctx context.Context, current, next ReconciliationPagePayload) error {
	tx, err := s.repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockReconciliationRun(ctx, tx, current, "running"); err != nil {
		return err
	}
	if _, err := jobs.EnqueueInTx(ctx, s.repo.store.Queries.WithTx(tx), reconciliationPageSpec(next)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if s.afterReconciliationSuccessorHook != nil {
		return s.afterReconciliationSuccessorHook()
	}
	return nil
}

func (s *SearchScanService) finishReconciliationPage(ctx context.Context, p ReconciliationPagePayload, partial bool) error {
	tx, err := s.repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockReconciliationRun(ctx, tx, p, "running"); err != nil {
		return err
	}
	status := "complete"
	if partial {
		status = "partial"
	}
	command, err := tx.Exec(ctx, `UPDATE catalog_search_runs SET status=$2,completed_at=$3,
		last_successful_at=CASE WHEN $2='complete' THEN $3 ELSE last_successful_at END,safe_error=''
		WHERE id=$1 AND run_revision=$4 AND retry_generation=$5 AND status='running'`,
		p.RunID, status, s.now().UTC(), p.RunRevision, p.PageGeneration)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return jobs.ErrLeaseLost
	}
	return tx.Commit(ctx)
}

func (s *SearchScanService) finishReconciliationAndQualify(ctx context.Context, p ReconciliationPagePayload, partial bool) error {
	if err := s.finishReconciliationPage(ctx, p, partial); err != nil {
		return err
	}
	if s.afterReconciliationTerminalHook != nil {
		if err := s.afterReconciliationTerminalHook(); err != nil {
			return err
		}
	}
	return s.enqueueReconciliationQualifications(ctx)
}

func (s *SearchScanService) enqueueReconciliationQualifications(ctx context.Context) error {
	_, err := s.repo.EnqueueReadyIngressQualifications(ctx, s.queue, s.cfg, s.now().UTC())
	return err
}

func (s *SearchScanService) reconciliationNewAppsUsed(ctx context.Context, p ReconciliationPagePayload) (int, error) {
	var total int
	err := s.repo.store.Pool.QueryRow(ctx, `SELECT new_app_count FROM catalog_search_runs WHERE id=$1
		AND run_revision=$2 AND retry_generation=$3`, p.RunID, p.RunRevision, p.PageGeneration).Scan(&total)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, jobs.ErrLeaseLost
	}
	return total, err
}

func lockReconciliationRun(ctx context.Context, tx pgx.Tx, p ReconciliationPagePayload, status string) error {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM catalog_search_runs WHERE id=$1 AND run_revision=$2
		AND retry_generation=$3 AND status=$4 FOR UPDATE`, p.RunID, p.RunRevision, p.PageGeneration, status).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.ErrLeaseLost
	}
	return err
}

func (s *SearchScanService) reconciliationFailure(ctx context.Context, lease jobs.Lease, p ReconciliationPagePayload, cause error) error {
	failure := classifyCatalogSearchFailure(cause)
	tx, err := s.repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockReconciliationRun(ctx, tx, p, "running"); err != nil {
		return err
	}
	if lease.Attempts < lease.MaxAttempts {
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		return jobs.NewCatalogRetryError(cause, failure.reason, retryAfterFromError(failure))
	}
	var used int
	if err = tx.QueryRow(ctx, `SELECT new_app_count FROM catalog_search_runs WHERE id=$1`, p.RunID).Scan(&used); err != nil {
		return err
	}
	manifest := reconciliationManifest{Version: 1, ResumeAfterAppID: p.AfterAppID, PartialReason: failure.reason,
		Continuation: true, ResumePage: p.Page, NewAppsUsed: used}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE sync_jobs SET result=$1,updated_at=now() WHERE id=$2 AND status='running'
		AND worker_id=$3 AND fencing_token=$4 AND lease_until>now()`, encoded, lease.ID, lease.WorkerID, lease.FencingToken)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return jobs.ErrLeaseLost
	}
	command, err := tx.Exec(ctx, `UPDATE catalog_search_runs SET status='failed',completed_at=$2,safe_error=$3
		WHERE id=$1 AND run_revision=$4 AND retry_generation=$5 AND status='running'`,
		p.RunID, s.now().UTC(), failure.reason, p.RunRevision, p.PageGeneration)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return jobs.ErrLeaseLost
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return cause
}

func (s *SearchScanService) persistReconciliationCandidate(ctx context.Context, p ReconciliationPagePayload, app steam.DiscoveredApp) (bool, bool, error) {
	if app.AppID < 1 || app.AppID > math.MaxUint32 || app.LastModified.IsZero() {
		return false, false, steam.ErrInvalidResponse
	}
	identity := fmt.Sprintf("%s:%d", p.RunID, app.AppID)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", app.AppID, app.LastModified.Unix())))
	fingerprint := hex.EncodeToString(sum[:])
	tx, err := s.repo.store.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback(ctx)
	if err = lockReconciliationRun(ctx, tx, p, "running"); err != nil {
		return false, false, err
	}
	q := s.repo.store.Queries.WithTx(tx)
	if err = q.LockMetadataApp(ctx, app.AppID); err != nil {
		return false, false, err
	}
	var seen bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_metadata_provenance
		WHERE steam_app_id=$1 AND ingress_kind='reconciliation' AND ingress_identity=$2)`, app.AppID, identity).Scan(&seen); err != nil {
		return false, false, err
	}
	if seen {
		return false, false, tx.Commit(ctx)
	}
	current, currentErr := q.LockCurrentMetadataCandidate(ctx, app.AppID)
	if currentErr != nil && !errors.Is(currentErr, pgx.ErrNoRows) {
		return false, false, currentErr
	}
	isNew := errors.Is(currentErr, pgx.ErrNoRows)
	if isNew {
		var total int
		if err = tx.QueryRow(ctx, `SELECT new_app_count FROM catalog_search_runs WHERE id=$1`, p.RunID).Scan(&total); err != nil {
			return false, false, err
		}
		if total >= s.cfg.SearchMaxNewAppsPerRun {
			return false, true, tx.Commit(ctx)
		}
	} else if current.MaterialFingerprint.Valid && current.MaterialFingerprint.String == fingerprint {
		head, headErr := q.LockMetadataCandidateHead(ctx, app.AppID)
		if headErr != nil {
			return false, false, headErr
		}
		material := CandidateRevision{RunRevision: head.MaterialRunRevision, ObservationOrdinal: head.MaterialObservationOrdinal}
		if head.MaterialFingerprint == fingerprint {
			if current.MaterialRunRevision.Int64 != material.RunRevision || current.MaterialObservationOrdinal.Int64 != material.ObservationOrdinal {
				if _, err = q.SyncCurrentMaterialCandidate(ctx, dbgen.SyncCurrentMaterialCandidateParams{
					RunRevision:        pgtype.Int8{Int64: material.RunRevision, Valid: true},
					ObservationOrdinal: pgtype.Int8{Int64: material.ObservationOrdinal, Valid: true},
					Fingerprint:        pgtype.Text{String: fingerprint, Valid: true}, SteamAppID: app.AppID}); err != nil {
					return false, false, err
				}
			}
			if err = q.RecordMetadataProvenance(ctx, dbgen.RecordMetadataProvenanceParams{SteamAppID: app.AppID,
				IngressKind: "reconciliation", IngressIdentity: identity, RunRevision: material.RunRevision,
				ObservationOrdinal: material.ObservationOrdinal, FirstSeenAt: pgtype.Timestamptz{Time: s.now().UTC(), Valid: true}}); err != nil {
				return false, false, err
			}
			if _, err = q.UpsertMetadataIngressWork(ctx, dbgen.UpsertMetadataIngressWorkParams{SteamAppID: app.AppID,
				IngressKind: "reconciliation", IngressIdentity: identity, RunRevision: material.RunRevision,
				ObservationOrdinal: material.ObservationOrdinal, Fingerprint: fingerprint, MaterialRunRevision: material.RunRevision,
				MaterialObservationOrdinal: material.ObservationOrdinal, MaterialFingerprint: fingerprint, Priority: 10,
				UpdatedAt: pgtype.Timestamptz{Time: s.now().UTC(), Valid: true}}); err != nil {
				return false, false, err
			}
			return false, false, tx.Commit(ctx)
		}
	}
	ordinal, err := q.AllocateObservationOrdinal(ctx, catalogUUID(p.RunID))
	if err != nil {
		return false, false, err
	}
	revision := CandidateRevision{RunRevision: p.RunRevision, ObservationOrdinal: ordinal}
	if isNew {
		if _, err = tx.Exec(ctx, `INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,
		material_run_revision,material_observation_ordinal,material_fingerprint,fingerprint,precision,review_band,priority)
		VALUES($1,$2,$3,$2,$3,$4,$4,'unknown','unknown',10)`, app.AppID, p.RunRevision, ordinal, fingerprint); err != nil {
			return false, false, err
		}
	}
	head, err := q.UpsertMetadataCandidateHead(ctx, dbgen.UpsertMetadataCandidateHeadParams{SteamAppID: app.AppID,
		RunRevision: revision.RunRevision, ObservationOrdinal: revision.ObservationOrdinal, Fingerprint: fingerprint,
		NowAt: pgtype.Timestamptz{Time: s.now().UTC(), Valid: true}})
	if err != nil {
		return false, false, err
	}
	material := CandidateRevision{RunRevision: head.MaterialRunRevision, ObservationOrdinal: head.MaterialObservationOrdinal}
	if !isNew {
		changed := current.MaterialRunRevision.Int64 != material.RunRevision ||
			current.MaterialObservationOrdinal.Int64 != material.ObservationOrdinal || current.MaterialFingerprint.String != head.MaterialFingerprint
		if _, err = tx.Exec(ctx, `UPDATE catalog_search_current SET material_run_revision=$2,
			material_observation_ordinal=$3,material_fingerprint=$4,
			next_refresh_at=CASE WHEN $5 THEN $6 ELSE next_refresh_at END WHERE steam_app_id=$1`,
			app.AppID, material.RunRevision, material.ObservationOrdinal, head.MaterialFingerprint, changed, s.now().UTC()); err != nil {
			return false, false, err
		}
	}
	if err = q.RecordMetadataProvenance(ctx, dbgen.RecordMetadataProvenanceParams{SteamAppID: app.AppID,
		IngressKind: "reconciliation", IngressIdentity: identity, RunRevision: revision.RunRevision,
		ObservationOrdinal: revision.ObservationOrdinal, FirstSeenAt: pgtype.Timestamptz{Time: s.now().UTC(), Valid: true}}); err != nil {
		return false, false, err
	}
	if _, err = q.UpsertMetadataIngressWork(ctx, dbgen.UpsertMetadataIngressWorkParams{SteamAppID: app.AppID,
		IngressKind: "reconciliation", IngressIdentity: identity, RunRevision: revision.RunRevision,
		ObservationOrdinal: revision.ObservationOrdinal, Fingerprint: fingerprint, MaterialRunRevision: material.RunRevision,
		MaterialObservationOrdinal: material.ObservationOrdinal, MaterialFingerprint: head.MaterialFingerprint, Priority: 10,
		UpdatedAt: pgtype.Timestamptz{Time: s.now().UTC(), Valid: true}}); err != nil {
		return false, false, err
	}
	if isNew {
		command, updateErr := tx.Exec(ctx, `UPDATE catalog_search_runs SET new_app_count=new_app_count+1
			WHERE id=$1 AND run_revision=$2 AND retry_generation=$3 AND status='running'`, p.RunID, p.RunRevision, p.PageGeneration)
		if updateErr != nil {
			return false, false, updateErr
		}
		if command.RowsAffected() != 1 {
			return false, false, jobs.ErrLeaseLost
		}
	}
	return isNew, false, tx.Commit(ctx)
}
