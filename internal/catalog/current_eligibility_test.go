package catalog

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCurrentEligibilityWindowReviewAndProvenance(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	repo := NewEligibilityRepository(db)

	insert := func(appID int64, precision string, release time.Time, comingSoon bool, review string, wishlist, popularUpcoming, popularNew bool) uuid.UUID {
		t.Helper()
		gameID := uuid.New()
		raw := release.Format(time.DateOnly)
		switch precision {
		case "month":
			raw = release.Format("Jan 2006")
		case "quarter":
			raw = fmt.Sprintf("Q%d %d", (int(release.Month())-1)/3+1, release.Year())
		case "year":
			raw = fmt.Sprintf("%d", release.Year())
		case "coming_soon", "unknown":
			raw = "Coming Soon"
		}
		snapshot := fmt.Sprintf(`{"AppID":%d,"Type":"game","Name":"Game","ReleaseDate":{"ComingSoon":%t,"Raw":"%s"}}`, appID, comingSoon, raw)
		if _, err := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at,source_snapshot) VALUES($1,$2,'Game','test',$3,$4)`, gameID, appID, now, snapshot); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, gameID); err != nil {
			t.Fatal(err)
		}
		var date any
		if precision == "day" {
			date = release
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,release_date,raw_text,observed_at,source,last_successful_fetch_at) VALUES($1,$2,$3,$4,$5,$6,'test',$6)`, uuid.New(), gameID, precision, date, raw, now); err != nil {
			t.Fatal(err)
		}
		var currentDate any
		if precision == "day" {
			currentDate = release
		}
		month, quarter := int(release.Month()), (int(release.Month())-1)/3+1
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,coming_soon,precision,release_date,release_year,release_month,release_quarter,review_band,active) VALUES($1,1,1,$2,$3,$4,$5,$6,$7,$8,true)`, appID, comingSoon, precision, currentDate, release.Year(), month, quarter, review); err != nil {
			t.Fatal(err)
		}
		if wishlist {
			userID := uuid.New()
			if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',$3)`, userID, gameID, now); err != nil {
				t.Fatal(err)
			}
		}
		for feed, active := range map[string]bool{"popular_upcoming": popularUpcoming, "popular_new": popularNew} {
			if active {
				if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_memberships(feed,steam_app_id,published_run_revision,active) VALUES($1,$2,1,true)`, feed, appID); err != nil {
					t.Fatal(err)
				}
			}
		}
		return gameID
	}

	cases := []struct {
		name string
		id   uuid.UUID
		want bool
	}{
		{"released lookback boundary", insert(91001, "day", now.AddDate(0, 0, -30), false, "positive", false, false, false), true},
		{"released before lookback", insert(91002, "day", now.AddDate(0, 0, -31), false, "positive", true, true, true), false},
		{"upcoming horizon boundary", insert(91003, "day", now.AddDate(0, 0, 30), true, "unknown", false, false, false), true},
		{"upcoming after horizon", insert(91004, "day", now.AddDate(0, 0, 31), true, "positive", true, true, true), false},
		{"released mixed", insert(91005, "day", now.AddDate(0, 0, -1), false, "mixed_or_below", true, true, true), false},
		{"released unknown", insert(91006, "day", now.AddDate(0, 0, -1), false, "unknown", true, true, true), false},
		{"unrated day seven popular", insert(91007, "day", now.AddDate(0, 0, -7), false, "unrated", false, false, true), true},
		{"unrated day eight", insert(91008, "day", now.AddDate(0, 0, -8), false, "unrated", true, true, true), false},
		{"unrated not popular new", insert(91009, "day", now, false, "unrated", true, true, false), false},
		{"coarse upcoming popular", insert(91010, "coming_soon", now, true, "unknown", false, true, false), true},
		{"coarse upcoming no provenance", insert(91011, "coming_soon", now, true, "unknown", false, false, false), false},
		{"coarse released unrated", insert(91012, "year", now, false, "unrated", true, true, true), false},
		{"released month intersects lookback", insert(91013, "month", now, false, "positive", false, false, false), true},
		{"upcoming quarter intersects horizon", insert(91014, "quarter", now, true, "unknown", false, false, false), true},
	}
	ids := make([]uuid.UUID, 0, len(cases))
	for _, tc := range cases {
		ids = append(ids, tc.id)
	}
	filtered, err := repo.FilterEligibleGameIDs(ctx, ids, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.Eligible(ctx, tc.id, now)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want || filtered[tc.id] != tc.want {
				t.Fatalf("eligible=%v filtered=%v want=%v", got, filtered[tc.id], tc.want)
			}
		})
	}
}

func TestCurrentEligibilityPreservesLastKnownGoodOnRefreshFailure(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	gameID := uuid.New()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at,source_snapshot) VALUES($1,92001,'LKG','test',$2,'{"AppID":92001,"Type":"game","Name":"LKG","ReleaseDate":{"ComingSoon":false,"Raw":"18 Oct 2026"}}')`, []any{gameID, now}},
		{`INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, []any{gameID}},
		{`INSERT INTO game_release_dates(id,game_id,precision,release_date,raw_text,observed_at,source,last_successful_fetch_at) VALUES($1,$2,'day',$3,'18 Oct 2026',$4,'test',$4)`, []any{uuid.New(), gameID, now, now}},
		{`INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,coming_soon,precision,release_date,review_band,active,safe_error) VALUES(92001,1,1,false,'day',$1,'positive',true,'request_failed')`, []any{now}},
	}
	for _, statement := range statements {
		if _, err := db.Pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	got, err := NewEligibilityRepository(db).Eligible(ctx, gameID, now)
	if err != nil || !got {
		t.Fatalf("last-known-good eligibility=%v err=%v", got, err)
	}
}

func TestCurrentEligibilityPreservesReviewedSeedFallback(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	gameID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at,source_snapshot) VALUES($1,92501,'Seed','catalog_seed',$2,'{"app_id":92501,"release_window":"Oct 2026"}')`, gameID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, gameID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,raw_text,observed_at,source,last_successful_fetch_at) VALUES($1,$2,'month','Oct 2026',$3,'catalog_seed:test',$3)`, uuid.New(), gameID, now); err != nil {
		t.Fatal(err)
	}
	got, err := NewEligibilityRepository(db).Eligible(ctx, gameID, now)
	if err != nil || !got {
		t.Fatalf("seed fallback eligibility=%v err=%v", got, err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,last_successful_appdetails_at,tombstone_reason,active) VALUES(92501,1,1,$1,'not_game',false)`, now); err != nil {
		t.Fatal(err)
	}
	got, err = NewEligibilityRepository(db).Eligible(ctx, gameID, now)
	batch, batchErr := NewEligibilityRepository(db).FilterEligibleGameIDs(ctx, []uuid.UUID{gameID}, now)
	if err != nil || batchErr != nil || got || batch[gameID] {
		t.Fatalf("authoritative not_game single=%v batch=%v err=%v/%v", got, batch, err, batchErr)
	}
}

