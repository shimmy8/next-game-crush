package sync

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"next-game-crush/internal/steam"
)

func TestWishlistFailedPersistenceCannotAdvanceSuccessfulFetch(t *testing.T) {
	for _, failure := range []struct {
		name string
		sql  string
	}{
		{
			name: "membership replacement",
			sql:  `ALTER TABLE wishlist_entries ADD CONSTRAINT reject_replacement CHECK (priority <> 99)`,
		},
		{
			name: "source success marker",
			sql: `ALTER TABLE user_source_states ADD CONSTRAINT reject_new_wishlist_success CHECK (
				source <> 'wishlist' OR availability <> 'available' OR last_successful_fetch_at <= '2026-10-17T00:00:00Z'::timestamptz)`,
		},
	} {
		t.Run(failure.name, func(t *testing.T) {
			service, fake, db, userID := newTestService(t)
			ctx := context.Background()
			if err := service.importSteam(ctx, userID); err != nil {
				t.Fatal(err)
			}
			before := service.now().UTC()
			if _, err := db.Pool.Exec(ctx, failure.sql); err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			fake.wishlist = []steam.WishlistItem{{AppID: 40, Priority: 99}}
			fake.mu.Unlock()
			after := before.Add(time.Hour)
			service.now = func() time.Time { return after }
			if err := service.importSteam(ctx, userID); err == nil {
				t.Fatal("wishlist import succeeded despite injected persistence failure")
			}
			var membership []int64
			if err := db.Pool.QueryRow(ctx, `SELECT array_agg(g.steam_app_id ORDER BY g.steam_app_id)
				FROM wishlist_entries w JOIN games g ON g.id=w.game_id WHERE w.user_id=$1`, userID).Scan(&membership); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(membership, []int64{20}) {
				t.Fatalf("membership after failed replacement = %v, want prior snapshot [20]", membership)
			}
			var availability, safeError string
			var lastAttempt, lastSuccess, membershipSuccess time.Time
			if err := db.Pool.QueryRow(ctx, `SELECT availability,last_attempt_at,last_successful_fetch_at,COALESCE(error,'')
				FROM user_source_states WHERE user_id=$1 AND source='wishlist' AND resource_key=''`, userID).Scan(&availability, &lastAttempt, &lastSuccess, &safeError); err != nil {
				t.Fatal(err)
			}
			if availability != string(steam.RequestFailed) || !lastAttempt.Equal(after) || !lastSuccess.Equal(before) || safeError != "Steam wishlist request failed" {
				t.Fatalf("failed wishlist state = %s/%s/%s/%q, want request_failed with prior success and bounded error", availability, lastAttempt, lastSuccess, safeError)
			}
			if err := db.Pool.QueryRow(ctx, `SELECT last_successful_fetch_at FROM wishlist_entries WHERE user_id=$1`, userID).Scan(&membershipSuccess); err != nil {
				t.Fatal(err)
			}
			if !membershipSuccess.Equal(before) {
				t.Fatalf("membership success = %s, want %s", membershipSuccess, before)
			}
		})
	}
}

func TestWishlistRefreshStatesPreserveOrReplaceSnapshot(t *testing.T) {
	for _, test := range []struct {
		name        string
		state       steam.Availability
		err         error
		wantCount   int
		wantSuccess bool
	}{
		{name: "unavailable", state: steam.Unavailable, wantCount: 1},
		{name: "request failed", state: steam.RequestFailed, err: errors.New("raw upstream canary"), wantCount: 1},
		{name: "not modified", state: steam.NotModified, err: steam.ErrNotModified, wantCount: 1, wantSuccess: true},
		{name: "successful empty", state: steam.Available, wantSuccess: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, fake, db, userID := newTestService(t)
			ctx := context.Background()
			if err := service.importSteam(ctx, userID); err != nil {
				t.Fatal(err)
			}
			before := service.now().UTC()
			after := before.Add(time.Hour)
			service.now = func() time.Time { return after }
			fake.mu.Lock()
			fake.wishlist, fake.wishlistAvailability, fake.wishlistErr = nil, test.state, test.err
			fake.mu.Unlock()
			if err := service.importSteam(ctx, userID); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM wishlist_entries WHERE user_id=$1`, userID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			var availability string
			var lastAttempt, lastSuccess time.Time
			if err := db.Pool.QueryRow(ctx, `SELECT availability,last_attempt_at,last_successful_fetch_at
				FROM user_source_states WHERE user_id=$1 AND source='wishlist' AND resource_key=''`, userID).Scan(&availability, &lastAttempt, &lastSuccess); err != nil {
				t.Fatal(err)
			}
			wantSuccess := before
			if test.wantSuccess {
				wantSuccess = after
			}
			if count != test.wantCount || availability != string(test.state) || !lastAttempt.Equal(after) || !lastSuccess.Equal(wantSuccess) {
				t.Fatalf("wishlist = count %d, availability %s, attempt %s, success %s; want %d/%s/%s/%s", count, availability, lastAttempt, lastSuccess, test.wantCount, test.state, after, wantSuccess)
			}
		})
	}
}

func TestWishlistSuccessUsesOneTimestamp(t *testing.T) {
	service, _, db, userID := newTestService(t)
	now := service.now().UTC()
	service.now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	ctx := context.Background()
	if err := service.importSteam(ctx, userID); err != nil {
		t.Fatal(err)
	}
	var membershipSuccess, sourceAttempt, sourceSuccess time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT w.last_successful_fetch_at,s.last_attempt_at,s.last_successful_fetch_at
		FROM wishlist_entries w JOIN user_source_states s ON s.user_id=w.user_id
		WHERE w.user_id=$1 AND s.source='wishlist' AND s.resource_key=''`, userID).Scan(&membershipSuccess, &sourceAttempt, &sourceSuccess); err != nil {
		t.Fatal(err)
	}
	if !membershipSuccess.Equal(sourceAttempt) || !membershipSuccess.Equal(sourceSuccess) {
		t.Fatalf("wishlist snapshot and state timestamps differ: membership %s, attempt %s, success %s", membershipSuccess, sourceAttempt, sourceSuccess)
	}
}
