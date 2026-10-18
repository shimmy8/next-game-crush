package ai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"next-game-crush/internal/preferences"
)

const defaultBaseURL = "https://api.openai.com"

type OpenAIConfig struct {
	APIKey         string
	Model          string
	EmbeddingModel string
	BaseURL        string
	HTTPClient     *http.Client
}

type OpenAI struct {
	apiKey, model, embeddingModel, baseURL string
	httpClient                             *http.Client
}

func NewOpenAI(config OpenAIConfig) (*OpenAI, error) {
	config.APIKey = strings.TrimSpace(config.APIKey)
	config.Model = strings.TrimSpace(config.Model)
	config.EmbeddingModel = strings.TrimSpace(config.EmbeddingModel)
	if config.Model == "" {
		config.Model = "gpt-6-luna"
	}
	if config.EmbeddingModel == "" {
		config.EmbeddingModel = "text-embedding-3-small"
	}
	if config.BaseURL == "" {
		config.BaseURL = defaultBaseURL
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	if config.HTTPClient == nil {
		config.HTTPClient = http.DefaultClient
	}
	baseURL, baseURLErr := url.Parse(config.BaseURL)
	if config.Model == "" || config.EmbeddingModel == "" || baseURLErr != nil || baseURL.Host == "" || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, fmt.Errorf("%w: models and base URL are required", ErrInvalidInput)
	}
	return &OpenAI{apiKey: config.APIKey, model: config.Model, embeddingModel: config.EmbeddingModel, baseURL: config.BaseURL, httpClient: config.HTTPClient}, nil
}

func (client *OpenAI) Disabled() bool { return client == nil || client.apiKey == "" }

func (client *OpenAI) ExtractGameFeatures(ctx context.Context, input GameEvidence) (GameFeatures, error) {
	var output GameFeatures
	allowed, normalized, err := normalizeGameEvidence(input)
	if err != nil {
		return output, err
	}
	allowedKeys := make([]string, len(normalized.Evidence))
	for i, item := range normalized.Evidence {
		allowedKeys[i] = item.Key
	}
	err = client.structured(ctx, "game_features_v1", featureInstructions, FeaturePromptVersion, gameFeaturesSchema(allowedKeys), normalized, &output)
	if err != nil {
		return GameFeatures{}, err
	}
	if err := validateFeatureOutput(output); err != nil {
		return GameFeatures{}, err
	}
	if err := validateCitations(output.EvidenceKeys, allowed); err != nil {
		return GameFeatures{}, err
	}
	return output, nil
}

func (client *OpenAI) SummarizeProfile(ctx context.Context, input ProfileEvidence) (ProfileSummary, error) {
	var output ProfileSummary
	allowed, normalized, err := normalizeProfileEvidence(input)
	if err != nil {
		return output, err
	}
	err = client.structured(ctx, "profile_summary_v3", profileInstructions, ProfilePromptVersion, profileSummarySchema(), normalized, &output)
	if err != nil {
		return ProfileSummary{}, err
	}
	output, err = normalizeAndValidateProfileOutput(output, allowed)
	if err != nil {
		return ProfileSummary{}, err
	}
	return output, nil
}

// BuildProfileSummary is an explicit alias for callers that use the product
// operation name rather than the Client interface name.
func (client *OpenAI) BuildProfileSummary(ctx context.Context, input ProfileEvidence) (ProfileSummary, error) {
	return client.SummarizeProfile(ctx, input)
}

func (client *OpenAI) ExplainRecommendation(ctx context.Context, input RecommendationEvidence) (Explanation, error) {
	var output Explanation
	allowed, normalized, err := normalizeRecommendationEvidence(input)
	if err != nil {
		return output, err
	}
	allowedKeys := make([]string, len(normalized.Evidence))
	for i, item := range normalized.Evidence {
		allowedKeys[i] = item.Key
	}
	err = client.structured(ctx, "recommendation-explanation-v2", explanationInstructions, ExplanationPromptVersion, explanationSchema(allowedKeys), normalized, &output)
	if err != nil {
		return Explanation{}, err
	}
	if err := validateExplanationOutput(output, allowed); err != nil {
		return Explanation{}, err
	}
	output.Generated = true
	return output, nil
}

