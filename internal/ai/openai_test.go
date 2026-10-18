package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestExtractGameFeaturesUsesStrictResponsesSchema(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("authorization was not set")
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"themes\":[\"exploration\"],\"mechanics\":[\"platforming\"],\"modes\":[\"single-player\"],\"tone\":[\"playful\"],\"evidence_keys\":[\"ev-title\"]}"}]}]}`))
	}))
	defer server.Close()

	client := newTestClient(t, server)
	features, err := client.ExtractGameFeatures(context.Background(), GameEvidence{
		EvidenceKey: "game-opaque", Name: "Portal 2",
		Evidence: []EvidenceItem{
			{Key: " ev-title ", Value: "Portal 2"},
			{Key: "ev-description", Value: "schema-value-canary"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(features.Themes) != 1 || features.Themes[0] != "exploration" {
		t.Fatalf("features = %#v", features)
	}
	text := request["text"].(map[string]any)
	format := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["strict"] != true {
		t.Fatalf("format = %#v", format)
	}
	if format["name"] != "game_features_v1" || format["schema"] == nil {
		t.Fatalf("format = %#v", format)
	}
	schema := format["schema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	evidenceKeys := properties["evidence_keys"].(map[string]any)
	items := evidenceKeys["items"].(map[string]any)
	if got := items["enum"]; !reflect.DeepEqual(got, []any{"ev-description", "ev-title"}) {
		t.Fatalf("evidence key enum = %#v", got)
	}
	if evidenceKeys["minItems"] != float64(1) || evidenceKeys["maxItems"] != float64(50) {
		t.Fatalf("evidence key constraints = %#v", evidenceKeys)
	}
	if _, unsupported := evidenceKeys["uniqueItems"]; unsupported {
		t.Fatalf("evidence key schema contains unsupported uniqueItems: %#v", evidenceKeys)
	}
	encodedSchema, _ := json.Marshal(schema)
	if strings.Contains(string(encodedSchema), "schema-value-canary") || strings.Contains(string(encodedSchema), "Portal 2") {
		t.Fatalf("evidence value leaked into schema: %s", encodedSchema)
	}
	encoded, _ := json.Marshal(request["input"])
	if strings.Contains(string(encoded), "steam_id") || strings.Contains(string(encoded), "api_key") {
		t.Fatalf("sensitive field leaked: %s", encoded)
	}
}

func TestClassifiedOutputErrorsExposeOnlySafeReasons(t *testing.T) {
	tests := []struct {
		name, want string
		err        error
	}{
		{name: "invalid evidence", err: fmt.Errorf("request context: %w", ErrInvalidEvidence), want: "invalid_evidence"},
		{name: "invalid output", err: fmt.Errorf("request context: %w", ErrInvalidOutput), want: "invalid_output"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var classified interface{ SafeReason() string }
			if !errors.As(test.err, &classified) {
				t.Fatalf("error %v does not expose SafeReason", test.err)
			}
			if got := classified.SafeReason(); got != test.want {
				t.Fatalf("SafeReason() = %q, want %q", got, test.want)
			}
			if strings.Contains(classified.SafeReason(), "request context") {
				t.Fatalf("SafeReason leaked error context: %q", classified.SafeReason())
			}
		})
	}
}

func TestStructuredOutputRejectsUnknownEvidenceKey(t *testing.T) {
	server := responseServer(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"themes\":[\"exploration\"],\"mechanics\":[],\"modes\":[],\"tone\":[],\"evidence_keys\":[\"made-up\"]}"}]}]}`)
	defer server.Close()
	client := newTestClient(t, server)
	_, err := client.ExtractGameFeatures(context.Background(), GameEvidence{EvidenceKey: "g", Name: "Portal", Evidence: []EvidenceItem{{Key: "known", Value: "Portal"}}})
	if !errors.Is(err, ErrInvalidEvidence) {
		t.Fatalf("error = %v", err)
	}
}

