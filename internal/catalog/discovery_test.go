package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
)

type discoveryMetadata struct {
	steam.Client
	details          steam.GameDetails
	err              error
	deckStatus       steam.DeckCompatibility
	deckAvailability steam.Availability
	deckErr          error
	deckCalls        int
}

type discoveryPlanner struct{ calls int }

func (p *discoveryPlanner) PlanGameFeatureJob(_ context.Context, id uuid.UUID, hash string, priority int) (jobs.Spec, error) {
	p.calls++
	return jobs.Spec{Kind: "feature.test", IdempotencyKey: "feature:" + id.String() + ":" + hash, Priority: priority}, nil
}
func TestDiscoveryFeaturePlanningUsesContentAndModel(t *testing.T) {
	s, m := discoveryFixture(t)
	planner := &discoveryPlanner{}
	s.SetEnrichmentPlanner(planner)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	app := DiscoveredApp{AppID: 42, LastModified: now}
	l := qualificationLease(t, s, app, 0)
	if err := s.handleQualification(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Complete(ctx, l); err != nil {
		t.Fatal(err)
	}
	app.LastModified = app.LastModified.Add(time.Second)
	m.details.HeaderImage = "https://cdn.steamstatic.com/cover.jpg"
	l = leaseDiscoveryID(t, s, enqueueQualification(t, s, app, 0))
	if err := s.handleQualification(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Complete(ctx, l); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='feature.test'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("presentation-only revision scheduled new semantic work")
	}
	app.LastModified = app.LastModified.Add(time.Second)
	m.details.ShortDescription = "A changed game"
	l = leaseDiscoveryID(t, s, enqueueQualification(t, s, app, 0))
	if err := s.handleQualification(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE kind='feature.test'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatal("changed evidence did not schedule feature work")
	}
}
func enqueueQualification(t *testing.T, s *DiscoveryService, app DiscoveredApp, generation int) uuid.UUID {
	t.Helper()
	id, err := s.queue.Enqueue(context.Background(), qualificationSpec(QualificationPayload{Version: 1, App: app, RetryGeneration: generation, BudgetHour: s.now().UTC().Truncate(time.Hour), Priority: 50}, uuid.Nil))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (m *discoveryMetadata) GameDetails(context.Context, int64) (steam.GameDetails, error) {
	return m.details, m.err
}

func (m *discoveryMetadata) DeckCompatibility(context.Context, int64) (steam.DeckCompatibility, steam.Availability, error) {
	m.deckCalls++
	return m.deckStatus, m.deckAvailability, m.deckErr
}

type discoverySourceStub struct {
	pages []DiscoveryPage
	calls int
	since []time.Time
}

func (s *discoverySourceStub) ListChangedGames(_ context.Context, since time.Time, _ uint32, _ int) (DiscoveryPage, error) {
	s.since = append(s.since, since)
	s.calls++
	if len(s.pages) == 0 {
		return DiscoveryPage{}, errors.New("source unavailable")
	}
	p := s.pages[0]
	s.pages = s.pages[1:]
	return p, nil
}
func discoveryFixture(t *testing.T) (*DiscoveryService, *discoveryMetadata) {
	t.Helper()
	db := openCatalogTestStore(t)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	m := &discoveryMetadata{
		details:          steam.GameDetails{AppID: 42, Name: "Upcoming", Type: "game", ShortDescription: "A game", ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "Coming Soon"}},
		deckStatus:       steam.DeckUnknown,
		deckAvailability: steam.Available,
	}
	s, err := NewDiscoveryService(NewDiscoveryRepository(db), &discoverySourceStub{}, m, jobs.NewQueue(db), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, m
}

func TestDiscoveryAdmissionPersistsDeckCompatibility(t *testing.T) {
	s, metadata := discoveryFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	metadata.details.Platforms = steam.Platforms{Windows: true, Mac: false, Linux: true}
	metadata.deckStatus = steam.DeckVerified
	app := DiscoveredApp{AppID: 42, LastModified: now}
	lease := qualificationLease(t, s, app, 0)
	if err := s.handleQualification(ctx, lease); err != nil {
		t.Fatal(err)
	}
	var windows, mac, linux bool
	var status, availability string
	var succeededAt time.Time
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT windows,mac,linux,deck_status,deck_availability,deck_last_successful_fetch_at FROM game_compatibility`).Scan(&windows, &mac, &linux, &status, &availability, &succeededAt); err != nil {
		t.Fatal(err)
	}
	if !windows || mac || !linux || status != string(steam.DeckVerified) || availability != string(steam.Available) || !succeededAt.Equal(now) || metadata.deckCalls != 1 {
		t.Fatalf("compatibility=%v,%v,%v,%q,%q,%v calls=%d", windows, mac, linux, status, availability, succeededAt, metadata.deckCalls)
	}
}

func TestDiscoveryDeckFailurePreservesLastSuccess(t *testing.T) {
	for _, test := range []struct {
		name             string
		status           steam.DeckCompatibility
		availability     steam.Availability
		err              error
		wantAvailability steam.Availability
	}{
		{name: "unavailable", status: steam.DeckUnsupported, availability: steam.Unavailable, err: steam.ErrUnavailable, wantAvailability: steam.Unavailable},
		{name: "malformed", status: steam.DeckCompatibility("future"), availability: steam.Available, wantAvailability: steam.RequestFailed},
		{name: "request failure", status: steam.DeckUnknown, availability: steam.RequestFailed, err: errors.New("upstream failed"), wantAvailability: steam.RequestFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, metadata := discoveryFixture(t)
			ctx := context.Background()
			now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
			s.now = func() time.Time { return now }
			metadata.deckStatus = steam.DeckVerified
			app := DiscoveredApp{AppID: 42, LastModified: now}
			first := qualificationLease(t, s, app, 0)
			if err := s.handleQualification(ctx, first); err != nil {
				t.Fatal(err)
			}
			if err := s.queue.Complete(ctx, first); err != nil {
				t.Fatal(err)
			}
			metadata.deckStatus = test.status
			metadata.deckAvailability = test.availability
			metadata.deckErr = test.err
			now = now.Add(time.Hour)
			app.LastModified = app.LastModified.Add(time.Second)
			second := qualificationLease(t, s, app, 0)
			if err := s.handleQualification(ctx, second); err != nil {
				t.Fatalf("best-effort Deck failure blocked admission: %v", err)
			}
			var status, availability string
			var attemptedAt, succeededAt time.Time
			if err := s.repo.store.Pool.QueryRow(ctx, `SELECT deck_status,deck_availability,deck_last_attempt_at,deck_last_successful_fetch_at FROM game_compatibility`).Scan(&status, &availability, &attemptedAt, &succeededAt); err != nil {
				t.Fatal(err)
			}
			if status != string(steam.DeckVerified) || availability != string(test.wantAvailability) || !attemptedAt.Equal(now) || !succeededAt.Equal(now.Add(-time.Hour)) || metadata.deckCalls != 2 {
				t.Fatalf("Deck state=%q,%q attempt=%v success=%v calls=%d", status, availability, attemptedAt, succeededAt, metadata.deckCalls)
			}
		})
	}
}

func TestDiscoveryDeckFetchIsLimitedToAdmission(t *testing.T) {
	s, metadata := discoveryFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	metadata.details.ReleaseDate.Raw = "2030-01-01"
	app := DiscoveredApp{AppID: 42, LastModified: now}
	lease := qualificationLease(t, s, app, 0)
	if err := s.handleQualification(ctx, lease); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT status FROM catalog_discovery_observations WHERE steam_app_id=$1`, app.AppID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "outside_horizon" || metadata.deckCalls != 0 {
		t.Fatalf("qualification=%q Deck calls=%d", status, metadata.deckCalls)
	}
}

func qualificationLease(t *testing.T, s *DiscoveryService, app DiscoveredApp, generation int) jobs.Lease {
	t.Helper()
	raw, _ := json.Marshal(QualificationPayload{Version: 1, App: app, RetryGeneration: generation, BudgetHour: s.now().UTC().Truncate(time.Hour), Priority: 50})
	_, err := s.queue.Enqueue(context.Background(), jobs.Spec{Kind: QualifyJobKind, IdempotencyKey: qualificationKey(app, generation), Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.queue.Claim(context.Background(), uuid.NewString(), time.Minute)
	if err != nil || l == nil {
		t.Fatalf("claim: %v", err)
	}
	return *l
}
func TestDiscoveryQualificationCreatesSharedCandidate(t *testing.T) {
	s, _ := discoveryFixture(t)
	ctx := context.Background()
	app := DiscoveredApp{AppID: 42, LastModified: time.Now().UTC().Truncate(time.Second)}
	l := qualificationLease(t, s, app, 0)
	if err := s.handleQualification(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := s.handleQualification(ctx, l); err != nil {
		t.Fatal(err)
	}
	var games, observations, sources, releases, wishlists int
	err := s.repo.store.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM games),(SELECT count(*) FROM catalog_discovery_observations),(SELECT count(*) FROM catalog_entry_sources WHERE source='steam_discovery' AND active),(SELECT count(*) FROM game_release_dates),(SELECT count(*) FROM wishlist_entries)`).Scan(&games, &observations, &sources, &releases, &wishlists)
	if err != nil {
		t.Fatal(err)
	}
	if games != 1 || observations != 1 || sources != 1 || releases != 1 || wishlists != 0 {
		t.Fatalf("counts=%d,%d,%d,%d,%d", games, observations, sources, releases, wishlists)
	}
}
func TestQualificationRevisionReplayAndFailurePreservation(t *testing.T) {
	s, m := discoveryFixture(t)
	ctx := context.Background()
	app := DiscoveredApp{AppID: 42, LastModified: time.Now().UTC().Truncate(time.Second)}
	l := qualificationLease(t, s, app, 0)
	if err := s.handleQualification(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Complete(ctx, l); err != nil {
		t.Fatal(err)
	}
	app.LastModified = app.LastModified.Add(time.Second)
	m.err = steam.ErrUnavailable
	l = qualificationLease(t, s, app, 0)
	if err := s.handleQualification(ctx, l); err != nil {
		t.Fatal(err)
	}
	var active bool
	var name, status string
	if err := s.repo.store.Pool.QueryRow(ctx, `SELECT g.name,src.active,o.status FROM games g JOIN catalog_entry_sources src ON src.game_id=g.id CROSS JOIN catalog_discovery_observations o WHERE o.last_modified=$1`, app.LastModified).Scan(&name, &active, &status); err != nil {
		t.Fatal(err)
	}
	if name != "Upcoming" || !active || status != "unavailable" {
		t.Fatal("unavailability replaced canonical state")
	}
	stale := l
	stale.FencingToken = uuid.New()
	if err := s.handleQualification(ctx, stale); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale lease=%v", err)
	}
}
func TestDisabledDiscoverySchedulesNothing(t *testing.T) {
	s, _ := discoveryFixture(t)
	s.cfg.Enabled = false
	if err := s.EnsureScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.repo.store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM sync_jobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("disabled scheduling")
	}
}

func TestDiscoveryDisabledAndUnavailableRetainSeedCandidates(t *testing.T) {
	for _, test := range []struct {
		name        string
		enabled     bool
		unavailable bool
	}{
		{name: "disabled", enabled: false},
		{name: "source unavailable", enabled: true, unavailable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openCatalogTestStore(t)
			ctx := context.Background()
			now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
			seed := Seed{Version: 1, ObservedDate: "2026-10-17", Entries: []SeedEntry{{AppID: 730, ReleaseWindow: "2027", SourceURL: "https://store.steampowered.com/app/730/", ObservedDate: "2026-10-17"}}}
			catalogService, err := NewService(db, &modeSteamClient{}, seed)
			if err != nil {
				t.Fatal(err)
			}
			if err := catalogService.Bootstrap(ctx); err != nil {
				t.Fatal(err)
			}

			cfg := DefaultDiscoveryConfig()
			cfg.Enabled = test.enabled
			repo := NewDiscoveryRepository(db)
			discovery, err := NewDiscoveryService(repo, &discoverySourceStub{}, &modeSteamClient{}, jobs.NewQueue(db), cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			discovery.now = func() time.Time { return now }
			if !test.enabled {
				if err := discovery.EnsureScheduled(ctx); err != nil {
					t.Fatal(err)
				}
			} else if test.unavailable {
				state, err := repo.BeginScan(ctx, DiscoveryBackfill, now)
				if err != nil || state.ScanStartedAt == nil {
					t.Fatalf("begin scan: state=%#v err=%v", state, err)
				}
				payload := ScanPayload{Version: 1, Mode: DiscoveryBackfill, ScanStartedAt: *state.ScanStartedAt, BudgetHour: now.Truncate(time.Hour)}
				raw, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				queue := jobs.NewQueue(db)
				if _, err := queue.Enqueue(ctx, jobs.Spec{Kind: BackfillJobKind, IdempotencyKey: scanKey(payload), Payload: raw, Priority: 10}); err != nil {
					t.Fatal(err)
				}
				lease, err := queue.Claim(ctx, "discovery-unavailable-test", time.Minute)
				if err != nil || lease == nil {
					t.Fatalf("claim unavailable scan: lease=%#v err=%v", lease, err)
				}
				if err := discovery.handleScan(ctx, *lease); err == nil {
					t.Fatal("unavailable discovery source unexpectedly succeeded")
				}
			}

			var entries, activeSeed, semanticJobs int
			if err := db.Pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM catalog_entries),
				(SELECT count(*) FROM catalog_entry_sources WHERE source='seed' AND active),
				(SELECT count(*) FROM sync_jobs WHERE kind LIKE 'ai.%' OR kind LIKE 'feature.%')`).Scan(&entries, &activeSeed, &semanticJobs); err != nil {
				t.Fatal(err)
			}
			if entries != 1 || activeSeed != 1 || semanticJobs != 0 {
				t.Fatalf("seed fallback changed: entries=%d activeSeed=%d semanticJobs=%d", entries, activeSeed, semanticJobs)
			}
			status, err := discovery.DiscoveryStatus(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !test.enabled && status.Status != "disabled" {
				t.Fatalf("disabled status=%q", status.Status)
			}
			if test.unavailable && status.Status != "degraded" {
				t.Fatalf("unavailable status=%q", status.Status)
			}
		})
	}
}
