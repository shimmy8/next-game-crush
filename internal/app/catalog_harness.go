package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/store"
)

type catalogHarness struct {
	store     *store.Store
	scheduler *catalog.SearchScheduler
	restart   func(context.Context) error
}

type harnessPage struct {
	Feed   string  `json:"feed"`
	Start  int32   `json:"start"`
	AppIDs []int64 `json:"appIds"`
}
type harnessFeed struct {
	Status string `json:"status"`
}
type harnessRunState struct {
	Status          string                 `json:"status"`
	PublishedAppIDs []int64                `json:"publishedAppIds"`
	ProcessedAppIDs []int64                `json:"processedAppIds"`
	CommittedPages  []harnessPage          `json:"committedPages"`
	Feeds           map[string]harnessFeed `json:"feeds"`
}

func (h *catalogHarness) StartTestCatalogRun(ctx context.Context, kind string) (string, error) {
	var run catalog.RunRef
	var err error
	if kind == "reconciliation" {
		run, err = h.scheduler.EnqueueTestReconciliation(ctx, time.Now().UTC())
	} else if kind == "manual" {
		run, err = h.scheduler.EnqueueManual(ctx, time.Now().UTC())
	} else {
		return "", errors.New("unsupported catalog run kind")
	}
	return run.ID.String(), err
}

func (h *catalogHarness) RestartTestCatalogRuntime(ctx context.Context) error { return h.restart(ctx) }

