// Command e2e-fakes serves deterministic Steam and OpenAI responses for the
// browser smoke test. It is never linked into the application binary.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func main() {
	var steamPrivacy atomic.Int32
	var wishlistMode atomic.Int32
	var catalogMode atomic.Int32
	var catalogRequests atomic.Int32
	var explanationMode atomic.Int32
	var achievementMode atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/__test/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		steamPrivacy.Store(0)
		wishlistMode.Store(0)
		catalogMode.Store(0)
		catalogRequests.Store(0)
		explanationMode.Store(0)
		achievementMode.Store(0)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/__test/openai-explanation-mode", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Query().Get("mode") {
		case "success":
			explanationMode.Store(0)
		case "rate_limited":
			explanationMode.Store(1)
		case "invalid_citation":
			explanationMode.Store(2)
		case "unsafe_text":
			explanationMode.Store(3)
		case "provider_unavailable":
			explanationMode.Store(4)
		default:
			http.Error(w, "unknown fixture mode", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/__test/openai-achievement-mode", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Query().Get("mode") {
		case "success":
			achievementMode.Store(0)
		case "invalid":
			achievementMode.Store(1)
		case "provider_unavailable":
			achievementMode.Store(2)
		case "hundred_percent":
			achievementMode.Store(3)
		default:
			http.Error(w, "unknown fixture mode", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/__test/steam-privacy", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Query().Get("mode") {
		case "public":
			steamPrivacy.Store(0)
		case "private":
			steamPrivacy.Store(1)
		case "ambiguous":
			steamPrivacy.Store(2)
		default:
			http.Error(w, "unknown fixture mode", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/__test/wishlist-mode", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Query().Get("mode") {
		case "public":
			wishlistMode.Store(0)
		case "empty":
			wishlistMode.Store(1)
		case "unavailable":
			wishlistMode.Store(2)
		case "failed":
			wishlistMode.Store(3)
		default:
			http.Error(w, "unknown fixture mode", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/__test/catalog-mode", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Query().Get("mode") {
		case "healthy":
			catalogMode.Store(0)
		case "unavailable":
			catalogMode.Store(1)
		case "malformed":
			catalogMode.Store(2)
		default:
			http.Error(w, "unknown fixture mode", http.StatusBadRequest)
			return
		}
		catalogRequests.Store(0)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/ISteamUser/GetPlayerSummaries/v2/", func(w http.ResponseWriter, r *http.Request) {
		visibility := 3
		if steamPrivacy.Load() == 1 {
			visibility = 1
		}
		writeJSON(w, map[string]any{"response": map[string]any{"players": []any{map[string]any{
			"steamid": r.URL.Query().Get("steamids"), "communityvisibilitystate": visibility,
			"personaname": "Fixture Player", "avatarfull": "https://avatars.cloudflare.steamstatic.com/fixture-avatar.jpg",
		}}}})
	})
	mux.HandleFunc("/IPlayerService/GetOwnedGames/v1/", func(w http.ResponseWriter, _ *http.Request) {
		if steamPrivacy.Load() == 2 {
			writeJSON(w, map[string]any{"response": map[string]any{"game_count": 0, "games": []any{}}})
			return
		}
		writeJSON(w, map[string]any{"response": map[string]any{"game_count": 2, "games": []any{
			map[string]any{"appid": 10, "name": "Fixture Short Story", "playtime_forever": 90, "playtime_2weeks": 0, "rtime_last_played": 1767225600},
			map[string]any{"appid": 11, "name": "Fixture Endless Realm", "playtime_forever": 12000, "playtime_2weeks": 60, "rtime_last_played": 1792195200},
		}}})
	})
	mux.HandleFunc("/IPlayerService/GetRecentlyPlayedGames/v1/", jsonHandler(map[string]any{"response": map[string]any{"total_count": 1, "games": []any{map[string]any{"appid": 11, "name": "Fixture Endless Realm", "playtime_forever": 12000, "playtime_2weeks": 60, "rtime_last_played": 1792195200}}}}))
	mux.HandleFunc("/IWishlistService/GetWishlist/v1/", func(w http.ResponseWriter, _ *http.Request) {
		switch wishlistMode.Load() {
		case 1:
			w.Header().Set("X-Eresult", "1")
			writeJSON(w, map[string]any{"response": map[string]any{"items": []any{}}})
		case 2:
			w.Header().Set("X-Eresult", "15")
			writeJSON(w, map[string]any{"response": map[string]any{}})
		case 3:
			http.Error(w, "fixture request failure", http.StatusBadRequest)
		default:
			w.Header().Set("X-Eresult", "1")
			items := make([]any, 0, 8)
			for id := int64(300001); id <= 300008; id++ {
				items = append(items, map[string]any{"appid": id, "priority": id - 300000, "date_added": 1792195200})
			}
			writeJSON(w, map[string]any{"response": map[string]any{"items": items}})
		}
	})
	mux.HandleFunc("/ISteamUserStats/GetPlayerAchievements/v1/", func(w http.ResponseWriter, r *http.Request) {
		appID, _ := strconv.ParseInt(r.URL.Query().Get("appid"), 10, 64)
		if appID == 11 {
			achieved, unlockTime := 0, 0
			if achievementMode.Load() == 3 {
				achieved, unlockTime = 1, 1768000000
			}
			writeJSON(w, map[string]any{"playerstats": map[string]any{"gameName": "Fixture Endless Realm", "success": true, "achievements": []any{
				map[string]any{"apiname": "VETERAN", "name": "Veteran", "description": "A long road travelled.", "achieved": achieved, "unlocktime": unlockTime},
			}}})
			return
		}
		writeJSON(w, map[string]any{"playerstats": map[string]any{"gameName": "Fixture Short Story", "success": true, "achievements": []any{
			map[string]any{"apiname": "ENDING", "name": "", "description": "", "achieved": 1, "unlocktime": 1767300000},
			map[string]any{"apiname": "MYSTERY", "name": "A Quiet Door", "description": "Discover what waits beyond.", "achieved": 0, "unlocktime": 0},
			map[string]any{"apiname": "CHAPTER", "name": "Chapter Complete", "description": "Complete the game chapter.", "achieved": 0, "unlocktime": 0},
			map[string]any{"apiname": "DLC", "name": "DLC Finale", "description": "Reach an ending in the expansion.", "achieved": 0, "unlocktime": 0},
			map[string]any{"apiname": "NG_PLUS", "name": "New Game Plus", "description": "Finish the game in NG+.", "achieved": 0, "unlocktime": 0},
			map[string]any{"apiname": "DIFFICULTY", "name": "Hard Mode", "description": "Finish the game on the highest difficulty.", "achieved": 0, "unlocktime": 0},
			map[string]any{"apiname": "SPEEDRUN", "name": "Fast Finish", "description": "Finish the game as a speedrun.", "achieved": 0, "unlocktime": 0},
			map[string]any{"apiname": "COLLECTION", "name": "Ending Collection", "description": "Reach all endings.", "achieved": 0, "unlocktime": 0},
		}}})
	})
	mux.HandleFunc("/ISteamUserStats/GetSchemaForGame/v2/", func(w http.ResponseWriter, r *http.Request) {
		appID, _ := strconv.ParseInt(r.URL.Query().Get("appid"), 10, 64)
		if appID != 10 {
			writeJSON(w, map[string]any{"game": map[string]any{"availableGameStats": map[string]any{"achievements": []any{}}}})
			return
		}
		definitions := []any{
			map[string]any{"name": "ENDING", "displayName": "Home Again", "description": "Reach an ending.", "defaultvalue": 0, "hidden": 0, "icon": "https://example.invalid/ending.jpg", "icongray": "https://example.invalid/ending-gray.jpg"},
			map[string]any{"name": "MYSTERY", "displayName": "A Quiet Door", "description": "Discover what waits beyond.", "defaultvalue": 0, "hidden": 0, "icon": "https://example.invalid/mystery.jpg", "icongray": "https://example.invalid/mystery-gray.jpg"},
		}
		writeJSON(w, map[string]any{"game": map[string]any{"availableGameStats": map[string]any{"achievements": definitions}}})
	})
	mux.HandleFunc("/IStoreService/GetAppList/v1/", func(w http.ResponseWriter, r *http.Request) {
		switch catalogMode.Load() {
		case 1:
			http.Error(w, "fixture catalog unavailable", http.StatusServiceUnavailable)
			return
		case 2:
			_, _ = w.Write([]byte(`{"response":{"apps":`))
			return
		}
		var input struct {
			IncludeGames bool   `json:"include_games"`
			LastAppID    uint32 `json:"last_appid"`
			MaxResults   int    `json:"max_results"`
		}
		if json.Unmarshal([]byte(r.URL.Query().Get("input_json")), &input) != nil || !input.IncludeGames || input.MaxResults < 1 || input.MaxResults > 200 {
			http.Error(w, "invalid input_json", http.StatusBadRequest)
			return
		}
		catalogRequests.Add(1)
		if input.LastAppID > 0 {
			http.Error(w, "fixture later page unavailable", http.StatusServiceUnavailable)
			return
		}
		modified := uint32(time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC).Unix())
		apps := []any{
			map[string]any{"appid": 424242, "name": "Fixture Discovered Co-op", "last_modified": modified},
			map[string]any{"appid": 424243, "name": "Fixture DLC", "last_modified": modified},
			map[string]any{"appid": 424244, "name": "Fixture Released", "last_modified": modified},
			map[string]any{"appid": 424245, "name": "Fixture Software", "last_modified": modified},
			map[string]any{"appid": 424246, "name": "Fixture Far Future", "last_modified": modified},
		}
		writeJSON(w, map[string]any{"response": map[string]any{"apps": apps, "last_appid": 424246, "have_more_results": true, "success": true}})
	})
	mux.HandleFunc("/api/appdetails", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.URL.Query().Get("appids"), 10, 64)
		date := "Dec 12, 2026"
		if id%3 == 0 {
			date = "December 2026"
		}
		gameType, comingSoon := "game", true
		categories := []any{map[string]any{"id": 2, "description": "Single-player"}}
		name := fmt.Sprintf("Fixture Release %d", id)
		switch id {
		case 10:
			name, date, comingSoon = "Fixture Short Story", "Jan 1, 2020", false
		case 11:
			name, date, comingSoon = "Fixture Endless Realm", "Jan 1, 2021", false
		case 424242:
			name, date = "Fixture Discovered Co-op", "Nov 15, 2026"
			categories = []any{map[string]any{"id": 9, "description": "Co-op"}}
		case 424243:
			gameType = "dlc"
		case 424244:
			comingSoon, date = false, "Oct 1, 2026"
		case 424245:
			gameType = "software"
		case 424246:
			date = "Dec 12, 2030"
		case 300001, 300002:
			categories = []any{map[string]any{"id": 9, "description": "Co-op"}}
		case 300003:
			categories = []any{map[string]any{"id": 1, "description": "Multi-player"}}
		case 300008:
			categories = []any{map[string]any{"id": 999, "description": "Unknown fixture mode"}}
		}
		payload := map[string]any{strconv.FormatInt(id, 10): map[string]any{"success": true, "data": map[string]any{"type": gameType, "name": name, "steam_appid": id, "required_age": 0, "is_free": false, "short_description": "A cooperative exploration game.", "header_image": "https://example.invalid/game.jpg", "developers": []string{"Fixture Studio"}, "publishers": []string{"Fixture Publisher"}, "platforms": map[string]bool{"windows": true, "mac": id%3 == 0, "linux": id%2 == 0}, "categories": categories, "genres": []any{map[string]any{"id": "1", "description": "Action"}}, "release_date": map[string]any{"coming_soon": comingSoon, "date": date}}}}
		writeJSON(w, payload)
	})
	mux.HandleFunc("/saleaction/ajaxgetdeckappcompatibilityreport", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.URL.Query().Get("nAppID"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "invalid fixture AppID", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"success": 1, "results": map[string]any{"resolved_category": (id / 10) % 4}})
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, _ *http.Request) {
		vector := make([]float64, 1536)
		vector[0] = 1
		writeJSON(w, map[string]any{"data": []any{map[string]any{"index": 0, "embedding": vector}}})
	})
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		key := evidenceKey(body)
		input := structuredInput(body)
		name := ""
		if text, ok := body["text"].(map[string]any); ok {
			if format, ok := text["format"].(map[string]any); ok {
				name, _ = format["name"].(string)
			}
		}
		var out any
		switch name {
		case "game_features_v1":
			out = map[string]any{"themes": []string{"exploration"}, "mechanics": []string{"cooperation"}, "modes": []string{"single-player"}, "tone": []string{"hopeful"}, "evidence_keys": []string{key}}
		case "profile_summary_v3":
			out = map[string]any{"summary": "Fixture Short Story — сильный исторический сигнал: игра пройдена, хотя времени в ней немного и недавней активности нет. Fixture Endless Realm отражает большой накопленный опыт и отдельную активность за последние две недели. Эти признаки учитываются независимо: прохождение короткой игры не слабее одних только часов в долгой игре. Данных пока немного, поэтому новые реакции помогут точнее определить темы и темп.", "preferences": []string{"Завершённые сюжетные игры и исследование"}, "avoidances": []string{}, "uncertainty": "Историческое прохождение не означает недавний интерес.", "evidence_keys": []string{key}}
		case "recommendation-explanation-v2":
			switch explanationMode.Load() {
			case 1:
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			case 2:
				out = map[string]any{"text": "Подтверждённые игровые сигналы совпадают.", "facts": []string{"Есть конкретное совпадение"}, "inferences": []string{}, "uncertainty": "", "evidence_keys": []string{"missing-evidence-key"}}
			case 3:
				out = map[string]any{"text": "Игра подходит из-за positiveSimilarity и deterministic score 99.", "facts": []string{"Есть конкретное совпадение"}, "inferences": []string{}, "uncertainty": "", "evidence_keys": []string{key}}
			case 4:
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			default:
				out = map[string]any{"text": "Игра продолжает подтверждённый интерес к знакомым темам и механикам.", "facts": []string{"Есть конкретное совпадение"}, "inferences": []string{}, "uncertainty": "", "evidence_keys": []string{key}}
			}
		case "feedback_classification_v2":
			text, _ := input["text"].(string)
			if strings.Contains(strings.ToLower(text), "кооп") || strings.Contains(strings.ToLower(text), "co-op") {
				out = map[string]any{"taxonomy": "multiplayer", "polarity": "negative", "confidence": 0.95, "actionable": true, "preference": map[string]any{"dimension": "mode", "target": "co-op", "direction": "avoid"}, "evidence_keys": []string{key}}
			} else {
				out = map[string]any{"taxonomy": "genre", "polarity": "positive", "confidence": 0.95, "actionable": true, "preference": nil, "evidence_keys": []string{key}}
			}
		case "achievement_semantics_v1":
			if achievementMode.Load() == 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			achievements, _ := input["achievements"].([]any)
			results := make([]any, 0, len(achievements))
			for _, raw := range achievements {
				item, _ := raw.(map[string]any)
				evidenceKey, _ := item["evidence_key"].(string)
				apiName, _ := item["api_name"].(string)
				class := fixtureAchievementClass(apiName)
				results = append(results, map[string]any{"evidence_key": evidenceKey, "class": class})
			}
			if achievementMode.Load() == 1 {
				results = []any{map[string]any{"evidence_key": "not-a-real-key", "class": "invalid-private-class"}}
			}
			out = map[string]any{"results": results}
		default:
			out = map[string]any{"error": "unknown structured output fixture"}
		}
		encoded, _ := json.Marshal(out)
		writeJSON(w, map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": string(encoded)}}}}})
	})
	addr := os.Getenv("E2E_FAKE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:18090"
	}
	if err := http.ListenAndServe(addr, mux); err != nil {
		panic(err)
	}
}

func fixtureAchievementClass(apiName string) string {
	switch apiName {
	case "ENDING":
		return "base_game_ending"
	case "DLC":
		return "dlc_ending_or_completion"
	case "COLLECTION":
		return "meta_ending_collection"
	case "CHAPTER":
		return "partial_progress"
	case "DIFFICULTY", "SPEEDRUN", "NG_PLUS":
		return "optional_challenge"
	case "MYSTERY", "VETERAN":
		return "other"
	default:
		return "unknown"
	}
}

func structuredInput(body map[string]any) map[string]any {
	raw, _ := body["input"].(string)
	var input map[string]any
	_ = json.Unmarshal([]byte(raw), &input)
	return input
}

func jsonHandler(value any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, value) }
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func evidenceKey(body map[string]any) string {
	raw, _ := body["input"].(string)
	var input map[string]any
	if json.Unmarshal([]byte(raw), &input) != nil {
		return "title"
	}
	if evidence, ok := input["evidence"].([]any); ok && len(evidence) > 0 {
		if item, ok := evidence[0].(map[string]any); ok {
			if key, ok := item["key"].(string); ok && key != "" {
				return key
			}
		}
	}
	if key, ok := input["evidence_key"].(string); ok && key != "" {
		return key
	}
	return "title"
}
