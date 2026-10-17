package config

import (
	"strings"
	"testing"
)

func setRequiredEnvironment(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"APP_BASE_URL":           "http://localhost:8080",
		"ALLOWED_STEAM_ID":       "76561198000000000",
		"STEAM_WEB_API_KEY":      "test-steam-key",
		"OPENAI_API_KEY":         "test-openai-key",
		"DATABASE_URL":           "postgres://next_game_crush:next_game_crush@localhost:5432/next_game_crush?sslmode=disable",
		"SMTP_URL":               "smtp://localhost:1025",
		"EMAIL_ACTION_SECRET":    "test-email-action-secret",
		"SESSION_SECRET":         "test-session-secret",
		"OPENAI_MODEL":           "",
		"OPENAI_EMBEDDING_MODEL": "",
	} {
		t.Setenv(name, value)
	}
}

func TestLoadRejectsMissingRequiredSecrets(t *testing.T) {
	for _, name := range []string{"ALLOWED_STEAM_ID", "STEAM_WEB_API_KEY", "EMAIL_ACTION_SECRET", "SESSION_SECRET"} {
		t.Run(name, func(t *testing.T) {
			setRequiredEnvironment(t)
			t.Setenv(name, "")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("Load() error = %v, want named validation error for %s", err, name)
			}
		})
	}
}

func TestLoadUsesDocumentedModelDefaults(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("OPENAI_API_KEY", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAIModel != "gpt-6-luna" {
		t.Errorf("OpenAIModel = %q, want gpt-6-luna", cfg.OpenAIModel)
	}
	if cfg.OpenAIEmbeddingModel != "text-embedding-3-small" {
		t.Errorf("OpenAIEmbeddingModel = %q, want text-embedding-3-small", cfg.OpenAIEmbeddingModel)
	}
}
