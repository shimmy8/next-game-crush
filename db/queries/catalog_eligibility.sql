-- name: IsCatalogGameEligible :one
WITH requested AS (SELECT sqlc.arg(game_id)::uuid AS game_id),
facts AS (
    SELECT requested.game_id,release.precision,release.release_date,release.raw_text,
        COALESCE(current_search.coming_soon,
            CASE WHEN jsonb_typeof(g.source_snapshot->'ReleaseDate'->'ComingSoon')='boolean'
				 THEN (g.source_snapshot->'ReleaseDate'->>'ComingSoon')::boolean END,
			CASE WHEN g.source='catalog_seed' THEN true END) AS coming_soon,
		current_search.review_band,current_search.tombstone_reason,current_search.last_successful_appdetails_at,
        EXISTS(SELECT 1 FROM wishlist_entries w WHERE w.game_id=g.id) AS wishlisted,
        EXISTS(SELECT 1 FROM catalog_entries ce WHERE ce.game_id=g.id) AS shared_source,
        EXISTS(SELECT 1 FROM catalog_search_memberships m WHERE m.steam_app_id=g.steam_app_id AND m.feed='popular_upcoming' AND m.active) AS popular_upcoming,
        EXISTS(SELECT 1 FROM catalog_search_memberships m WHERE m.steam_app_id=g.steam_app_id AND m.feed='popular_new' AND m.active) AS popular_new,
		(CASE WHEN g.source='catalog_seed' AND current_search.last_successful_appdetails_at IS NULL THEN true
			ELSE lower(btrim(COALESCE(g.source_snapshot->>'Type','')))='game' END) AS is_game
    FROM requested JOIN games g ON g.id=requested.game_id
    LEFT JOIN catalog_search_current current_search ON current_search.steam_app_id=g.steam_app_id
    LEFT JOIN LATERAL (
        SELECT precision,release_date,raw_text FROM game_release_dates
        WHERE game_release_dates.game_id=g.id ORDER BY observed_at DESC,id DESC LIMIT 1
    ) release ON true
), parsed AS (
    SELECT facts.*,
        CASE
			WHEN precision='day' THEN release_date
			WHEN precision='month' AND raw_text~'^[0-9]{4}-(0[1-9]|1[0-2])$' AND left(raw_text,4)<>'0000'
				THEN (raw_text||'-01')::date
			WHEN precision='month' AND raw_text~*'^(Jan(uary)?|Feb(ruary)?|Mar(ch)?|Apr(il)?|May|Jun(e)?|Jul(y)?|Aug(ust)?|Sep(tember)?|Oct(ober)?|Nov(ember)?|Dec(ember)?),? [0-9]{4}$'
				AND substring(raw_text from '([0-9]{4})$')<>'0000'
				THEN CASE WHEN length(split_part(replace(raw_text,',',''),' ',1))=3
					THEN to_date(replace(raw_text,',',''),'Mon YYYY')
					ELSE to_date(replace(raw_text,',',''),'Month YYYY') END
			WHEN precision='quarter' AND raw_text~*'^Q[1-4]\s*[,/-]?\s*[0-9]{4}$'
				AND substring(raw_text from '([0-9]{4})$')<>'0000'
				THEN make_date(substring(raw_text from '([0-9]{4})$')::integer,(substring(raw_text from '[Qq]([1-4])')::integer-1)*3+1,1)
			WHEN precision='year' AND raw_text~'^[0-9]{4}$' AND raw_text<>'0000' THEN make_date(raw_text::integer,1,1)
		END AS starts_at
    FROM facts
), spans AS (
	SELECT parsed.*,
		CASE precision WHEN 'day' THEN starts_at
			WHEN 'month' THEN (starts_at+interval '1 month - 1 day')::date
			WHEN 'quarter' THEN (starts_at+interval '3 months - 1 day')::date
			WHEN 'year' THEN (starts_at+interval '1 year - 1 day')::date END AS ends_at
	FROM parsed
), eligible AS (
    SELECT game_id FROM spans
    WHERE (shared_source OR wishlisted) AND is_game AND tombstone_reason IS DISTINCT FROM 'not_game' AND coming_soon IS NOT NULL AND (
        (coming_soon AND (
            (precision IN ('day','month','quarter') AND ends_at>=sqlc.arg(today)::date AND starts_at<=sqlc.arg(today)::date+sqlc.arg(horizon_days)::integer)
            OR (precision IN ('year','coming_soon','unknown') AND (popular_upcoming OR wishlisted))))
        OR (NOT coming_soon AND (
            ((precision IN ('day','month','quarter') AND ends_at>=sqlc.arg(today)::date-sqlc.arg(lookback_days)::integer AND starts_at<=sqlc.arg(today)::date)
             OR (precision IN ('year','coming_soon','unknown') AND (popular_upcoming OR wishlisted)))
            AND (review_band='positive' OR (review_band='unrated' AND precision='day'
                AND release_date BETWEEN sqlc.arg(today)::date-7 AND sqlc.arg(today)::date AND popular_new))))
    )
)
SELECT EXISTS(SELECT 1 FROM eligible);