func TestCurrentEligibilityUsesCanonicalReleaseWhenSearchDiverges(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	repo := NewEligibilityRepository(db)
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	fixtures := []struct {
		id                                     uuid.UUID
		appID                                  int64
		precision, raw                         string
		searchYear, searchMonth, searchQuarter int
	}{
		{ids[0], 92601, "month", "Oct 2026", 2027, 1, 0},
		{ids[1], 92602, "quarter", "Q4 2026", 2027, 0, 1},
	}
	for _, item := range fixtures {
		snapshot := fmt.Sprintf(`{"AppID":%d,"Type":"game","Name":"Canonical","ReleaseDate":{"ComingSoon":true,"Raw":"%s"}}`, item.appID, item.raw)
		if _, err := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at,source_snapshot) VALUES($1,$2,'Canonical','steam_store',$3,$4)`, item.id, item.appID, now, snapshot); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, item.id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,raw_text,observed_at,source,last_successful_fetch_at) VALUES($1,$2,$3,$4,$5,'steam_store',$5)`, uuid.New(), item.id, item.precision, item.raw, now); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,coming_soon,precision,release_year,release_month,release_quarter,review_band,last_successful_appdetails_at,safe_error,active) VALUES($1,2,2,true,$2,$3,NULLIF($4,0),NULLIF($5,0),'unknown',$6,'request_failed',true)`, item.appID, item.precision, item.searchYear, item.searchMonth, item.searchQuarter, now.Add(-24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := repo.FilterEligibleGameIDs(ctx, ids, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		single, err := repo.Eligible(ctx, id, now)
		if err != nil || !single || !batch[id] {
			t.Fatalf("canonical LKG id=%s single=%v batch=%v err=%v", id, single, batch[id], err)
		}
	}
}

func TestCurrentEligibilityConfiguredWindowsSingleMatchesBatch(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	repo := NewEligibilityRepository(db)
	if err := repo.SetWindowDays(10, 45); err != nil {
		t.Fatal(err)
	}
	insert := func(appID int64, date time.Time, comingSoon bool) uuid.UUID {
		id := uuid.New()
		snapshot := fmt.Sprintf(`{"AppID":%d,"Type":"game","Name":"Window","ReleaseDate":{"ComingSoon":%t,"Raw":"%s"}}`, appID, comingSoon, date.Format(time.DateOnly))
		if _, err := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at,source_snapshot) VALUES($1,$2,'Window','steam_store',$3,$4)`, id, appID, now, snapshot); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,release_date,raw_text,observed_at,source,last_successful_fetch_at) VALUES($1,$2,'day',$3,$4,$5,'steam_store',$5)`, uuid.New(), id, date, date.Format(time.DateOnly), now); err != nil {
			t.Fatal(err)
		}
		review := "unknown"
		if !comingSoon {
			review = "positive"
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,coming_soon,precision,release_date,review_band,last_successful_appdetails_at,active) VALUES($1,1,1,$2,'day',$3,$4,$5,true)`, appID, comingSoon, date, review, now); err != nil {
			t.Fatal(err)
		}
		return id
	}
	cases := []struct {
		id   uuid.UUID
		want bool
	}{{insert(92701, now.AddDate(0, 0, -10), false), true}, {insert(92702, now.AddDate(0, 0, -11), false), false}, {insert(92703, now.AddDate(0, 0, 45), true), true}, {insert(92704, now.AddDate(0, 0, 46), true), false}}
	ids := make([]uuid.UUID, 0, len(cases))
	for _, tc := range cases {
		ids = append(ids, tc.id)
	}
	batch, err := repo.FilterEligibleGameIDs(ctx, ids, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		single, err := repo.Eligible(ctx, tc.id, now)
		if err != nil || single != tc.want || batch[tc.id] != tc.want {
			t.Fatalf("id=%s single=%v batch=%v want=%v err=%v", tc.id, single, batch[tc.id], tc.want, err)
		}
	}
}

