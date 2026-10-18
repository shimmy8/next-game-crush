package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestAchievementSemanticsPromptDeclaresBoundedAuthority(t *testing.T) {
	if AchievementSemanticsPromptVersion != "achievement-semantics-v1" || AchievementSemanticsSchemaVersion != "achievement-semantics-structured-output-v1" {
		t.Fatal("achievement semantic versions must identify the strict contract")
	}
	for _, required := range []string{"only the supplied shared", "exactly one result", "opaque evidence key", "Do not return confidence", "weights", "ranking"} {
		if !strings.Contains(achievementSemanticsInstructions, required) {
			t.Fatalf("achievement semantic instructions missing %q", required)
		}
	}
}

func TestAchievementSemanticClassifierUsesOnlySharedMetadata(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"results\":[{\"evidence_key\":\"achievement-a\",\"class\":\"base_game_ending\"},{\"evidence_key\":\"achievement-b\",\"class\":\"other\"}]}"}]}]}`))
	}))
	defer server.Close()

	client := newTestClient(t, server)
	result, err := client.ClassifyAchievementSemantics(context.Background(), AchievementSemanticEvidence{
		EvidenceKey: "game-achievements",
		GameName:    "Example Game",
		Achievements: []AchievementSemanticItem{
			{EvidenceKey: "achievement-b", APIName: "ACH_OTHER", Name: "Explorer", Description: "Find every room"},
			{EvidenceKey: "achievement-a", APIName: "ACH_END", Name: "The End", Description: "Reach an ending"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 2 {
		t.Fatalf("results = %#v", result.Results)
	}

	format := request["text"].(map[string]any)["format"].(map[string]any)
	if format["name"] != "achievement_semantics_v1" || format["strict"] != true {
		t.Fatalf("format = %#v", format)
	}
	schema := format["schema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	results := properties["results"].(map[string]any)
	items := results["items"].(map[string]any)
	itemProperties := items["properties"].(map[string]any)
	if got := itemProperties["evidence_key"].(map[string]any)["enum"]; !reflect.DeepEqual(got, []any{"achievement-a", "achievement-b"}) {
		t.Fatalf("evidence enum = %#v", got)
	}
	if got := itemProperties["class"].(map[string]any)["enum"]; !reflect.DeepEqual(got, achievementSemanticClassStrings()) {
		t.Fatalf("class enum = %#v", got)
	}

	input, _ := request["input"].(string)
	for _, required := range []string{"Example Game", "game-achievements", "achievement-a", "ACH_END", "The End", "Reach an ending"} {
		if !strings.Contains(input, required) {
			t.Fatalf("input missing %q: %s", required, input)
		}
	}
	for _, forbidden := range []string{"steam_id", "user_id", "unlocked", "unlocktime", "playtime", "feedback", "profile", "weight", "raw_body", "76561198000000000", "9f4d964b-567d-4aef-b59b-c0135593b5df"} {
		if strings.Contains(strings.ToLower(input), forbidden) {
			t.Fatalf("private field %q leaked: %s", forbidden, input)
		}
	}
	if strings.Index(input, "achievement-a") > strings.Index(input, "achievement-b") {
		t.Fatalf("achievements were not sorted by opaque key: %s", input)
	}
}

func TestValidateAchievementSemanticClassification(t *testing.T) {
	classes := allAchievementSemanticClasses()
	valid := AchievementSemanticClassification{Results: make([]AchievementSemanticResult, len(classes))}
	keys := make([]string, len(classes))
	for i, class := range classes {
		keys[i] = "achievement-" + string(rune('a'+i))
		valid.Results[i] = AchievementSemanticResult{EvidenceKey: keys[i], Class: class}
	}
	if err := ValidateAchievementSemanticClassification(valid, keys); err != nil {
		t.Fatalf("valid classification rejected: %v", err)
	}

	tests := []struct {
		name   string
		value  AchievementSemanticClassification
		keys   []string
		target error
	}{
		{name: "nil results", value: AchievementSemanticClassification{}, keys: []string{"achievement-a"}, target: ErrInvalidOutput},
		{name: "missing key", value: AchievementSemanticClassification{Results: []AchievementSemanticResult{{EvidenceKey: "achievement-a", Class: AchievementSemanticOther}}}, keys: []string{"achievement-a", "achievement-b"}, target: ErrInvalidEvidence},
		{name: "duplicate key", value: AchievementSemanticClassification{Results: []AchievementSemanticResult{{EvidenceKey: "achievement-a", Class: AchievementSemanticOther}, {EvidenceKey: "achievement-a", Class: AchievementSemanticUnknown}}}, keys: []string{"achievement-a", "achievement-b"}, target: ErrInvalidEvidence},
		{name: "unknown key", value: AchievementSemanticClassification{Results: []AchievementSemanticResult{{EvidenceKey: "private-canary", Class: AchievementSemanticOther}}}, keys: []string{"achievement-a"}, target: ErrInvalidEvidence},
		{name: "unknown class", value: AchievementSemanticClassification{Results: []AchievementSemanticResult{{EvidenceKey: "achievement-a", Class: "private-class-canary"}}}, keys: []string{"achievement-a"}, target: ErrInvalidOutput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateAchievementSemanticClassification(test.value, test.keys)
			if !errors.Is(err, test.target) {
				t.Fatalf("error = %v, want %v", err, test.target)
			}
			if err != nil && (strings.Contains(err.Error(), "private-canary") || strings.Contains(err.Error(), "private-class-canary")) {
				t.Fatalf("error leaked untrusted output: %v", err)
			}
		})
	}
}

func TestAchievementSemanticClassifierRejectsMalformedOutputSafely(t *testing.T) {
	for _, test := range []struct {
		name, output string
	}{
		{name: "unknown field", output: `{"results":[{"evidence_key":"achievement-a","class":"other","private-canary":"secret"}]}`},
		{name: "trailing json", output: `{"results":[{"evidence_key":"achievement-a","class":"other"}]} {"private-canary":"secret"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, _ := json.Marshal(test.output)
			server := responseServer(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":` + string(encoded) + `}]}]}`)
			defer server.Close()
			_, err := newTestClient(t, server).ClassifyAchievementSemantics(context.Background(), AchievementSemanticEvidence{
				EvidenceKey: "game-achievements", GameName: "Example Game",
				Achievements: []AchievementSemanticItem{{EvidenceKey: "achievement-a", APIName: "ACH_A", Name: "A", Description: "Something"}},
			})
			if !errors.Is(err, ErrInvalidOutput) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "private-canary") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked output: %v", err)
			}
		})
	}
}