func (h *catalogHarness) TestCatalogRun(ctx context.Context, raw string) (any, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, err
	}
	var status string
	if err = h.store.Pool.QueryRow(ctx, `SELECT status FROM catalog_search_runs WHERE id=$1`, id).Scan(&status); err != nil {
		return nil, err
	}
	result := harnessRunState{PublishedAppIDs: []int64{}, ProcessedAppIDs: []int64{}, CommittedPages: []harnessPage{}, Feeds: map[string]harnessFeed{}}
	switch status {
	case "complete":
		result.Status = "succeeded"
	case "partial":
		result.Status = "degraded"
	case "failed":
		result.Status = "dead"
	default:
		result.Status = status
	}
	var pending, failed bool
	if err = h.store.Pool.QueryRow(ctx, `WITH current_run AS (
		SELECT retry_generation FROM catalog_search_runs WHERE id=$1
	), candidates AS (
		SELECT steam_app_id,COALESCE(material_run_revision,run_revision) AS material_run_revision,
			COALESCE(material_observation_ordinal,observation_ordinal) AS material_observation_ordinal
		FROM catalog_search_worklist WHERE run_id=$1
		UNION SELECT steam_app_id,material_run_revision,material_observation_ordinal FROM catalog_metadata_ingress_work
		WHERE ingress_kind='reconciliation' AND ingress_identity LIKE $2
	), game_ids AS (SELECT g.id::text AS id FROM games g JOIN candidates c ON c.steam_app_id=g.steam_app_id),
	qualification_jobs AS (
		SELECT j.status FROM sync_jobs j JOIN candidates c ON j.payload->>'app_id'=c.steam_app_id::text
		WHERE j.kind='catalog.search.qualify'
			AND (j.payload->'candidate_revision'->>'run_revision')::bigint=c.material_run_revision
			AND (j.payload->'candidate_revision'->>'observation_ordinal')::bigint=c.material_observation_ordinal
	), ai_generations AS (
		SELECT j.payload->>'game_id' AS game_id,
			max(COALESCE(NULLIF(j.payload->'execution'->>'generation','')::bigint,0)) AS generation
		FROM sync_jobs j JOIN game_ids g ON j.payload->>'game_id'=g.id
		WHERE j.kind IN ('ai.features','ai.embedding') GROUP BY j.payload->>'game_id'
	), ai_jobs AS (
		SELECT j.status FROM sync_jobs j JOIN ai_generations g ON j.payload->>'game_id'=g.game_id
			AND COALESCE(NULLIF(j.payload->'execution'->>'generation','')::bigint,0)=g.generation
		WHERE j.kind IN ('ai.features','ai.embedding')
	), run_jobs AS (
		SELECT j.status FROM sync_jobs j CROSS JOIN current_run r WHERE j.payload->>'run_id'=$3 AND CASE
			WHEN j.kind='catalog.search.run' THEN (j.payload->>'generation')::integer=r.retry_generation
			WHEN j.kind IN ('catalog.search.page','catalog.reconciliation.page') THEN (j.payload->>'page_generation')::integer=r.retry_generation
			ELSE true END
	)
	SELECT EXISTS(SELECT 1 FROM catalog_search_worklist WHERE run_id=$1 AND status IN ('pending','reserved','deferred'))
	OR EXISTS(SELECT 1 FROM catalog_metadata_ingress_work WHERE ingress_kind='reconciliation' AND ingress_identity LIKE $2
		AND status IN ('pending','reserved','deferred'))
	OR EXISTS(SELECT 1 FROM run_jobs WHERE status IN ('queued','running','waiting','retryable'))
	OR EXISTS(SELECT 1 FROM qualification_jobs WHERE status IN ('queued','running','waiting','retryable'))
	OR EXISTS(SELECT 1 FROM ai_jobs WHERE status IN ('queued','running','waiting','retryable')),
	EXISTS(SELECT 1 FROM catalog_search_worklist WHERE run_id=$1 AND status='failed')
	OR EXISTS(SELECT 1 FROM catalog_metadata_ingress_work WHERE ingress_kind='reconciliation' AND ingress_identity LIKE $2
		AND status='failed')
	OR EXISTS(SELECT 1 FROM run_jobs WHERE status='dead')
	OR EXISTS(SELECT 1 FROM qualification_jobs WHERE status='dead')
	OR EXISTS(SELECT 1 FROM ai_jobs WHERE status='dead')`,
		id, id.String()+":%", id.String()).Scan(&pending, &failed); err != nil {
		return nil, err
	}
	if failed {
		result.Status = "dead"
	} else if pending {
		result.Status = "running"
	}
	rows, err := h.store.Pool.Query(ctx, `SELECT feed,status FROM catalog_search_feeds WHERE run_id=$1 ORDER BY feed`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var feed, state string
		if err = rows.Scan(&feed, &state); err != nil {
			rows.Close()
			return nil, err
		}
		result.Feeds[feed] = harnessFeed{Status: state}
	}
	rows.Close()
	rows, err = h.store.Pool.Query(ctx, `SELECT payload,result FROM sync_jobs WHERE kind='catalog.reconciliation.page' AND payload->>'run_id'=$1 AND result<>'{}'::jsonb ORDER BY created_at`, id.String())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var payload catalog.ReconciliationPagePayload
		var manifest struct {
			Apps []struct {
				AppID int64 `json:"appid"`
			} `json:"apps"`
		}
		var rawPayload, rawResult []byte
		if err = rows.Scan(&rawPayload, &rawResult); err != nil {
			rows.Close()
			return nil, err
		}
		if json.Unmarshal(rawPayload, &payload) != nil || json.Unmarshal(rawResult, &manifest) != nil {
			rows.Close()
			return nil, errors.New("invalid reconciliation audit state")
		}
		page := harnessPage{Feed: "reconciliation", Start: int32(payload.AfterAppID), AppIDs: []int64{}}
		for _, app := range manifest.Apps {
			page.AppIDs = append(page.AppIDs, app.AppID)
		}
		result.CommittedPages = append(result.CommittedPages, page)
	}
	rows.Close()
	rows, err = h.store.Pool.Query(ctx, `SELECT p.feed,p.start_offset,COALESCE(array_agg(DISTINCT o.steam_app_id ORDER BY o.steam_app_id) FILTER(WHERE o.steam_app_id IS NOT NULL),'{}')
		FROM catalog_search_pages p LEFT JOIN catalog_search_observations o ON o.run_id=p.run_id AND o.feed=p.feed AND o.start_offset=p.start_offset AND o.page_generation=p.page_generation
		WHERE p.run_id=$1 GROUP BY p.feed,p.start_offset ORDER BY p.feed,p.start_offset`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var page harnessPage
		if err = rows.Scan(&page.Feed, &page.Start, &page.AppIDs); err != nil {
			rows.Close()
			return nil, err
		}
		result.CommittedPages = append(result.CommittedPages, page)
	}
	rows.Close()
	if err = h.store.Pool.QueryRow(ctx, `WITH processed AS (
		SELECT steam_app_id FROM catalog_search_worklist WHERE run_id=$1 AND status IN ('complete','failed','superseded')
		UNION SELECT steam_app_id FROM catalog_metadata_ingress_work
		WHERE ingress_kind='reconciliation' AND ingress_identity LIKE $2 AND status IN ('complete','failed','superseded')
	) SELECT COALESCE(array_agg(steam_app_id ORDER BY steam_app_id),'{}') FROM processed`, id, id.String()+":%").Scan(&result.ProcessedAppIDs); err != nil {
		return nil, err
	}
	if err = h.store.Pool.QueryRow(ctx, `WITH candidates AS (
		SELECT steam_app_id FROM catalog_search_worklist WHERE run_id=$1
		UNION SELECT steam_app_id FROM catalog_metadata_ingress_work
		WHERE ingress_kind='reconciliation' AND ingress_identity LIKE $2
	) SELECT COALESCE(array_agg(DISTINCT candidates.steam_app_id ORDER BY candidates.steam_app_id),'{}')
	FROM candidates JOIN catalog_search_current c USING(steam_app_id) WHERE c.active`, id, id.String()+":%").Scan(&result.PublishedAppIDs); err != nil {
		return nil, err
	}
	sort.Slice(result.PublishedAppIDs, func(i, j int) bool { return result.PublishedAppIDs[i] < result.PublishedAppIDs[j] })
	return result, nil
}

func (app *App) runCatalogRuntime(ctx context.Context) error {
	for {
		child, cancel := context.WithCancel(ctx)
		results := make(chan error, 2)
		go func() { results <- app.catalogWorker.Run(child) }()
		go func() { results <- app.scheduler.Run(child) }()
		select {
		case err := <-results:
			cancel()
			<-results
			return err
		case ack := <-app.catalogRestart:
			cancel()
			first, second := <-results, <-results
			fatal := first
			if fatal == nil {
				fatal = second
			}
			ack <- fatal
			close(ack)
			if fatal != nil {
				return fatal
			}
			continue
		case <-ctx.Done():
			cancel()
			<-results
			<-results
			return nil
		}
	}
}

func (app *App) restartCatalogRuntime(ctx context.Context) error {
	ack := make(chan error, 1)
	select {
	case app.catalogRestart <- ack:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
