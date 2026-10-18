-- +goose Up
-- Global slot leases make the persisted per-host concurrency limit authoritative
-- across catalog clients and application processes.
CREATE TABLE catalog_host_throttle_slots (
    host text NOT NULL REFERENCES catalog_host_throttles(host) ON DELETE CASCADE,
    slot integer NOT NULL CHECK (slot BETWEEN 1 AND 16),
    fencing_token uuid,
    lease_until timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(host,slot),
    CHECK ((fencing_token IS NULL)=(lease_until IS NULL))
);
INSERT INTO catalog_host_throttles(host,concurrency_limit) VALUES
    ('store.steampowered.com',4),('api.steampowered.com',4),('api.openai.com',4)
ON CONFLICT(host) DO NOTHING;
INSERT INTO catalog_host_throttle_slots(host,slot)
SELECT host,slot FROM catalog_host_throttles CROSS JOIN generate_series(1,16) AS slot;

CREATE TABLE catalog_metadata_provenance (
    steam_app_id bigint NOT NULL CHECK (steam_app_id BETWEEN 1 AND 4294967295),
    ingress_kind text NOT NULL CHECK (ingress_kind IN ('scheduled','manual','initial','reconciliation','user_sync')),
    ingress_identity text NOT NULL CHECK (char_length(ingress_identity) BETWEEN 1 AND 256),
    run_revision bigint NOT NULL CHECK (run_revision>0),
    observation_ordinal bigint NOT NULL CHECK (observation_ordinal>0),
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(steam_app_id,ingress_kind,ingress_identity)
);
CREATE INDEX catalog_metadata_provenance_revision_idx
    ON catalog_metadata_provenance(steam_app_id,run_revision DESC,observation_ordinal DESC);

CREATE TABLE catalog_metadata_ingress_work (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    steam_app_id bigint NOT NULL CHECK (steam_app_id BETWEEN 1 AND 4294967295),
    ingress_kind text NOT NULL CHECK (ingress_kind IN ('scheduled','manual','initial','reconciliation','user_sync')),
    ingress_identity text NOT NULL CHECK (char_length(ingress_identity) BETWEEN 1 AND 256),
    run_revision bigint NOT NULL CHECK (run_revision>0),
    observation_ordinal bigint NOT NULL CHECK (observation_ordinal>0),
    fingerprint text NOT NULL DEFAULT '' CHECK (char_length(fingerprint)<=128),
    material_run_revision bigint NOT NULL CHECK (material_run_revision>0),
    material_observation_ordinal bigint NOT NULL CHECK (material_observation_ordinal>0),
    material_fingerprint text NOT NULL DEFAULT '' CHECK (char_length(material_fingerprint)<=128),
    priority smallint NOT NULL DEFAULT 90 CHECK (priority BETWEEN 1 AND 100),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','reserved','complete','deferred','failed','superseded')),
    next_eligible_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(steam_app_id,ingress_kind,ingress_identity)
);
CREATE INDEX catalog_metadata_ingress_work_due_idx ON catalog_metadata_ingress_work(status,next_eligible_at,priority DESC,created_at)
    WHERE status IN ('pending','reserved','deferred');

ALTER TABLE catalog_store_reservations
    ADD COLUMN budget_hour timestamptz,
    ADD COLUMN budget_identity_hash text CHECK (budget_identity_hash IS NULL OR char_length(budget_identity_hash)=64),
    ADD CONSTRAINT catalog_store_reservation_budget_pair CHECK ((budget_hour IS NULL)=(budget_identity_hash IS NULL));

-- Search review observations may advance without invalidating an in-flight
-- Store qualification. These columns retain the exact observation that last
-- changed material product identity/release data.
ALTER TABLE catalog_search_current
    ADD COLUMN material_run_revision bigint CHECK (material_run_revision IS NULL OR material_run_revision>0),
    ADD COLUMN material_observation_ordinal bigint CHECK (material_observation_ordinal IS NULL OR material_observation_ordinal>0),
    ADD COLUMN material_fingerprint text CHECK (material_fingerprint IS NULL OR char_length(material_fingerprint)<=128),
    ADD CONSTRAINT catalog_search_current_material_triplet CHECK (
        (material_run_revision IS NULL)=(material_observation_ordinal IS NULL)
        AND (material_run_revision IS NULL)=(material_fingerprint IS NULL));