func TestStructuredOutputRejectsRefusalIncompleteAndUnknownFields(t *testing.T) {
	tests := []struct {
		name, body string
		target     error
	}{
		{"refusal", `{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]}]}`, ErrRefusal},
		{"incomplete", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`, ErrIncomplete},
		{"unknown field", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"themes\":[],\"mechanics\":[],\"modes\":[],\"tone\":[],\"evidence_keys\":[],\"extra\":true}"}]}]}`, ErrInvalidOutput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := responseServer(tt.body)
			defer server.Close()
			client := newTestClient(t, server)
			_, err := client.ExtractGameFeatures(context.Background(), GameEvidence{EvidenceKey: "g", Name: "Portal", Evidence: []EvidenceItem{{Key: "known", Value: "Portal"}}})
			if !errors.Is(err, tt.target) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestEmbedRequestsAndRequiresExactly1536Dimensions(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		values := make([]float32, EmbeddingDimensions)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": values}}})
	}))
	defer server.Close()
	client := newTestClient(t, server)
	vectors, err := client.Embed(context.Background(), []string{"portal puzzles"})
	if err != nil {
		t.Fatal(err)
	}
	if request["dimensions"] != float64(EmbeddingDimensions) {
		t.Fatalf("dimensions = %v", request["dimensions"])
	}
	if len(vectors) != 1 || len(vectors[0]) != EmbeddingDimensions {
		t.Fatalf("dimensions = %d", len(vectors[0]))
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1]}]}`))
	}))
	defer bad.Close()
	badClient := newTestClient(t, bad)
	if _, err := badClient.Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrInvalidEmbedding) {
		t.Fatalf("error = %v", err)
	}
}

func TestProfileExplanationAndFeedbackStructuredOperations(t *testing.T) {
	profileText := strings.Repeat("я", 400)
	profileServer := responseServer(fmt.Sprintf(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"summary\":\"%s\",\"preferences\":[\"puzzles\"],\"avoidances\":[],\"uncertainty\":\"small sample\",\"evidence_keys\":[\"signal-1\"]}"}]}]}`, profileText))
	profileClient := newTestClient(t, profileServer)
	profile, err := profileClient.BuildProfileSummary(context.Background(), ProfileEvidence{EvidenceKey: "profile", Evidence: []EvidenceItem{{Key: "signal-1", Value: "Portal; high playtime"}}})
	profileServer.Close()
	if err != nil || profile.Summary == "" {
		t.Fatalf("profile=%#v error=%v", profile, err)
	}

	explanationServer := responseServer(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"text\":\"Matches puzzles\",\"facts\":[\"puzzle tag\"],\"inferences\":[\"likely fit\"],\"uncertainty\":\"unreleased\",\"evidence_keys\":[\"factor-1\"]}"}]}]}`)
	explanationClient := newTestClient(t, explanationServer)
	explanation, err := explanationClient.ExplainRecommendation(context.Background(), RecommendationEvidence{EvidenceKey: "recommendation", GameName: "Portal 3", Evidence: []EvidenceItem{{Key: "factor-1", Value: "tag affinity 0.9"}}})
	explanationServer.Close()
	if err != nil || !explanation.Generated {
		t.Fatalf("explanation=%#v error=%v", explanation, err)
	}

	feedbackServer := responseServer(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"taxonomy\":\"other\",\"polarity\":\"neutral\",\"confidence\":0.2,\"actionable\":true,\"preference\":null,\"evidence_keys\":[\"feedback-1\"]}"}]}]}`)
	feedbackClient := newTestClient(t, feedbackServer)
	classification, err := feedbackClient.ClassifyFeedback(context.Background(), FeedbackText{EvidenceKey: "feedback-1", Text: "Maybe later"})
	feedbackServer.Close()
	if err != nil || classification.Actionable {
		t.Fatalf("classification=%#v error=%v", classification, err)
	}
}

