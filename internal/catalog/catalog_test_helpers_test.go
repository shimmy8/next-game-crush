package catalog

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/steam"
	"next-game-crush/internal/store"
)

type searchQualificationPlanner struct{ calls int }

func (p *searchQualificationPlanner) PlanCatalogGameFeatureJob(_ context.Context, gameID uuid.UUID, hash string, priority int) (jobs.Spec, error) {
	p.calls++
	raw, _ := json.Marshal(map[string]any{"game_id": gameID, "content_hash": hash})
	return jobs.Spec{Lane: jobs.LaneCatalog, Kind: "feature.test", Stage: "feature", IdempotencyKey: "feature:" + gameID.String() + ":" + hash,
		Payload: raw, Priority: priority, MaxAttempts: 4}, nil
}

func enableSearchQualificationProducer(t *testing.T, scan *SearchScanService, repo *SearchRepository) {
	t.Helper()
	scan.SetSearchQualificationProducer(repo)
}

func searchQualificationFixture(t *testing.T, observation steam.SearchObservation, details steam.GameDetails) (*DiscoveryService, *discoveryMetadata, *SearchRepository, jobs.Lease) {
	t.Helper()
	db := openCatalogTestStore(t)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	repo := NewSearchRepository(db)
	scan, err := NewSearchScanService(repo, &searchStub{}, jobs.NewQueue(db), cfg, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	enableSearchQualificationProducer(t, scan, repo)
	run := searchTestRun(t, repo, cfg, now.Add(-time.Hour))
	if err := scan.CommitPage(context.Background(), run, steam.FeedPopularUpcoming, 0, 0,
		steam.SearchPage{Start: 0, Count: 1, Total: 1, Items: []steam.SearchObservation{observation}}); err != nil {
		t.Fatal(err)
	}
	if err := scan.CommitPage(context.Background(), run, steam.FeedPopularNew, 0, 0,
		steam.SearchPage{Start: 0, Count: 0, Total: 0}); err != nil {
		t.Fatal(err)
	}
	metadata := &discoveryMetadata{details: details}
	service, err := NewDiscoveryService(NewDiscoveryRepository(db), metadata, jobs.NewQueue(db), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	lease, err := service.queue.Claim(context.Background(), jobs.LaneCatalog, "search-qualification-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if lease == nil || lease.Kind != SearchQualificationJobKind {
		t.Fatalf("claimed %+v", lease)
	}
	return service, metadata, repo, *lease
}

func openCatalogTestStore(t *testing.T) *store.Store {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Path != "/next_game_crush_test" && parsed.Path != "/next_game_crush_e2e") {
		t.Fatal("requires isolated test database")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := "catalog_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	admin.Close(ctx)
	t.Cleanup(func() {
		conn, e := pgx.Connect(context.Background(), raw)
		if e == nil {
			defer conn.Close(context.Background())
			_, _ = conn.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		}
	})
	query := parsed.Query()
	query.Set("search_path", schema+",public")
	parsed.RawQuery = query.Encode()
	database, err := store.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	return database
}

type discoveryMetadata struct {
	steam.Client
	details          steam.GameDetails
	err              error
	deckStatus       steam.DeckCompatibility
	deckAvailability steam.Availability
	deckErr          error
	deckCalls        int
	calls            int
}

func (m *discoveryMetadata) GameDetails(context.Context, int64) (steam.GameDetails, error) {
	m.calls++
	return m.details, m.err
}

func (m *discoveryMetadata) DeckCompatibility(context.Context, int64) (steam.DeckCompatibility, steam.Availability, error) {
	m.deckCalls++
	return m.deckStatus, m.deckAvailability, m.deckErr
}

func discoveryFixture(t *testing.T) (*DiscoveryService, *discoveryMetadata) {
	t.Helper()
	db := openCatalogTestStore(t)
	cfg := DefaultDiscoveryConfig()
	cfg.Enabled = true
	metadata := &discoveryMetadata{details: steam.GameDetails{AppID: 42, Name: "Upcoming", Type: "game",
		ShortDescription: "A game", ReleaseDate: steam.ReleaseDate{ComingSoon: true, Raw: "Coming Soon"}},
		deckStatus: steam.DeckUnknown, deckAvailability: steam.Available}
	service, err := NewDiscoveryService(NewDiscoveryRepository(db), metadata, jobs.NewQueue(db), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return service, metadata
}
