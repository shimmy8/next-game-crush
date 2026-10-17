package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Config contains runtime configuration. Never log it: it contains credentials.
type Config struct {
	AppBaseURL           string
	AllowedSteamID       string
	SteamWebAPIKey       string
	OpenAIAPIKey         string
	OpenAIModel          string
	OpenAIEmbeddingModel string
	DatabaseURL          string
	SMTPURL              string
	EmailActionSecret    string
	SessionSecret        string
}

func Load() (Config, error) {
	cfg := Config{
		AppBaseURL:           envOrDefault("APP_BASE_URL", "http://localhost:8080"),
		AllowedSteamID:       os.Getenv("ALLOWED_STEAM_ID"),
		SteamWebAPIKey:       os.Getenv("STEAM_WEB_API_KEY"),
		OpenAIAPIKey:         os.Getenv("OPENAI_API_KEY"),
		OpenAIModel:          envOrDefault("OPENAI_MODEL", "gpt-6-luna"),
		OpenAIEmbeddingModel: envOrDefault("OPENAI_EMBEDDING_MODEL", "text-embedding-3-small"),
		DatabaseURL:          envOrDefault("DATABASE_URL", "postgres://next_game_crush:next_game_crush@localhost:5432/next_game_crush?sslmode=disable"),
		SMTPURL:              envOrDefault("SMTP_URL", "smtp://localhost:1025"),
		EmailActionSecret:    os.Getenv("EMAIL_ACTION_SECRET"),
		SessionSecret:        os.Getenv("SESSION_SECRET"),
	}
	var problems []string
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
	return cfg, nil
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