func TestExplanationUsesV2SchemaWithOnlySuppliedCitationKeys(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"text\":\"Подходит по атмосфере.\",\"facts\":[],\"inferences\":[],\"uncertainty\":\"\",\"evidence_keys\":[\"feature-overlap-1\"]}"}]}]}`))
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.ExplainRecommendation(context.Background(), RecommendationEvidence{
		EvidenceKey: "recommendation-evidence",
		GameName:    "Игра",
		Evidence: []EvidenceItem{
			{Key: "similar-game-1", Value: "Похожа на SOMA."},
			{Key: "feature-overlap-1", Value: "Совпадают темы: horror."},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	format := request["text"].(map[string]any)["format"].(map[string]any)
	if format["name"] != "recommendation-explanation-v2" {
		t.Fatalf("schema name = %#v", format["name"])
	}
	schema := format["schema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	keys := properties["evidence_keys"].(map[string]any)
	items := keys["items"].(map[string]any)
	if got := items["enum"]; !reflect.DeepEqual(got, []any{"feature-overlap-1", "similar-game-1"}) {
		t.Fatalf("citation enum = %#v", got)
	}
	if keys["minItems"] != float64(1) || keys["maxItems"] != float64(2) {
		t.Fatalf("citation constraints = %#v", keys)
	}
	encoded, _ := json.Marshal(schema)
	if strings.Contains(string(encoded), "SOMA") || strings.Contains(string(encoded), "horror") {
		t.Fatalf("evidence values leaked into schema: %s", encoded)
	}
}

func TestProfileSummaryUsesUnicodeLengthBoundaries(t *testing.T) {
	allowed := map[string]struct{}{"signal-1": {}}
	for _, test := range []struct {
		name    string
		length  int
		wantErr bool
	}{
		{name: "below lower bound", length: 319, wantErr: true},
		{name: "lower bound", length: 320},
		{name: "upper bound", length: 720},
		{name: "above upper bound", length: 721, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := ProfileSummary{Summary: strings.Repeat("я", test.length), Preferences: []string{}, Avoidances: []string{}, EvidenceKeys: []string{"signal-1"}}
			_, err := normalizeAndValidateProfileOutput(output, allowed)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, test.wantErr)
			}
		})
	}
}

func TestProfileSummaryNormalizesOptionalArraysWithoutFailing(t *testing.T) {
	output, err := normalizeAndValidateProfileOutput(ProfileSummary{
		Summary: strings.Repeat("я", 400),
		Preferences: []string{
			"  головоломки  ",
			"",
			"головоломки",
			strings.Repeat("я", 144),
			strings.Repeat("я", 145),
		},
		Avoidances:   nil,
		Uncertainty:  "  мало данных  ",
		EvidenceKeys: []string{"signal-1"},
	}, map[string]struct{}{"signal-1": {}})
	if err != nil {
		t.Fatal(err)
	}
	wantPreferences := []string{"головоломки", strings.Repeat("я", 144)}
	if !reflect.DeepEqual(output.Preferences, wantPreferences) {
		t.Fatalf("preferences = %#v, want %#v", output.Preferences, wantPreferences)
	}
	if output.Avoidances == nil || len(output.Avoidances) != 0 {
		t.Fatalf("avoidances = %#v", output.Avoidances)
	}
	if output.Uncertainty != "мало данных" {
		t.Fatalf("uncertainty = %q", output.Uncertainty)
	}
}

func TestProfileSummaryStillRejectsUnknownEvidence(t *testing.T) {
	for _, test := range []struct {
		name string
		keys []string
	}{
		{name: "unknown", keys: []string{"made-up"}},
		{name: "duplicate", keys: []string{"signal-1", "signal-1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := normalizeAndValidateProfileOutput(ProfileSummary{
				Summary:      strings.Repeat("я", 400),
				Preferences:  []string{},
				Avoidances:   []string{},
				EvidenceKeys: test.keys,
			}, map[string]struct{}{"signal-1": {}})
			if !errors.Is(err, ErrInvalidEvidence) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestEmbedRejectsEmptyInputWithoutRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	client := newTestClient(t, server)
	if _, err := client.Embed(context.Background(), []string{" "}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v", err)
	}
	if called {
		t.Fatal("empty input made a request")
	}
}

func TestMissingAPIKeyReturnsTypedDisabledCapability(t *testing.T) {
	client, err := NewOpenAI(OpenAIConfig{Model: "gpt-6-luna", EmbeddingModel: "text-embedding-3-small"})
	if err != nil {
		t.Fatal(err)
	}
	_, callErr := client.Embed(context.Background(), []string{"x"})
	if !errors.Is(callErr, ErrCapabilityDisabled) {
		t.Fatalf("error = %v", callErr)
	}
	if fallback := DeterministicExplanation([]string{"wishlist", "genre affinity"}); fallback.Text == "" || fallback.Generated {
		t.Fatalf("fallback = %#v", fallback)
	}
}

func TestFeedbackLengthAndContentHashVersions(t *testing.T) {
	client, err := NewOpenAI(OpenAIConfig{APIKey: "x", Model: "gpt-6-luna", EmbeddingModel: "text-embedding-3-small", HTTPClient: http.DefaultClient})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ClassifyFeedback(context.Background(), FeedbackText{EvidenceKey: "feedback-1", Text: strings.Repeat("x", 501)}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v", err)
	}
	a, err := ContentHash("ai.features", "schema-v1", "prompt-v1", "gpt-6-luna", GameEvidence{Name: "Portal"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ContentHash("ai.features", "schema-v2", "prompt-v1", "gpt-6-luna", GameEvidence{Name: "Portal"})
	if a == b {
		t.Fatal("schema version must affect content hash")
	}
}

func TestSensitiveEvidenceKeysAreRejectedBeforeRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	client := newTestClient(t, server)
	_, err := client.ExtractGameFeatures(context.Background(), GameEvidence{EvidenceKey: "game", Name: "Portal", Evidence: []EvidenceItem{{Key: "abcdef0123456789abcdef0123456789", Value: "title"}}})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v", err)
	}
	if called {
		t.Fatal("sensitive evidence key was sent")
	}
}

func TestSecretsAreRedactedFromModelInputAndHTTPErrorsDoNotEchoBodies(t *testing.T) {
	var input string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		input, _ = request["input"].(string)
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"themes\":[\"puzzle\"],\"mechanics\":[],\"modes\":[],\"tone\":[],\"evidence_keys\":[\"description\"]}"}]}]}`))
	}))
	client := newTestClient(t, server)
	_, err := client.ExtractGameFeatures(context.Background(), GameEvidence{EvidenceKey: "game", Name: "Portal", Evidence: []EvidenceItem{{Key: "description", Value: "contact dev@example.com with sk-abcdefghijk or Steam 76561198000000000"}}})
	server.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(input, "dev@example.com") || strings.Contains(input, "sk-abcdefghijk") || strings.Contains(input, "76561198000000000") {
		t.Fatalf("secret leaked in %s", input)
	}
	if !strings.Contains(input, "[redacted]") {
		t.Fatalf("redaction missing in %s", input)
	}

	errorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"sk-super-secret"}`))
	}))
	defer errorServer.Close()
	errorClient := newTestClient(t, errorServer)
	_, err = errorClient.Embed(context.Background(), []string{"safe"})
	if !errors.Is(err, ErrHTTP) || strings.Contains(err.Error(), "sk-super-secret") {
		t.Fatalf("error = %v", err)
	}
}

func TestHTTPErrorExposesSafeRetryMetadata(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		retryAfter string
		retryable  bool
		wantDelay  time.Duration
	}{
		{name: "request timeout", status: http.StatusRequestTimeout, retryable: true},
		{name: "rate limited", status: http.StatusTooManyRequests, retryAfter: "7", retryable: true, wantDelay: 7 * time.Second},
		{name: "service unavailable", status: http.StatusServiceUnavailable, retryAfter: "11", retryable: true, wantDelay: 11 * time.Second},
		{name: "server failure", status: http.StatusInternalServerError, retryable: true},
		{name: "bad request", status: http.StatusBadRequest},
		{name: "unauthorized", status: http.StatusUnauthorized},
		{name: "forbidden", status: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.retryAfter != "" {
					w.Header().Set("Retry-After", test.retryAfter)
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(`{"error":"sk-body-canary"}`))
			}))
			defer server.Close()

			client := newTestClient(t, server)
			_, err := client.Embed(context.Background(), []string{"safe"})
			var requestErr *RequestError
			if !errors.Is(err, ErrHTTP) || !errors.As(err, &requestErr) {
				t.Fatalf("error = %T %v", err, err)
			}
			if requestErr.StatusCode != test.status || requestErr.Retryable() != test.retryable || requestErr.RetryDelay() != test.wantDelay {
				t.Fatalf("request error = %#v, retryable=%v delay=%s", requestErr, requestErr.Retryable(), requestErr.RetryDelay())
			}
			if strings.Contains(err.Error(), "sk-body-canary") {
				t.Fatalf("response body leaked in %q", err.Error())
			}
		})
	}
}

func TestHTTPErrorParsesRetryAfterDate(t *testing.T) {
	retryAt := time.Now().Add(10 * time.Second).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", retryAt.Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.Embed(context.Background(), []string{"safe"})
	var requestErr *RequestError
	if !errors.As(err, &requestErr) {
		t.Fatalf("error = %T %v", err, err)
	}
	if delay := requestErr.RetryDelay(); delay < 8*time.Second || delay > 10*time.Second {
		t.Fatalf("RetryDelay() = %s", delay)
	}
}

func TestStructuredOutputFailuresAreNonRetryable(t *testing.T) {
	for _, err := range []error{ErrInvalidOutput, ErrInvalidEvidence, ErrRefusal} {
		disposition, ok := err.(interface{ Retryable() bool })
		if !ok || disposition.Retryable() {
			t.Fatalf("error %v retry disposition = %v, %v", err, disposition, ok)
		}
	}
}

func TestTimeoutIsPropagated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()
	client := newTestClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := client.Embed(ctx, []string{"safe"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func newTestClient(t *testing.T, server *httptest.Server) *OpenAI {
	t.Helper()
	client, err := NewOpenAI(OpenAIConfig{APIKey: "test-key", Model: "gpt-6-luna", EmbeddingModel: "text-embedding-3-small", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func responseServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}