UPDATE catalog_search_current c SET
    material_run_revision=CASE WHEN r.status='reserved' AND r.fingerprint=c.fingerprint
        AND NOT EXISTS(SELECT 1 FROM catalog_search_observations o WHERE o.steam_app_id=c.steam_app_id
            AND (o.run_revision,o.observation_ordinal)>(r.run_revision,r.observation_ordinal)
            AND (o.run_revision,o.observation_ordinal)<=(c.run_revision,c.observation_ordinal)
            AND o.fingerprint<>r.fingerprint) THEN r.run_revision ELSE c.run_revision END,
    material_observation_ordinal=CASE WHEN r.status='reserved' AND r.fingerprint=c.fingerprint
        AND NOT EXISTS(SELECT 1 FROM catalog_search_observations o WHERE o.steam_app_id=c.steam_app_id
            AND (o.run_revision,o.observation_ordinal)>(r.run_revision,r.observation_ordinal)
            AND (o.run_revision,o.observation_ordinal)<=(c.run_revision,c.observation_ordinal)
            AND o.fingerprint<>r.fingerprint) THEN r.observation_ordinal ELSE c.observation_ordinal END,
    material_fingerprint=CASE WHEN r.status='reserved' AND r.fingerprint=c.fingerprint
        AND NOT EXISTS(SELECT 1 FROM catalog_search_observations o WHERE o.steam_app_id=c.steam_app_id
            AND (o.run_revision,o.observation_ordinal)>(r.run_revision,r.observation_ordinal)
            AND (o.run_revision,o.observation_ordinal)<=(c.run_revision,c.observation_ordinal)
            AND o.fingerprint<>r.fingerprint) THEN r.fingerprint ELSE c.fingerprint END
FROM catalog_store_reservations r WHERE r.steam_app_id=c.steam_app_id;
UPDATE catalog_search_current SET material_run_revision=run_revision,
    material_observation_ordinal=observation_ordinal,material_fingerprint=fingerprint
WHERE material_run_revision IS NULL;

-- One source-independent head separates the latest observation from the exact
-- material candidate owned by Store/AI work. Search and non-search ingress both
-- advance it while holding the same per-AppID advisory lock.
CREATE TABLE catalog_metadata_heads (
    steam_app_id bigint PRIMARY KEY CHECK (steam_app_id BETWEEN 1 AND 4294967295),
    latest_run_revision bigint NOT NULL CHECK (latest_run_revision>0),
    latest_observation_ordinal bigint NOT NULL CHECK (latest_observation_ordinal>0),
    latest_fingerprint text NOT NULL CHECK (char_length(latest_fingerprint)<=128),
    material_run_revision bigint NOT NULL CHECK (material_run_revision>0),
    material_observation_ordinal bigint NOT NULL CHECK (material_observation_ordinal>0),
    material_fingerprint text NOT NULL CHECK (char_length(material_fingerprint)<=128),
    updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO catalog_metadata_heads(steam_app_id,latest_run_revision,latest_observation_ordinal,latest_fingerprint,
    material_run_revision,material_observation_ordinal,material_fingerprint)
SELECT steam_app_id,run_revision,observation_ordinal,fingerprint,
    material_run_revision,material_observation_ordinal,material_fingerprint
FROM catalog_search_current;
INSERT INTO catalog_metadata_heads(steam_app_id,latest_run_revision,latest_observation_ordinal,latest_fingerprint,
    material_run_revision,material_observation_ordinal,material_fingerprint)
SELECT steam_app_id,run_revision,observation_ordinal,fingerprint,run_revision,observation_ordinal,fingerprint
FROM catalog_store_reservations ON CONFLICT(steam_app_id) DO NOTHING;
ALTER TABLE catalog_search_worklist
    ADD COLUMN material_run_revision bigint CHECK (material_run_revision IS NULL OR material_run_revision>0),
    ADD COLUMN material_observation_ordinal bigint CHECK (material_observation_ordinal IS NULL OR material_observation_ordinal>0),
    ADD CONSTRAINT catalog_search_worklist_material_pair CHECK (
        (material_run_revision IS NULL)=(material_observation_ordinal IS NULL));
UPDATE catalog_search_worklist w SET material_run_revision=h.material_run_revision,
    material_observation_ordinal=h.material_observation_ordinal
FROM catalog_metadata_heads h WHERE h.steam_app_id=w.steam_app_id;
UPDATE catalog_search_worklist SET material_run_revision=run_revision,
    material_observation_ordinal=observation_ordinal WHERE material_run_revision IS NULL;
UPDATE catalog_search_worklist w SET status='pending',next_retry_at=NULL
FROM catalog_metadata_heads h LEFT JOIN catalog_store_reservations r ON r.steam_app_id=h.steam_app_id
WHERE w.steam_app_id=h.steam_app_id AND w.status='reserved'
  AND (r.steam_app_id IS NULL OR (r.run_revision,r.observation_ordinal,r.fingerprint)<>
      (h.material_run_revision,h.material_observation_ordinal,h.material_fingerprint));

-- +goose Down
ALTER TABLE catalog_store_reservations DROP CONSTRAINT catalog_store_reservation_budget_pair,
    DROP COLUMN budget_hour,DROP COLUMN budget_identity_hash;
ALTER TABLE catalog_search_current DROP CONSTRAINT catalog_search_current_material_triplet,
    DROP COLUMN material_run_revision,DROP COLUMN material_observation_ordinal,DROP COLUMN material_fingerprint;
ALTER TABLE catalog_search_worklist DROP CONSTRAINT catalog_search_worklist_material_pair,
    DROP COLUMN material_run_revision,DROP COLUMN material_observation_ordinal;
DROP TABLE catalog_metadata_provenance;
DROP TABLE catalog_metadata_ingress_work;
DROP TABLE catalog_metadata_heads;
DROP TABLE catalog_host_throttle_slots;
