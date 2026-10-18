package catalog

import (
	"context"
	"errors"
	"time"

	"next-game-crush/internal/steam"
)

type DiscoveryPage = steam.DiscoveryPage
type DiscoveredApp = steam.DiscoveredApp
type DiscoverySource interface {
	ListChangedGames(context.Context, time.Time, uint32, int) (DiscoveryPage, error)
}
type DiscoveryMode string

const (
	DiscoveryIncremental DiscoveryMode = "incremental"
	DiscoveryBackfill    DiscoveryMode = "backfill"
)

type Revision struct {
	AppID        int64
	LastModified time.Time
}
type Observation struct {
	Revision
	Status          string
	Window          ReleaseWindow
	Attempts        int
	RetryGeneration int
	NextRetryAt     *time.Time
	SafeError       string
}
type ScanState struct {
	Source          string
	Mode            DiscoveryMode
	CursorAppID     uint32
	WatermarkAt     *time.Time
	ScanStartedAt   *time.Time
	ScanCompletedAt *time.Time
	Availability    string
	LastError       string
}
type DiscoveryConfig struct {
	Enabled              bool
	HorizonDays          int
	IntervalHours        int
	HourlyBudget         int
	UnknownFreshnessDays int
	PageSize             int
}

func DefaultDiscoveryConfig() DiscoveryConfig {
	return DiscoveryConfig{HorizonDays: 90, IntervalHours: 1, HourlyBudget: 200, UnknownFreshnessDays: 180, PageSize: 100}
}
func (c DiscoveryConfig) Validate() error {
	if c.HorizonDays < 1 || c.HorizonDays > 366 || c.IntervalHours < 1 || c.IntervalHours > 24 || c.HourlyBudget < 1 || c.HourlyBudget > 1000 || c.UnknownFreshnessDays < 1 || c.UnknownFreshnessDays > 730 || c.PageSize < 1 || c.PageSize > 200 {
		return errors.New("discovery configuration is outside supported bounds")
	}
	return nil
}