func (client *OpenAI) ClassifyFeedback(ctx context.Context, input FeedbackText) (FeedbackClassification, error) {
	var output FeedbackClassification
	normalized, _, err := normalizeFeedback(input)
	if err != nil {
		return output, err
	}
	err = client.structured(ctx, "feedback_classification_v2", feedbackInstructions, FeedbackPromptVersion, feedbackSchema(), normalized, &output)
	if err != nil {
		return FeedbackClassification{}, err
	}
	if err := ValidateFeedbackClassification(output, normalized.EvidenceKey); err != nil {
		return FeedbackClassification{}, err
	}
	if output.Confidence < .7 {
		output.Actionable = false
	}
	return output, nil
}

func (client *OpenAI) structured(ctx context.Context, name, instructions, promptVersion string, schema map[string]any, input any, output any) error {
	if client.Disabled() {
		return ErrCapabilityDisabled
	}
	payload := map[string]any{
		"model":        client.model,
		"store":        false,
		"instructions": instructions + " Prompt version: " + promptVersion + ".",
		"input":        mustJSON(input),
		"text":         map[string]any{"format": map[string]any{"type": "json_schema", "name": name, "schema": schema, "strict": true}},
	}
	var envelope responseEnvelope
	if err := client.post(ctx, "/v1/responses", payload, &envelope); err != nil {
		return err
	}
	text, err := envelope.outputText()
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return ErrInvalidOutput
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("%w: trailing JSON", ErrInvalidOutput)
	}
	return nil
}

func (client *OpenAI) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if client.Disabled() {
		return nil, ErrCapabilityDisabled
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("%w: embedding input is empty", ErrInvalidInput)
	}
	normalized := make([]string, len(inputs))
	for i, value := range inputs {
		value = sanitizeText(value)
		if value == "" {
			return nil, fmt.Errorf("%w: embedding input %d is empty", ErrInvalidInput, i)
		}
		normalized[i] = value
	}
	payload := map[string]any{"model": client.embeddingModel, "input": normalized, "dimensions": EmbeddingDimensions, "encoding_format": "float"}
	var response embeddingResponse
	if err := client.post(ctx, "/v1/embeddings", payload, &response); err != nil {
		return nil, err
	}
	if len(response.Data) != len(normalized) {
		return nil, fmt.Errorf("%w: got %d vectors for %d inputs", ErrInvalidEmbedding, len(response.Data), len(normalized))
	}
	vectors := make([][]float32, len(normalized))
	seen := make([]bool, len(normalized))
	for _, item := range response.Data {
		if item.Index < 0 || item.Index >= len(vectors) || seen[item.Index] || len(item.Embedding) != EmbeddingDimensions {
			return nil, fmt.Errorf("%w: invalid index or dimensions", ErrInvalidEmbedding)
		}
		for _, value := range item.Embedding {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return nil, fmt.Errorf("%w: non-finite value", ErrInvalidEmbedding)
			}
		}
		seen[item.Index] = true
		vectors[item.Index] = item.Embedding
	}
	return vectors, nil
}

func (client *OpenAI) post(ctx context.Context, path string, payload any, destination any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode OpenAI request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create OpenAI request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+client.apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w: OpenAI request timed out", context.DeadlineExceeded)
		}
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		return fmt.Errorf("%w: transport error", ErrHTTP)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return &RequestError{
			StatusCode: response.StatusCode,
			RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"), time.Now()),
			Temporary:  retryableHTTPStatus(response.StatusCode),
		}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8<<20))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: invalid response JSON", ErrInvalidOutput)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("%w: trailing response JSON", ErrInvalidOutput)
	}
	return nil
}

func retryableHTTPStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		const maxDurationSeconds = int64((time.Duration(1<<63 - 1)) / time.Second)
		if seconds <= 0 || seconds > maxDurationSeconds {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	retryAt, err := http.ParseTime(value)
	if err != nil || !retryAt.After(now) {
		return 0
	}
	return retryAt.Sub(now)
}

type responseEnvelope struct {
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []struct {
		Type    string                                 `json:"type"`
		Content []struct{ Type, Text, Refusal string } `json:"content"`
	} `json:"output"`
}

func (response responseEnvelope) outputText() (string, error) {
	if response.Status == "incomplete" {
		return "", ErrIncomplete
	}
	if response.Status != "completed" {
		return "", fmt.Errorf("%w: response status %q", ErrIncomplete, response.Status)
	}
	var texts []string
	for _, output := range response.Output {
		for _, content := range output.Content {
			if content.Type == "refusal" {
				return "", ErrRefusal
			}
			if content.Type == "output_text" {
				texts = append(texts, content.Text)
			}
		}
	}
	if len(texts) != 1 || strings.TrimSpace(texts[0]) == "" {
		return "", fmt.Errorf("%w: expected one output_text item", ErrInvalidOutput)
	}
	return texts[0], nil
}

type embeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func ContentHash(task, schemaVersion, promptVersion, model string, normalizedInput any) (string, error) {
	if strings.TrimSpace(task) == "" || strings.TrimSpace(schemaVersion) == "" || strings.TrimSpace(promptVersion) == "" || strings.TrimSpace(model) == "" {
		return "", fmt.Errorf("%w: hash version fields are required", ErrInvalidInput)
	}
	payload := struct {
		Task, SchemaVersion, PromptVersion, Model string
		Input                                     any
	}{task, schemaVersion, promptVersion, model, normalizedInput}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode content hash: %w", err)
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var longNumericPattern = regexp.MustCompile(`^[0-9]{16,}$`)
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bsk-[A-Za-z0-9_-]{8,}\b`),
	regexp.MustCompile(`\b7656119[0-9]{10}\b`),
	regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\b`),
	regexp.MustCompile(`(?i)\b[0-9a-f]{32,64}\b`),
	regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`),
	regexp.MustCompile(`(?i)\b(?:api[_ -]?key|session(?:[_ -]?id)?|cookie|authorization)\s*[:=]\s*[^\s,;]+`),
}

func sanitizeText(value string) string {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value))
	for _, pattern := range secretPatterns {
		value = pattern.ReplaceAllString(value, "[redacted]")
	}
	return value
}

func normalizeItems(items []EvidenceItem) (map[string]struct{}, []EvidenceItem, error) {
	if len(items) == 0 || len(items) > 200 {
		return nil, nil, fmt.Errorf("%w: evidence is empty", ErrInvalidInput)
	}
	allowed := make(map[string]struct{}, len(items))
	normalized := make([]EvidenceItem, 0, len(items))
	for _, item := range items {
		item.Key = strings.TrimSpace(item.Key)
		item.Value = sanitizeText(item.Value)
		if !validOpaqueKey(item.Key) || item.Value == "" || !utf8.ValidString(item.Value) || utf8.RuneCountInString(item.Value) > 20_000 {
			return nil, nil, fmt.Errorf("%w: invalid evidence item", ErrInvalidInput)
		}
		if _, duplicate := allowed[item.Key]; duplicate {
			return nil, nil, fmt.Errorf("%w: duplicate evidence key", ErrInvalidInput)
		}
		allowed[item.Key] = struct{}{}
		normalized = append(normalized, item)
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].Key < normalized[j].Key })
	return allowed, normalized, nil
}

func normalizeGameEvidence(input GameEvidence) (map[string]struct{}, GameEvidence, error) {
	allowed, items, err := normalizeItems(input.Evidence)
	if err != nil {
		return nil, GameEvidence{}, err
	}
	input.EvidenceKey = strings.TrimSpace(input.EvidenceKey)
	input.Name = sanitizeText(input.Name)
	input.Evidence = items
	if !validOpaqueKey(input.EvidenceKey) || input.Name == "" || !utf8.ValidString(input.Name) || utf8.RuneCountInString(input.Name) > 500 {
		return nil, GameEvidence{}, fmt.Errorf("%w: invalid game evidence", ErrInvalidInput)
	}
	return allowed, input, nil
}
func normalizeProfileEvidence(input ProfileEvidence) (map[string]struct{}, ProfileEvidence, error) {
	allowed, items, err := normalizeItems(input.Evidence)
	if err != nil {
		return nil, ProfileEvidence{}, err
	}
	input.EvidenceKey = strings.TrimSpace(input.EvidenceKey)
	input.Evidence = items
	if !validOpaqueKey(input.EvidenceKey) {
		return nil, ProfileEvidence{}, fmt.Errorf("%w: invalid profile evidence", ErrInvalidInput)
	}
	return allowed, input, nil
}
func normalizeRecommendationEvidence(input RecommendationEvidence) (map[string]struct{}, RecommendationEvidence, error) {
	allowed, items, err := normalizeItems(input.Evidence)
	if err != nil {
		return nil, RecommendationEvidence{}, err
	}
	input.EvidenceKey = strings.TrimSpace(input.EvidenceKey)
	input.GameName = sanitizeText(input.GameName)
	input.Evidence = items
	if !validOpaqueKey(input.EvidenceKey) || input.GameName == "" || !utf8.ValidString(input.GameName) || utf8.RuneCountInString(input.GameName) > 500 {
		return nil, RecommendationEvidence{}, fmt.Errorf("%w: invalid recommendation evidence", ErrInvalidInput)
	}
	return allowed, input, nil
}
func normalizeFeedback(input FeedbackText) (FeedbackText, map[string]struct{}, error) {
	input.EvidenceKey = strings.TrimSpace(input.EvidenceKey)
	original := strings.TrimSpace(input.Text)
	if !utf8.ValidString(original) || utf8.RuneCountInString(original) > 500 {
		return FeedbackText{}, nil, fmt.Errorf("%w: feedback must contain 1 to 500 characters", ErrInvalidInput)
	}
	input.Text = sanitizeText(input.Text)
	if !validOpaqueKey(input.EvidenceKey) || input.Text == "" || utf8.RuneCountInString(input.Text) > 500 {
		return FeedbackText{}, nil, fmt.Errorf("%w: feedback must contain 1 to 500 characters", ErrInvalidInput)
	}
	return input, map[string]struct{}{input.EvidenceKey: {}}, nil
}

func validateCitations(keys []string, allowed map[string]struct{}) error {
	if len(keys) == 0 {
		return fmt.Errorf("%w: output has no citations", ErrInvalidEvidence)
	}
	seen := map[string]struct{}{}
	for _, key := range keys {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("%w: %q", ErrInvalidEvidence, key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%w: duplicate %q", ErrInvalidEvidence, key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic("AI normalized input is not JSON encodable: " + err.Error())
	}
	return string(encoded)
}
func validateFeatureOutput(output GameFeatures) error {
	return validateStringArrays(20, output.Themes, output.Mechanics, output.Modes, output.Tone)
}

func normalizeAndValidateProfileOutput(output ProfileSummary, allowed map[string]struct{}) (ProfileSummary, error) {
	output.Summary = strings.TrimSpace(output.Summary)
	output.Uncertainty = strings.TrimSpace(output.Uncertainty)
	summaryLength := utf8.RuneCountInString(output.Summary)
	if !utf8.ValidString(output.Summary) || summaryLength < 320 || summaryLength > 720 || !utf8.ValidString(output.Uncertainty) || utf8.RuneCountInString(output.Uncertainty) > 2000 {
		return ProfileSummary{}, fmt.Errorf("%w: invalid profile text", ErrInvalidOutput)
	}
	output.Preferences = normalizeProfileValues(output.Preferences, 30, 144)
	output.Avoidances = normalizeProfileValues(output.Avoidances, 30, 144)
	if err := validateCitations(output.EvidenceKeys, allowed); err != nil {
		return ProfileSummary{}, err
	}
	return output, nil
}

func normalizeProfileValues(values []string, maxItems, maxRunes int) []string {
	normalized := make([]string, 0, min(len(values), maxItems))
	seen := make(map[string]struct{}, min(len(values), maxItems))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
		if len(normalized) == maxItems {
			break
		}
	}
	return normalized
}

func validateExplanationOutput(output Explanation, allowed map[string]struct{}) error {
	if strings.TrimSpace(output.Text) == "" || len(output.Text) > 4000 || len(output.Uncertainty) > 2000 {
		return fmt.Errorf("%w: invalid explanation text", ErrInvalidOutput)
	}
	if err := validateStringArrays(30, output.Facts, output.Inferences); err != nil {
		return err
	}
	return validateCitations(output.EvidenceKeys, allowed)
}

func ValidateFeedbackClassification(output FeedbackClassification, evidenceKey string) error {
	if !validTaxonomy(output.Taxonomy) || !oneOf(output.Polarity, "positive", "negative", "neutral") || output.Confidence < 0 || output.Confidence > 1 || math.IsNaN(output.Confidence) || math.IsInf(output.Confidence, 0) {
		return fmt.Errorf("%w: invalid feedback classification", ErrInvalidOutput)
	}
	if p := output.Preference; p != nil {
		paired := output.Taxonomy == "single-player" && p.Target == preferences.ModeSinglePlayer || output.Taxonomy == "multiplayer" && p.Target != preferences.ModeSinglePlayer
		if p.Dimension != "mode" || p.Direction != "avoid" || !preferences.ValidMode(p.Target) || output.Polarity != "negative" || !output.Actionable || output.Confidence < .7 || !paired {
			return ErrInvalidOutput
		}
	}
	if !validOpaqueKey(evidenceKey) || len(output.EvidenceKeys) != 1 || output.EvidenceKeys[0] != evidenceKey {
		return ErrInvalidEvidence
	}
	return nil
}

func validOpaqueKey(value string) bool {
	if !keyPattern.MatchString(value) || uuidPattern.MatchString(value) || longNumericPattern.MatchString(value) {
		return false
	}
	for _, pattern := range secretPatterns {
		if pattern.MatchString(value) {
			return false
		}
	}
	return true
}

func validateStringArrays(max int, collections ...[]string) error {
	for _, values := range collections {
		if values == nil {
			return fmt.Errorf("%w: required array is null", ErrInvalidOutput)
		}
		if len(values) > max {
			return fmt.Errorf("%w: too many values", ErrInvalidOutput)
		}
		seen := map[string]struct{}{}
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" || len(value) > 120 {
				return fmt.Errorf("%w: invalid value", ErrInvalidOutput)
			}
			if _, duplicate := seen[value]; duplicate {
				return fmt.Errorf("%w: duplicate value", ErrInvalidOutput)
			}
			seen[value] = struct{}{}
		}
	}
	return nil
}
func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
func validTaxonomy(value string) bool {
	return oneOf(value, "genre", "setting", "multiplayer", "single-player", "difficulty", "length", "early-access", "already-known", "not-now", "other")
}

func arraySchema(item map[string]any, max int) map[string]any {
	return map[string]any{"type": "array", "items": item, "maxItems": max}
}
func stringSchema() map[string]any { return map[string]any{"type": "string"} }
func objectSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func gameFeaturesSchema(allowedKeys []string) map[string]any {
	evidenceKeys := arraySchema(map[string]any{"type": "string", "enum": append([]string(nil), allowedKeys...)}, 50)
	evidenceKeys["minItems"] = 1
	return objectSchema(map[string]any{"themes": arraySchema(stringSchema(), 20), "mechanics": arraySchema(stringSchema(), 20), "modes": arraySchema(stringSchema(), 20), "tone": arraySchema(stringSchema(), 20), "evidence_keys": evidenceKeys}, "themes", "mechanics", "modes", "tone", "evidence_keys")
}
func profileSummarySchema() map[string]any {
	return objectSchema(map[string]any{"summary": stringSchema(), "preferences": arraySchema(stringSchema(), 30), "avoidances": arraySchema(stringSchema(), 30), "uncertainty": stringSchema(), "evidence_keys": arraySchema(stringSchema(), 100)}, "summary", "preferences", "avoidances", "uncertainty", "evidence_keys")
}
func explanationSchema(allowedKeys []string) map[string]any {
	evidenceKeys := arraySchema(map[string]any{"type": "string", "enum": append([]string(nil), allowedKeys...)}, len(allowedKeys))
	evidenceKeys["minItems"] = 1
	return objectSchema(map[string]any{"text": stringSchema(), "facts": arraySchema(stringSchema(), 30), "inferences": arraySchema(stringSchema(), 30), "uncertainty": stringSchema(), "evidence_keys": evidenceKeys}, "text", "facts", "inferences", "uncertainty", "evidence_keys")
}
func feedbackSchema() map[string]any {
	preference := objectSchema(map[string]any{"dimension": map[string]any{"type": "string", "enum": []string{"mode"}}, "target": map[string]any{"type": "string", "enum": preferences.Modes()}, "direction": map[string]any{"type": "string", "enum": []string{"avoid"}}}, "dimension", "target", "direction")
	return objectSchema(map[string]any{"taxonomy": map[string]any{"type": "string", "enum": []string{"genre", "setting", "multiplayer", "single-player", "difficulty", "length", "early-access", "already-known", "not-now", "other"}}, "polarity": map[string]any{"type": "string", "enum": []string{"positive", "negative", "neutral"}}, "confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1}, "actionable": map[string]any{"type": "boolean"}, "preference": map[string]any{"anyOf": []any{preference, map[string]any{"type": "null"}}}, "evidence_keys": arraySchema(stringSchema(), 1)}, "taxonomy", "polarity", "confidence", "actionable", "preference", "evidence_keys")
}

var _ Client = (*OpenAI)(nil)
