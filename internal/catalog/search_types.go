package catalog

import (
	"time"

	"github.com/google/uuid"
	"next-game-crush/internal/steam"
)

type RunKind string

const (
	RunScheduled              RunKind = "scheduled"
	RunManual                 RunKind = "manual"
	RunInitial                RunKind = "initial"
	RunReconciliation         RunKind = "reconciliation"
	SearchRunJobKind                  = "catalog.search.run"
	SearchPageJobKind                 = "catalog.search.page"
	ReconciliationPageJobKind         = "catalog.reconciliation.page"
)

type RunRef struct {
	ID         uuid.UUID
	Revision   int64
	Generation int
	Kind       RunKind
	OccurredAt time.Time
}

type SearchRunPayload struct {
	Version     int       `json:"version"`
	RunID       uuid.UUID `json:"run_id"`
	RunRevision int64     `json:"run_revision"`
	Generation  int       `json:"generation"`
}

type SearchPagePayload struct {
	Version        int              `json:"version"`
	RunID          uuid.UUID        `json:"run_id"`
	Feed           steam.SearchFeed `json:"feed"`
	Start          int              `json:"start"`
	PageGeneration int              `json:"page_generation"`
}

type ReconciliationPagePayload struct {
	Version        int       `json:"version"`
	RunID          uuid.UUID `json:"run_id"`
	RunRevision    int64     `json:"run_revision"`
	ScanStartedAt  time.Time `json:"scan_started_at"`
	Since          time.Time `json:"since"`
	AfterAppID     uint32    `json:"after_app_id"`
	PageGeneration int       `json:"page_generation"`
	Page           int       `json:"page"`
	NewAppCount    int       `json:"new_app_count"`
}

type CandidateRevision struct {
	RunRevision        int64 `json:"run_revision"`
	ObservationOrdinal int64 `json:"observation_ordinal"`
}

type FeedCompletion struct {
	Status                              string
	NaturalEnd, Monotonic, BoundaryStop bool
	SafetyCapReason                     string
}

type MetadataReservation struct {
	ID             uuid.UUID
	AppID          int64
	Revision       CandidateRevision
	Granted        bool
	Reason         string
	NextEligibleAt time.Time
	FencingToken   uuid.UUID
}
