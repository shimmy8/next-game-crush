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
		"WORKER_CONCURRENCY":     "",
		"JOBS_ENABLED":           "",
		"DEMO_MODE_ENABLED":      "",
	} {
		t.Setenv(name, value)
	}
}

func TestLoadJobsEnabled(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		setRequiredEnvironment(t)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.JobsEnabled {
			t.Fatal("jobs must default to enabled")
		}
	})

	for _, value := range []string{"1", "true"} {
		t.Run("enabled "+value, func(t *testing.T) {
			setRequiredEnvironment(t)
			t.Setenv("JOBS_ENABLED", value)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.JobsEnabled {
				t.Fatalf("JobsEnabled = false for %q", value)
			}
		})
	}

	for _, value := range []string{"0", "false"} {
		t.Run("disabled "+value, func(t *testing.T) {
			setRequiredEnvironment(t)
			t.Setenv("JOBS_ENABLED", value)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.JobsEnabled {
				t.Fatalf("JobsEnabled = true for %q", value)
			}
		})
	}

	for _, value := range []string{"yes", "TRUE", "2"} {
		t.Run("reject "+value, func(t *testing.T) {
			setRequiredEnvironment(t)
			t.Setenv("JOBS_ENABLED", value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JOBS_ENABLED") {
				t.Fatalf("Load() error = %v, want JOBS_ENABLED validation error", err)
			}
		})
	}
}

func TestLoadDemoModeIsExplicitlyOptIn(t *testing.T) {
	setRequiredEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DemoMode {
		t.Fatal("demo mode must default to disabled")
	}
	t.Setenv("DEMO_MODE_ENABLED", "1")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DemoMode {
		t.Fatal("demo mode was not enabled")
	}
}

func TestLoadWorkerConcurrency(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		setRequiredEnvironment(t)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.WorkerConcurrency != 10 {
			t.Fatalf("WorkerConcurrency = %d, want 10", cfg.WorkerConcurrency)
		}
	})

	t.Run("explicit", func(t *testing.T) {
		setRequiredEnvironment(t)
		t.Setenv("WORKER_CONCURRENCY", "16")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.WorkerConcurrency != 16 {
			t.Fatalf("WorkerConcurrency = %d, want 16", cfg.WorkerConcurrency)
		}
	})

	for _, value := range []string{"0", "65", "not-a-number"} {
		t.Run("reject "+value, func(t *testing.T) {
			setRequiredEnvironment(t)
			t.Setenv("WORKER_CONCURRENCY", value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "WORKER_CONCURRENCY") {
				t.Fatalf("Load() error = %v, want WORKER_CONCURRENCY validation error", err)
			}
		})
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

func TestLoadAllowsMissingOpenAIKey(t *testing.T) {
	for _, value := range []string{"", " \t\n"} {
		t.Run(value, func(t *testing.T) {
			setRequiredEnvironment(t)
			t.Setenv("OPENAI_API_KEY", value)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.OpenAI.Enabled {
				t.Error("OpenAI must be disabled without a nonblank key")
			}
			if cfg.OpenAIAPIKey != "" {
				t.Error("OpenAIAPIKey must be empty after trimming whitespace")
			}
		})
	}
}

func TestLoadEnablesOpenAIWithTrimmedKey(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("OPENAI_API_KEY", " test-openai-key \t")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.OpenAI.Enabled {
		t.Error("OpenAI must be enabled with a configured key")
	}
	if cfg.OpenAIAPIKey != "test-openai-key" {
		t.Error("OpenAIAPIKey must be trimmed")
	}
}

func TestFixtureAuthFailsClosedOutsideTestHarness(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("FIXTURE_AUTH_ENABLED", "1")
	t.Setenv("TEST_HARNESS_MODE", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "TEST_HARNESS_MODE=1") {
		t.Fatalf("Load() error = %v, want fail-closed fixture-auth validation", err)
	}
}