func TestAchievementSemanticClassifierRedactsSensitiveMetadata(t *testing.T) {
	var input string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		input, _ = request["input"].(string)
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"results\":[{\"evidence_key\":\"achievement-a\",\"class\":\"unknown\"}]}"}]}]}`))
	}))
	defer server.Close()

	_, err := newTestClient(t, server).ClassifyAchievementSemantics(context.Background(), AchievementSemanticEvidence{
		EvidenceKey: "game-achievements",
		GameName:    "Example 76561198000000000",
		Achievements: []AchievementSemanticItem{{
			EvidenceKey: "achievement-a", APIName: "ACH_A", Name: "Contact dev@example.com", Description: "token sk-abcdefghijk",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"76561198000000000", "dev@example.com", "sk-abcdefghijk"} {
		if strings.Contains(input, secret) {
			t.Fatalf("sensitive metadata %q leaked: %s", secret, input)
		}
	}
	if !strings.Contains(input, "[redacted]") {
		t.Fatalf("redaction missing: %s", input)
	}
}

func TestAchievementSemanticClassifierRejectsMalformedUTF8BeforeRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()

	_, err := newTestClient(t, server).ClassifyAchievementSemantics(context.Background(), AchievementSemanticEvidence{
		EvidenceKey: "game-achievements",
		GameName:    "Example Game",
		Achievements: []AchievementSemanticItem{{
			EvidenceKey: "achievement-a", APIName: "ACH_A", Name: string([]byte{0xff}), Description: "Something",
		}},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v", err)
	}
	if called {
		t.Fatal("malformed UTF-8 made a provider request")
	}
}

func achievementSemanticClassStrings() []any {
	classes := allAchievementSemanticClasses()
	values := make([]any, len(classes))
	for i, class := range classes {
		values[i] = string(class)
	}
	return values
}
