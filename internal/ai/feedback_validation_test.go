package ai

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"next-game-crush/internal/preferences"
)

func TestFeedbackPreferenceValidation(t *testing.T) {
	valid := FeedbackClassification{Taxonomy: "multiplayer", Polarity: "negative", Confidence: .92, Actionable: true, Preference: &preferences.Target{Dimension: "mode", Target: preferences.ModeCoop, Direction: "avoid"}, EvidenceKeys: []string{"feedback-text"}}
	for _, test := range []struct {
		name    string
		mutate  func(*FeedbackClassification)
		invalid bool
	}{
		{"valid", func(*FeedbackClassification) {}, false},
		{"threshold", func(v *FeedbackClassification) { v.Confidence = .70 }, false},
		{"below threshold", func(v *FeedbackClassification) { v.Confidence = .699 }, true},
		{"null", func(v *FeedbackClassification) { v.Preference = nil; v.Actionable = false; v.Confidence = .3 }, false},
		{"dimension", func(v *FeedbackClassification) { v.Preference.Dimension = "genre" }, true},
		{"alias", func(v *FeedbackClassification) { v.Preference.Target = "coop" }, true},
		{"direction", func(v *FeedbackClassification) { v.Preference.Direction = "prefer" }, true},
		{"positive", func(v *FeedbackClassification) { v.Polarity = "positive" }, true},
		{"inactive", func(v *FeedbackClassification) { v.Actionable = false }, true},
		{"single-player coop", func(v *FeedbackClassification) { v.Taxonomy = "single-player" }, true},
		{"multiplayer single", func(v *FeedbackClassification) { v.Preference.Target = preferences.ModeSinglePlayer }, true},
		{"genre coop", func(v *FeedbackClassification) { v.Taxonomy = "genre" }, true},
		{"citation", func(v *FeedbackClassification) { v.EvidenceKeys = []string{"private-value"} }, true},
		{"duplicate", func(v *FeedbackClassification) { v.EvidenceKeys = []string{"feedback-text", "feedback-text"} }, true},
		{"NaN", func(v *FeedbackClassification) { v.Confidence = math.NaN() }, true},
		{"Inf", func(v *FeedbackClassification) { v.Confidence = math.Inf(1) }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := valid
			p := *valid.Preference
			v.Preference = &p
			test.mutate(&v)
			err := ValidateFeedbackClassification(v, "feedback-text")
			if (err != nil) != test.invalid {
				t.Fatalf("invalid=%v error=%v", test.invalid, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-value") {
				t.Fatal("error leaks output")
			}
		})
	}
}

func TestFeedbackPreferenceStructuredSchema(t *testing.T) {
	s := feedbackSchema()
	props := s["properties"].(map[string]any)
	if s["additionalProperties"] != false {
		t.Fatal("root must be strict")
	}
	if !reflect.DeepEqual(s["required"], []string{"taxonomy", "polarity", "confidence", "actionable", "preference", "evidence_keys"}) {
		t.Fatal("all fields required")
	}
	p := props["preference"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)
	if p["additionalProperties"] != false {
		t.Fatal("preference must be strict")
	}
	if !reflect.DeepEqual(p["properties"].(map[string]any)["target"].(map[string]any)["enum"], preferences.Modes()) {
		t.Fatal("canonical modes required")
	}
	if !reflect.DeepEqual(p["required"], []string{"dimension", "target", "direction"}) {
		t.Fatal("all nested fields required")
	}
	if !reflect.DeepEqual(p["properties"].(map[string]any)["dimension"].(map[string]any)["enum"], []string{"mode"}) || !reflect.DeepEqual(p["properties"].(map[string]any)["direction"].(map[string]any)["enum"], []string{"avoid"}) {
		t.Fatal("dimension and direction must be fixed enums")
	}
}

func TestFeedbackPreferenceHTTPAcceptsOnlyCanonicalOutput(t *testing.T) {
	for _, test := range []struct {
		name, target string
		wantErr      bool
	}{{"canonical", "co-op", false}, {"alias", "coop", true}} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"taxonomy":"multiplayer","polarity":"negative","confidence":0.92,"actionable":true,"preference":{"dimension":"mode","target":"` + test.target + `","direction":"avoid"},"evidence_keys":["feedback-text"]}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				format := request["text"].(map[string]any)["format"].(map[string]any)
				if format["name"] != "feedback_classification_v2" || format["strict"] != true {
					t.Error("feedback format must be strict v2")
				}
				encoded, _ := json.Marshal(format["schema"])
				if strings.Contains(string(encoded), "не люблю коопы") {
					t.Error("feedback text leaked into schema")
				}
				if request["input"] != `{"evidence_key":"feedback-text","text":"не люблю коопы / coop"}` {
					t.Error("model input must contain only feedback text and opaque key")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": body}}}}})
			}))
			defer server.Close()
			output, err := newTestClient(t, server).ClassifyFeedback(context.Background(), FeedbackText{EvidenceKey: "feedback-text", Text: "не люблю коопы / coop"})
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v", err)
			}
			if test.wantErr {
				if output.Preference != nil || output.Actionable {
					t.Fatal("invalid output usable")
				}
			} else if output.Preference == nil || output.Preference.Target != preferences.ModeCoop {
				t.Fatal("canonical preference missing")
			}
		})
	}
	for _, test := range []struct {
		name, body string
		want       error
	}{{"refusal", `{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"private-value"}]}]}`, ErrRefusal}, {"incomplete", `{"status":"incomplete","output":[]}`, ErrIncomplete}} {
		t.Run(test.name, func(t *testing.T) {
			server := responseServer(test.body)
			defer server.Close()
			output, err := newTestClient(t, server).ClassifyFeedback(context.Background(), FeedbackText{EvidenceKey: "feedback-text", Text: "coop"})
			if !errors.Is(err, test.want) || output.Preference != nil || output.Actionable {
				t.Fatalf("error=%v", err)
			}
			if strings.Contains(err.Error(), "private-value") {
				t.Fatal("raw refusal leaked")
			}
		})
	}
}

func TestFeedbackPreferenceHTTPRejectsMissingAndMalformedFields(t *testing.T) {
	for _, body := range []string{
		`{"taxonomy":"multiplayer","polarity":"negative","confidence":0.92,"actionable":true,"evidence_keys":["feedback-text"]}`,
		`{"taxonomy":"multiplayer","polarity":"negative","confidence":0.92,"actionable":true,"preference":null,"extra":"private-value","evidence_keys":["feedback-text"]}`,
		`{"taxonomy":"multiplayer","polarity":"negative","confidence":0.92,"actionable":true,"preference":{"dimension":"mode","target":"co-op","direction":"avoid","extra":true},"evidence_keys":["feedback-text"]}`,
		`{broken private-value`,
	} {
		t.Run(body[:min(len(body), 20)], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				_ = json.NewDecoder(r.Body).Decode(&request)
				if request["input"] != `{"evidence_key":"feedback-text","text":"не люблю коопы / coop"}` {
					t.Error("unexpected feedback input")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": body}}}}})
			}))
			defer server.Close()
			output, err := newTestClient(t, server).ClassifyFeedback(context.Background(), FeedbackText{EvidenceKey: "feedback-text", Text: "не люблю коопы / coop"})
			if !errors.Is(err, ErrInvalidOutput) || output.Preference != nil || output.Actionable {
				t.Fatalf("unsafe result: error=%v", err)
			}
			if strings.Contains(err.Error(), "private-value") {
				t.Fatal("error leaks output")
			}
		})
	}
}
