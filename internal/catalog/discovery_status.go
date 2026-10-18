package catalog

import (
	"context"
	"time"
)

type PresentationStatus struct {
	Status                string     `json:"status"`
	ActiveDiscoveredGames int64      `json:"activeDiscoveredGames"`
	LastSuccessfulScanAt  *time.Time `json:"lastSuccessfulScanAt"`
	BackfillCompletedAt   *time.Time `json:"backfillCompletedAt"`
}
type DiscoveryStatusReader interface {
	DiscoveryStatus(context.Context) (PresentationStatus, error)
}

type DiscoveryRunCounts struct {
	Discovered int64 `json:"discovered"`
	Evaluated  int64 `json:"evaluated"`
	Admitted   int64 `json:"admitted"`
	Skipped    int64 `json:"skipped"`
	Failed     int64 `json:"failed"`
}

type DiscoveryDiagnostics struct {
	PresentationStatus
	BackfillCursor        uint32             `json:"backfillCursor"`
	LatestRun             DiscoveryRunCounts `json:"latestRun"`
	ActiveBySource        map[string]int64   `json:"activeBySource"`
	PendingQualifications int64              `json:"pendingQualifications"`
	Reason                string             `json:"reason,omitempty"`
}
