package ai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/jobs"
	"next-game-crush/internal/store"
)

func TestCatalogExecutionPayloadPinsContentModelsAndGeneration(t *testing.T) {
	s := &Service{model: "text-v1", embeddingModel: "embedding-v1"}
	id := uuid.New()
	spec, err := s.PlanGameFeatureJob(context.Background(), id, "content", 50)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodeJobPayload(spec.Payload, true, false); err != nil {
		t.Fatal("a directly planned job must remain executable")
	}
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(spec.Kind+":"+spec.IdempotencyKey)))
	key := fmt.Sprintf("catalog:enrichment:v1:%s:7:%s", id, identity)
	bound, err := s.BindGameFeatureExecution(spec, 7, key)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := decodeJobPayload(bound.Payload, true, false)
	if err != nil {
		t.Fatal(err)
	}
	plan, current, err := s.executionForJob(payload, jobs.Lease{IdempotencyKey: key})
	if err != nil || !current || plan.Generation != 7 || plan.ContentHash != "content" || plan.TextModel != "text-v1" || plan.EmbeddingModel != "embedding-v1" {
		t.Fatal("planned execution lost its identity")
	}
	if _, current, err = s.executionForJob(payload, jobs.Lease{IdempotencyKey: key + ":embedding:v1"}); err != nil || !current {
		t.Fatal("embedding did not inherit its execution")
	}
	s.embeddingModel = "embedding-v2"
	if _, current, err = s.executionForJob(payload, jobs.Lease{IdempotencyKey: key}); err != nil || current {
		t.Fatal("an old embedding model was still current")
	}
	payload.Execution.ContentHash = "tampered"
	invalid, _ := json.Marshal(payload)
	if _, err = decodeJobPayload(invalid, true, false); err == nil {
		t.Fatal("tampered execution identity was accepted")
	}
}

type blockedManualFeatures struct {
	Client
	started, release chan struct{}
}

func (c *blockedManualFeatures) ExtractGameFeatures(ctx context.Context, evidence GameEvidence) (GameFeatures, error) {
	close(c.started)
	select {
	case <-c.release:
	case <-ctx.Done():
		return GameFeatures{}, ctx.Err()
	}
	return GameFeatures{Themes: []string{"old"}, Mechanics: []string{}, Modes: []string{}, Tone: []string{}, EvidenceKeys: []string{"title"}}, nil
}

func TestLegacyFeatureHandlerCannotPublishAfterContentOrLeaseChanges(t *testing.T) {
	for _, change := range []string{"content", "lease"} {
		t.Run(change, func(t *testing.T) {
			db := openAIIntegrationStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			gameID := uuid.New()
			if err := db.UpsertGame(ctx, store.Game{ID: gameID, SteamAppID: 42, Name: "Original", Source: "test", FetchedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			repo := NewSQLRepository(db)
			client := &blockedManualFeatures{started: make(chan struct{}), release: make(chan struct{})}
			service, err := NewService(repo, client, "text-v1", "embedding-v1")
			if err != nil {
				t.Fatal(err)
			}
			queue := jobs.NewQueue(db)
			raw, _ := json.Marshal(JobPayload{Version: 1, GameID: gameID})
			if _, err = queue.Enqueue(ctx, jobs.Spec{Lane: jobs.LaneInteractive, Kind: FeaturesJobKind, IdempotencyKey: "foreground:legacy", Payload: raw}); err != nil {
				t.Fatal(err)
			}
			lease, err := queue.Claim(ctx, jobs.LaneInteractive, "legacy-execution", time.Minute)
			if err != nil || lease == nil {
				t.Fatal("legacy job was not claimable")
			}
			done := make(chan error, 1)
			go func() { done <- service.handleFeatures(ctx, *lease) }()
			select {
			case <-client.started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if change == "content" {
				if _, err = db.Pool.Exec(ctx, `UPDATE games SET name='Current' WHERE id=$1`, gameID); err != nil {
					t.Fatal(err)
				}
				hash, _, err := featureEvidenceHash(StoredGameEvidence{Name: "Current"}, "text-v1")
				if err != nil {
					t.Fatal(err)
				}
				if err = repo.SaveGameFeatures(ctx, gameID, "text-v1", hash, GameFeatures{Themes: []string{"current"}}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err = db.Pool.Exec(ctx, `UPDATE sync_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, lease.ID); err != nil {
					t.Fatal(err)
				}
			}
			close(client.release)
			err = <-done
			if change == "lease" {
				if !errors.Is(err, jobs.ErrLeaseLost) {
					t.Fatalf("expired lease publication error=%v", err)
				}
				var count int
				if err = db.Pool.QueryRow(ctx, `SELECT count(*) FROM game_features`).Scan(&count); err != nil || count != 0 {
					t.Fatal("expired worker published features")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				var theme string
				if err = db.Pool.QueryRow(ctx, `SELECT features->'themes'->>0 FROM game_features WHERE game_id=$1`, gameID).Scan(&theme); err != nil || theme != "current" {
					t.Fatal("legacy handler overwrote newer canonical features")
				}
			}
		})
	}
}