func TestCurrentEligibilityFailsClosedForMalformedOrNeverConfirmedStoreData(t *testing.T) {
	db := openCatalogTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	userID := uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	fixtures := []struct {
		id       uuid.UUID
		appID    int64
		source   string
		snapshot string
		raw      string
	}{
		{ids[0], 92801, "steam_wishlist", `{}`, "Oct 2026"},
		{ids[1], 92802, "steam_store", `{"AppID":92802,"Type":"game","Name":"Malformed","ReleaseDate":{"ComingSoon":true,"Raw":"definitely not a month"}}`, "definitely not a month"},
	}
	for _, item := range fixtures {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at,source_snapshot) VALUES($1,$2,'Fail closed',$3,$4,$5)`, item.id, item.appID, item.source, now, item.snapshot); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',$3)`, userID, item.id, now); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,raw_text,observed_at,source,last_successful_fetch_at) VALUES($1,$2,'month',$3,$4,'steam_store',$4)`, uuid.New(), item.id, item.raw, now); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,coming_soon,precision,review_band,active) VALUES($1,1,1,true,'month','unknown',true)`, item.appID); err != nil {
			t.Fatal(err)
		}
	}
	repo := NewEligibilityRepository(db)
	batch, err := repo.FilterEligibleGameIDs(ctx, ids, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		single, err := repo.Eligible(ctx, id, now)
		if err != nil || single || batch[id] {
			t.Fatalf("id=%s single=%v batch=%v err=%v", id, single, batch[id], err)
		}
	}
}
