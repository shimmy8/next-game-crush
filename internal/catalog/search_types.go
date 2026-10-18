package catalog

import (
	"time"

	"github.com/google/uuid"
)

type RunKind string

const (
	RunScheduled      RunKind = "scheduled"
	RunManual         RunKind = "manual"
	RunInitial        RunKind = "initial"
	RunReconciliation RunKind = "reconciliation"
	SearchRunJobKind          = "catalog.search.run"
	SearchPageJobKind         = "catalog.search.page"
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
