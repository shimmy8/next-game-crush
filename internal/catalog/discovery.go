package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"next-game-crush/internal/jobs"
	"next-game-crush/internal/observability"
	"next-game-crush/internal/steam"
)

// DiscoveryService now owns only Search qualification and catalog rebuild
// coordination. The retired GetAppList hourly workflow is deliberately absent.
type DiscoveryService struct {
	repo          *DiscoveryRepository
	metadata      steam.Client
	queue         *jobs.Queue
	cfg           DiscoveryConfig
	searchPlanner SearchDiscoveryEnrichmentPlanner
	now           func() time.Time
	logger        *observability.Logger
}

func NewDiscoveryService(repo *DiscoveryRepository, metadata steam.Client, queue *jobs.Queue, cfg DiscoveryConfig, logger *observability.Logger) (*DiscoveryService, error) {
	if repo == nil || repo.store == nil || metadata == nil || queue == nil {
		return nil, errors.New("discovery dependencies are required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = observability.New(nil)
	}
	return &DiscoveryService{repo: repo, metadata: metadata, queue: queue, cfg: cfg, logger: logger, now: time.Now}, nil
}

func strictDiscoveryPayload(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("payload must contain one object")
	}
	return nil
}

func (s *DiscoveryService) DiscoveryStatus(ctx context.Context) (PresentationStatus, error) {
	return s.repo.SearchPresentationStatus(ctx, s.cfg.Enabled, s.now().UTC())
}

func (s *DiscoveryService) Diagnostics(ctx context.Context) (DiscoveryDiagnostics, error) {
	return s.repo.SearchDiscoveryDiagnostics(ctx, s.cfg.Enabled, s.now().UTC())
}
