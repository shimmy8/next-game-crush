package steam

import "time"

type DiscoveredApp struct {
	AppID        int64
	Name         string
	LastModified time.Time
}

type DiscoveryPage struct {
	Apps      []DiscoveredApp
	NextAppID uint32
	HasMore   bool
}
