package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const maxWorkerConcurrency = 64

// OpenAICapability reports whether credentials are configured for AI calls.
type OpenAICapability struct {
	Enabled bool
}

// Config contains runtime configuration. Never log it: it contains credentials.
type Config struct {
	AppBaseURL           string
	AllowedSteamID       string
	SteamWebAPIKey       string
	OpenAIAPIKey         string
	OpenAI               OpenAICapability
	OpenAIModel          string
	OpenAIEmbeddingModel string
	DatabaseURL          string
	SMTPURL              string
	EmailActionSecret    string
	SessionSecret        string
	TestHarness          bool
	FixtureAuth          bool
	SteamWebAPIBaseURL   string
	SteamStoreBaseURL    string
	OpenAIBaseURL        string
	HTTPAddr             string
	WorkerConcurrency    int
}

func Load() (Config, error) {
	cfg := Config{
		AppBaseURL:           envOrDefault("APP_BASE_URL", "http://localhost:8080"),
		AllowedSteamID:       os.Getenv("ALLOWED_STEAM_ID"),
		SteamWebAPIKey:       os.Getenv("STEAM_WEB_API_KEY"),
		OpenAIAPIKey:         strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		OpenAIModel:          envOrDefault("OPENAI_MODEL", "gpt-6-luna"),
		OpenAIEmbeddingModel: envOrDefault("OPENAI_EMBEDDING_MODEL", "text-embedding-3-small"),
		DatabaseURL:          envOrDefault("DATABASE_URL", "postgres://next_game_crush:next_game_crush@localhost:5432/next_game_crush?sslmode=disable"),
		SMTPURL:              envOrDefault("SMTP_URL", "smtp://localhost:1025"),
		EmailActionSecret:    os.Getenv("EMAIL_ACTION_SECRET"),
		SessionSecret:        os.Getenv("SESSION_SECRET"),
		TestHarness:          os.Getenv("TEST_HARNESS_MODE") == "1",
		FixtureAuth:          os.Getenv("FIXTURE_AUTH_ENABLED") == "1",
		SteamWebAPIBaseURL:   strings.TrimSpace(os.Getenv("STEAM_WEB_API_BASE_URL")),
		SteamStoreBaseURL:    strings.TrimSpace(os.Getenv("STEAM_STORE_BASE_URL")),
		OpenAIBaseURL:        strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
		HTTPAddr:             envOrDefault("HTTP_ADDR", ":8080"),
		WorkerConcurrency:    10,
	}
	cfg.OpenAI = OpenAICapability{Enabled: cfg.OpenAIAPIKey != ""}
	var problems []string
	if raw := strings.TrimSpace(os.Getenv("WORKER_CONCURRENCY")); raw != "" {
		concurrency, err := strconv.Atoi(raw)
		if err != nil || concurrency < 1 || concurrency > maxWorkerConcurrency {
			problems = append(problems, fmt.Sprintf("WORKER_CONCURRENCY must be an integer between 1 and %d", maxWorkerConcurrency))
		} else {
			cfg.WorkerConcurrency = concurrency
		}
	}
	for _, required := range []struct{ name, value string }{
		{"ALLOWED_STEAM_ID", cfg.AllowedSteamID},
		{"STEAM_WEB_API_KEY", cfg.SteamWebAPIKey},
		{"EMAIL_ACTION_SECRET", cfg.EmailActionSecret},
		{"SESSION_SECRET", cfg.SessionSecret},
	} {
		if strings.TrimSpace(required.value) == "" {
			problems = append(problems, required.name+" is required")
		}
	}
	for _, setting := range []struct {
		name, value string
		schemes     []string
	}{
		{"APP_BASE_URL", cfg.AppBaseURL, []string{"http", "https"}},
		{"DATABASE_URL", cfg.DatabaseURL, []string{"postgres", "postgresql"}},
		{"SMTP_URL", cfg.SMTPURL, []string{"smtp", "smtps"}},
	} {
		parsed, err := url.Parse(setting.value)
		valid := err == nil && parsed.Host != ""
		allowedScheme := false
		if err == nil {
			for _, scheme := range setting.schemes {
				allowedScheme = allowedScheme || parsed.Scheme == scheme
			}
		}
		if !valid || !allowedScheme {
			problems = append(problems, setting.name+" must be an absolute URL with a supported scheme")
		}
	}
	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %s", strings.Join(problems, "; "))
	}
	if cfg.FixtureAuth && !cfg.TestHarness {
		return Config{}, fmt.Errorf("invalid configuration: FIXTURE_AUTH_ENABLED requires TEST_HARNESS_MODE=1")
	}
	if !cfg.TestHarness && (cfg.SteamWebAPIBaseURL != "" || cfg.SteamStoreBaseURL != "" || cfg.OpenAIBaseURL != "") {
		return Config{}, fmt.Errorf("invalid configuration: test upstream URL overrides require TEST_HARNESS_MODE=1")
	}
	return cfg, nil
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
