package ai

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestDiscoveryFeaturePlanningModelIdentity(t *testing.T) {
	gameID := uuid.New()
	service := &Service{model: "text-v1", embeddingModel: "embedding-v1"}
	first, err := service.PlanGameFeatureJob(context.Background(), gameID, "content", 50)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := service.PlanGameFeatureJob(context.Background(), gameID, "content", 80)
	if first.IdempotencyKey != again.IdempotencyKey || again.Priority != 80 {
		t.Fatal("priority changed job identity")
	}
	service.embeddingModel = "embedding-v2"
	embedding, _ := service.PlanGameFeatureJob(context.Background(), gameID, "content", 50)
	if embedding.IdempotencyKey == first.IdempotencyKey {
		t.Fatal("embedding version missing from identity")
	}
	service.model = "text-v2"
	text, _ := service.PlanGameFeatureJob(context.Background(), gameID, "content", 50)
	if text.IdempotencyKey == embedding.IdempotencyKey {
		t.Fatal("text model missing from identity")
	}
	changed, _ := service.PlanGameFeatureJob(context.Background(), gameID, "changed", 50)
	if changed.IdempotencyKey == text.IdempotencyKey {
		t.Fatal("content missing from identity")
	}
}
