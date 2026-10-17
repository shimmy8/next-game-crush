// Command e2e-fakes serves deterministic Steam and OpenAI responses for the
// browser smoke test. It is never linked into the application binary.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/IPlayerService/GetOwnedGames/v1/", jsonHandler(map[string]any{"response": map[string]any{"game_count": 1, "games": []any{map[string]any{"appid": 10, "name": "Fixture Classic", "playtime_forever": 1200, "playtime_2weeks": 60, "rtime_last_played": 1792195200}}}}))
	mux.HandleFunc("/IPlayerService/GetRecentlyPlayedGames/v1/", jsonHandler(map[string]any{"response": map[string]any{"total_count": 1, "games": []any{map[string]any{"appid": 10, "name": "Fixture Classic", "playtime_forever": 1200, "playtime_2weeks": 60, "rtime_last_played": 1792195200}}}}))
	mux.HandleFunc("/IWishlistService/GetWishlist/v1/", jsonHandler(map[string]any{"response": map[string]any{"items": []any{map[string]any{"appid": 20, "priority": 1, "date_added": 1792195200}}}}))
	mux.HandleFunc("/ISteamUserStats/GetPlayerAchievements/v1/", jsonHandler(map[string]any{"playerstats": map[string]any{"gameName": "Fixture Classic", "success": true, "achievements": []any{map[string]any{"achieved": 1, "unlocktime": 1700000000}, map[string]any{"achieved": 0, "unlocktime": 0}}}}))
	mux.HandleFunc("/api/appdetails", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.URL.Query().Get("appids"), 10, 64)
		date := "Dec 12, 2026"
		if id%3 == 0 {
			date = "December 2026"
		}
		payload := map[string]any{strconv.FormatInt(id, 10): map[string]any{"success": true, "data": map[string]any{"type": "game", "name": fmt.Sprintf("Fixture Release %d", id), "steam_appid": id, "required_age": 0, "is_free": false, "short_description": "A cooperative exploration game.", "header_image": "https://example.invalid/game.jpg", "developers": []string{"Fixture Studio"}, "publishers": []string{"Fixture Publisher"}, "categories": []any{map[string]any{"id": 2, "description": "Single-player"}}, "genres": []any{map[string]any{"id": "1", "description": "Action"}}, "release_date": map[string]any{"coming_soon": true, "date": date}}}}
		writeJSON(w, payload)
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
		case "profile_summary_v1":
			out = map[string]any{"summary": "Вы любите исследование и понятные системы.", "preferences": []string{"exploration"}, "avoidances": []string{}, "uncertainty": "Fixture data is intentionally small.", "evidence_keys": []string{key}}
		case "explanation_v1":
			out = map[string]any{"text": "Похоже по сохранённым игровым сигналам.", "facts": []string{"Upcoming release"}, "inferences": []string{"Likely fit"}, "uncertainty": "Release date may change.", "evidence_keys": []string{key}}
		default:
			out = map[string]any{"taxonomy": "genre", "polarity": "positive", "confidence": 0.95, "actionable": true, "evidence_keys": []string{key}}
		}
		encoded, _ := json.Marshal(out)
		writeJSON(w, map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": string(encoded)}}}}})
	})
	if err := http.ListenAndServe("127.0.0.1:18090", mux); err != nil {
		panic(err)
	}
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
