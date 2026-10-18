package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/catalog"
	"next-game-crush/internal/notifications"
	"next-game-crush/internal/profile"
	"next-game-crush/internal/recommendations"
)

func TestCurrentEligibilityProtectsEveryDeliveryPathEndToEnd(t *testing.T) {
	db := openHTTPTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	clockNow := now
	clock := func() time.Time { return clockNow }
	userID, profileID := uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id) VALUES($1)`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO steam_accounts(user_id,steam_id,display_name) VALUES($1,'76561198000000999','Eligibility Player')`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO preference_profiles(id,user_id,version,created_at) VALUES($1,$2,1,$3)`, profileID, userID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendation_runs(id,user_id,profile_id,ranking_version,algorithm_version,build_key,published,created_at) VALUES($1,$2,$3,1,$4,'eligibility-e2e',true,$5)`, uuid.New(), userID, profileID, recommendations.AlgorithmVersion, now); err != nil {
		t.Fatal(err)
	}

	type fixture struct {
		gameID, recommendationID uuid.UUID
		appID                    int64
		name                     string
		date                     time.Time
		comingSoon               bool
		review                   string
		score                    float64
		popularNew               bool
	}
	fixtures := []fixture{
		{uuid.New(), uuid.New(), 95001, "A Day Seven Unrated", now.AddDate(0, 0, -7), false, "unrated", 100, true},
		{uuid.New(), uuid.New(), 95002, "B Mixed", now, false, "mixed_or_below", 99, false},
		{uuid.New(), uuid.New(), 95003, "C Outside", now.AddDate(0, 0, 32), true, "unknown", 98, false},
	}
	for i := 0; i < 11; i++ {
		item := fixture{uuid.New(), uuid.New(), int64(95010 + i), fmt.Sprintf("Eligible %02d", i), now.AddDate(0, 0, i+1), true, "unknown", float64(90 - i), false}
		if i == 0 {
			item.date = now
			item.comingSoon = false
			item.review = "positive"
		}
		fixtures = append(fixtures, item)
	}
	for _, item := range fixtures {
		snapshot := fmt.Sprintf(`{"AppID":%d,"Type":"game","Name":"%s","ReleaseDate":{"ComingSoon":%t,"Raw":"%s"}}`, item.appID, item.name, item.comingSoon, item.date.Format(time.DateOnly))
		if _, err := db.Pool.Exec(ctx, `INSERT INTO games(id,steam_app_id,name,source,last_successful_fetch_at,source_snapshot) VALUES($1,$2,$3,'steam_store',$4,$5)`, item.gameID, item.appID, item.name, now, snapshot); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_entries(game_id,origin) VALUES($1,'seed')`, item.gameID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO game_release_dates(id,game_id,precision,release_date,raw_text,observed_at,source,last_successful_fetch_at) VALUES($1,$2,'day',$3,$4,$5,'steam_store',$5)`, uuid.New(), item.gameID, item.date, item.date.Format(time.DateOnly), now); err != nil {
			t.Fatal(err)
		}
		var percent any
		total := int64(0)
		label := ""
		if item.review == "mixed_or_below" {
			percent = 55
			total = 100
			label = "Mixed"
		}
		if item.review == "unrated" {
			label = "No user reviews"
		}
		if item.review == "positive" {
			percent = 91
			total = 1284
			label = "Very Positive"
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_current(steam_app_id,run_revision,observation_ordinal,coming_soon,precision,release_date,review_band,review_label,positive_percent,review_total,review_observed_at,last_successful_appdetails_at,active) VALUES($1,1,1,$2,'day',$3,$4,$5,$6,$7,$8,$8,true)`, item.appID, item.comingSoon, item.date, item.review, label, percent, total, now); err != nil {
			t.Fatal(err)
		}
		if item.popularNew {
			if _, err := db.Pool.Exec(ctx, `INSERT INTO catalog_search_memberships(feed,steam_app_id,published_run_revision,active) VALUES('popular_new',$1,1,true)`, item.appID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO wishlist_entries(user_id,game_id,source,last_successful_fetch_at) VALUES($1,$2,'test',$3)`, userID, item.gameID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendations(id,user_id,game_id,profile_id,algorithm_version,ranking_version,score,explanation,created_at) VALUES($1,$2,$3,$4,$5,1,$6,'Объяснение',$7)`, item.recommendationID, userID, item.gameID, profileID, recommendations.AlgorithmVersion, item.score, now); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO recommendation_factors(recommendation_id,factors,weights) VALUES($1,'{}','{}')`, item.recommendationID); err != nil {
			t.Fatal(err)
		}
	}
	eligibility := catalog.NewEligibilityRepository(db)
	profileRepo, err := profile.NewSQLRepository(db, "embed")
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.NewService(profileRepo)
	if err != nil {
		t.Fatal(err)
	}
	rankingRepo, err := recommendations.NewSQLRepository(db, "embed", "text")
	if err != nil {
		t.Fatal(err)
	}
	ranking, err := recommendations.NewService(rankingRepo, profiles, nil, eligibility, clock)
	if err != nil {
		t.Fatal(err)
	}
	digests, err := notifications.NewSQLRepository(db, eligibility, clock)
	if err != nil {
		t.Fatal(err)
	}

	assertReads := func(daySeven bool) {
		t.Helper()
		ineligible := []uuid.UUID{fixtures[1].gameID, fixtures[2].gameID}
		if !daySeven {
			ineligible = append(ineligible, fixtures[0].gameID)
		}
		latest, err := ranking.Latest(ctx, userID)
		if err != nil {
			t.Fatal(err)
		}
		want := 11
		if daySeven {
			want = 12
		}
		if len(latest) != want {
			t.Fatalf("latest=%d want=%d", len(latest), want)
		}
		for _, item := range latest {
			for _, bad := range ineligible {
				if item.GameID == bad {
					t.Fatalf("ineligible latest item leaked: %s", bad)
				}
			}
		}
		byName := make(map[string]recommendations.Recommendation, len(latest))
		for _, item := range latest {
			byName[item.Name] = item
		}
		if daySeven {
			unrated := byName[fixtures[0].name].Reviews
			if unrated == nil || unrated.Band != recommendations.ReviewUnrated || unrated.PositivePercent != nil || unrated.Total != 0 || !unrated.ObservedAt.Equal(now) {
				t.Fatalf("unrated review projection=%#v", unrated)
			}
		}
		rated := byName[fixtures[3].name].Reviews
		if rated == nil || rated.Band != recommendations.ReviewPositive || rated.PositivePercent == nil || *rated.PositivePercent != 91 || rated.Total != 1284 || !rated.ObservedAt.Equal(now) {
			t.Fatalf("rated review projection=%#v", rated)
		}
		for _, item := range latest {
			if item.Name != fixtures[0].name && item.Name != fixtures[3].name && item.Reviews != nil {
				t.Fatalf("upcoming/missing review projection=%s %#v", item.Name, item.Reviews)
			}
		}
		overview, err := ranking.Overview(ctx, userID)
		if err != nil {
			t.Fatal(err)
		}
		calendarCount := len(overview.Calendar.ExactDates) + len(overview.Calendar.Month) + len(overview.Calendar.Quarter) + len(overview.Calendar.Year) + len(overview.Calendar.ComingSoon) + len(overview.Calendar.TBA)
		if calendarCount != want {
			t.Fatalf("overview calendar=%d want=%d", calendarCount, want)
		}
		overviewJSON, err := json.Marshal(overview)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range ineligible {
			if strings.Contains(string(overviewJSON), bad.String()) {
				t.Fatalf("ineligible overview/calendar item leaked: %s", bad)
			}
		}
		if strings.Contains(string(overviewJSON), "tooltip") || strings.Contains(string(overviewJSON), "<") {
			t.Fatalf("overview leaked raw review markup: %s", overviewJSON)
		}
		input, err := digests.DigestInput(ctx, userID)
		if err != nil {
			t.Fatal(err)
		}
		if len(input.Recommendations) != 5 || len(input.Wishlist) != 10 {
			t.Fatalf("digest limits rec=%d wish=%d", len(input.Recommendations), len(input.Wishlist))
		}
		for _, bad := range ineligible {
			for _, x := range input.Recommendations {
				if x.GameID == bad {
					t.Fatalf("ineligible recommendation leaked: %s", bad)
				}
			}
			for _, x := range input.Wishlist {
				if x.GameID == bad {
					t.Fatalf("ineligible wishlist leaked: %s", bad)
				}
			}
		}
		for _, index := range []int{1, 2} {
			if _, err := ranking.Get(ctx, userID, fixtures[index].recommendationID); err == nil {
				t.Fatalf("ineligible detail remained readable: %s", fixtures[index].gameID)
			}
		}
		_, detailErr := ranking.Get(ctx, userID, fixtures[0].recommendationID)
		if daySeven && detailErr != nil {
			t.Fatal(detailErr)
		}
		if !daySeven && detailErr == nil {
			t.Fatal("day-eight detail remained eligible")
		}
	}
	assertReads(true)
	authService, sessions := handlerService(t, "http://localhost:8080")
	session := sessions.addSession()
	session.UserID = userID
	// The in-memory auth repository owns the session value returned above.
	sessions.sessions[session.ID] = session
	router := NewRouter(Dependencies{Auth: authService, Recommendations: ranking})
	detailStatus := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/recommendations/"+fixtures[0].recommendationID.String(), nil)
		req.AddCookie(&http.Cookie{Name: "ngc_session", Value: session.Token})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}
	if got := detailStatus(); got != http.StatusOK {
		t.Fatalf("day-seven HTTP detail=%d", got)
	}
	for _, index := range []int{1, 2} {
		req := httptest.NewRequest(http.MethodGet, "/api/recommendations/"+fixtures[index].recommendationID.String(), nil)
		req.AddCookie(&http.Cookie{Name: "ngc_session", Value: session.Token})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("ineligible HTTP detail index=%d status=%d", index, w.Code)
		}
	}
	for _, path := range []string{"/api/recommendations/overview", "/api/calendar"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: "ngc_session", Value: session.Token})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), fixtures[1].gameID.String()) || strings.Contains(w.Body.String(), fixtures[2].gameID.String()) {
			t.Fatalf("path=%s status=%d body=%s", path, w.Code, w.Body.String())
		}
		var body any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}
	clockNow = now.AddDate(0, 0, 1)
	assertReads(false)
	if got := detailStatus(); got != http.StatusNotFound {
		t.Fatalf("day-eight HTTP detail=%d", got)
	}
	var historical int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM recommendations WHERE user_id=$1`, userID).Scan(&historical); err != nil || historical != len(fixtures) {
		t.Fatalf("historical=%d want=%d err=%v", historical, len(fixtures), err)
	}
}