-- name: FilterCatalogEligibleGameIDs :many
WITH requested AS (SELECT unnest(sqlc.arg(game_ids)::uuid[]) AS game_id),
facts AS (
    SELECT requested.game_id,release.precision,release.release_date,release.raw_text,
        COALESCE(current_search.coming_soon,
            CASE WHEN jsonb_typeof(g.source_snapshot->'ReleaseDate'->'ComingSoon')='boolean'
				 THEN (g.source_snapshot->'ReleaseDate'->>'ComingSoon')::boolean END,
			CASE WHEN g.source='catalog_seed' THEN true END) AS coming_soon,
		current_search.review_band,current_search.tombstone_reason,current_search.last_successful_appdetails_at,
        EXISTS(SELECT 1 FROM wishlist_entries w WHERE w.game_id=g.id) AS wishlisted,
        EXISTS(SELECT 1 FROM catalog_entries ce WHERE ce.game_id=g.id) AS shared_source,
        EXISTS(SELECT 1 FROM catalog_search_memberships m WHERE m.steam_app_id=g.steam_app_id AND m.feed='popular_upcoming' AND m.active) AS popular_upcoming,
        EXISTS(SELECT 1 FROM catalog_search_memberships m WHERE m.steam_app_id=g.steam_app_id AND m.feed='popular_new' AND m.active) AS popular_new,
		(CASE WHEN g.source='catalog_seed' AND current_search.last_successful_appdetails_at IS NULL THEN true
			ELSE lower(btrim(COALESCE(g.source_snapshot->>'Type','')))='game' END) AS is_game
    FROM requested JOIN games g ON g.id=requested.game_id
    LEFT JOIN catalog_search_current current_search ON current_search.steam_app_id=g.steam_app_id
    LEFT JOIN LATERAL (
        SELECT precision,release_date,raw_text FROM game_release_dates
        WHERE game_release_dates.game_id=g.id ORDER BY observed_at DESC,id DESC LIMIT 1
    ) release ON true
), parsed AS (
    SELECT facts.*,
        CASE
			WHEN precision='day' THEN release_date
			WHEN precision='month' AND raw_text~'^[0-9]{4}-(0[1-9]|1[0-2])$' AND left(raw_text,4)<>'0000'
				THEN (raw_text||'-01')::date
			WHEN precision='month' AND raw_text~*'^(Jan(uary)?|Feb(ruary)?|Mar(ch)?|Apr(il)?|May|Jun(e)?|Jul(y)?|Aug(ust)?|Sep(tember)?|Oct(ober)?|Nov(ember)?|Dec(ember)?),? [0-9]{4}$'
				AND substring(raw_text from '([0-9]{4})$')<>'0000'
				THEN CASE WHEN length(split_part(replace(raw_text,',',''),' ',1))=3
					THEN to_date(replace(raw_text,',',''),'Mon YYYY')
					ELSE to_date(replace(raw_text,',',''),'Month YYYY') END
			WHEN precision='quarter' AND raw_text~*'^Q[1-4]\s*[,/-]?\s*[0-9]{4}$'
				AND substring(raw_text from '([0-9]{4})$')<>'0000'
				THEN make_date(substring(raw_text from '([0-9]{4})$')::integer,(substring(raw_text from '[Qq]([1-4])')::integer-1)*3+1,1)
			WHEN precision='year' AND raw_text~'^[0-9]{4}$' AND raw_text<>'0000' THEN make_date(raw_text::integer,1,1)
		END AS starts_at
    FROM facts
), spans AS (
	SELECT parsed.*,
		CASE precision WHEN 'day' THEN starts_at
			WHEN 'month' THEN (starts_at+interval '1 month - 1 day')::date
			WHEN 'quarter' THEN (starts_at+interval '3 months - 1 day')::date
			WHEN 'year' THEN (starts_at+interval '1 year - 1 day')::date END AS ends_at
	FROM parsed
), eligible AS (
    SELECT game_id FROM spans
    WHERE (shared_source OR wishlisted) AND is_game AND tombstone_reason IS DISTINCT FROM 'not_game' AND coming_soon IS NOT NULL AND (
        (coming_soon AND (
            (precision IN ('day','month','quarter') AND ends_at>=sqlc.arg(today)::date AND starts_at<=sqlc.arg(today)::date+sqlc.arg(horizon_days)::integer)
            OR (precision IN ('year','coming_soon','unknown') AND (popular_upcoming OR wishlisted))))
        OR (NOT coming_soon AND (
            ((precision IN ('day','month','quarter') AND ends_at>=sqlc.arg(today)::date-sqlc.arg(lookback_days)::integer AND starts_at<=sqlc.arg(today)::date)
             OR (precision IN ('year','coming_soon','unknown') AND (popular_upcoming OR wishlisted)))
            AND (review_band='positive' OR (review_band='unrated' AND precision='day'
                AND release_date BETWEEN sqlc.arg(today)::date-7 AND sqlc.arg(today)::date AND popular_new))))
    )
)
SELECT DISTINCT game_id::text AS game_id FROM eligible ORDER BY game_id;
