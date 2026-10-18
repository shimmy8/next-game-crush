package steam

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"strings"
	"time"
)

func (c *HTTPClient) ListChangedGames(ctx context.Context, since time.Time, afterAppID uint32, limit int) (DiscoveryPage, error) {
	if limit < 1 || limit > 200 || (!since.IsZero() && (since.Unix() < 0 || since.Unix() > math.MaxUint32)) {
		return DiscoveryPage{}, invalidResponse(AppListOperation)
	}
	input := map[string]any{"include_games": true, "include_dlc": false, "include_software": false, "include_videos": false, "include_hardware": false, "last_appid": afterAppID, "max_results": limit}
	if !since.IsZero() {
		input["if_modified_since"] = since.Unix()
	}
	encoded, _ := json.Marshal(input)
	response, _, err := c.getWithoutCache(ctx, CacheKey{Operation: AppListOperation}, c.storeServiceBase, "/IStoreService/GetAppList/v1/", url.Values{"key": {c.apiKey}, "input_json": {string(encoded)}})
	if err != nil {
		return DiscoveryPage{}, err
	}
	var payload struct {
		Response *struct {
			Apps      *[]json.RawMessage `json:"apps"`
			LastAppID *uint32            `json:"last_appid"`
			More      *bool              `json:"have_more_results"`
			Success   *bool              `json:"success"`
		} `json:"response"`
		Success *bool `json:"success"`
	}
	if err := decode(response.body, &payload, AppListOperation); err != nil {
		return DiscoveryPage{}, err
	}
	if payload.Response == nil || payload.Response.Apps == nil || payload.Response.More == nil || (payload.Success != nil && !*payload.Success) || (payload.Response.Success != nil && !*payload.Response.Success) {
		return DiscoveryPage{}, invalidResponse(AppListOperation)
	}
	p := payload.Response
	if len(*p.Apps) > limit {
		return DiscoveryPage{}, invalidResponse(AppListOperation)
	}
	page := DiscoveryPage{Apps: make([]DiscoveredApp, 0, len(*p.Apps)), NextAppID: afterAppID, HasMore: *p.More}
	if p.LastAppID != nil {
		page.NextAppID = *p.LastAppID
	}
	var maxID uint32
	for _, raw := range *p.Apps {
		var item struct {
			AppID    uint32 `json:"appid"`
			Name     string `json:"name"`
			Modified uint32 `json:"last_modified"`
		}
		if json.Unmarshal(raw, &item) != nil || item.AppID == 0 || item.Modified == 0 {
			page.Apps = append(page.Apps, DiscoveredApp{})
			continue
		}
		if item.AppID > maxID {
			maxID = item.AppID
		}
		name := strings.TrimSpace(item.Name)
		if len(name) > 512 {
			name = ""
		}
		page.Apps = append(page.Apps, DiscoveredApp{AppID: int64(item.AppID), Name: name, LastModified: time.Unix(int64(item.Modified), 0).UTC()})
	}
	if page.NextAppID < afterAppID || (page.HasMore && (len(page.Apps) == 0 || p.LastAppID == nil || page.NextAppID <= afterAppID || page.NextAppID < maxID)) {
		return DiscoveryPage{}, invalidResponse(AppListOperation)
	}
	return page, nil
}
