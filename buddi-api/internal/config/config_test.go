package config

import (
	"errors"
	"testing"
	"time"
)

func setEnv(t *testing.T, key string, value string) {
	t.Helper()

	t.Setenv(key, value)
}

func requiredEnv(t *testing.T) {
	t.Helper()

	setEnv(t, "SECRET_KEY", "secret")
	setEnv(t, "DATABASE_URI", "postgres://localhost:5432/buddi")
	setEnv(t, "JWT_SECRET", "jwt")
}

func TestLoadAppliesDefaults(t *testing.T) {
	requiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Port != "8000" {
		t.Fatalf("port = %q, want %q", cfg.Port, "8000")
	}

	if cfg.Database.MaxOpenConns != 20 || cfg.Database.MaxIdleConns != 5 {
		t.Fatalf("pool = (%d, %d), want (20, 5)", cfg.Database.MaxOpenConns, cfg.Database.MaxIdleConns)
	}

	if cfg.ShutdownGrace != 10*time.Second {
		t.Fatalf("shutdown grace = %s, want 10s", cfg.ShutdownGrace)
	}

	if !cfg.Database.MigrateOnStart {
		t.Fatal("migrate on start should default to true")
	}

	// qwen3:0.6b is the only general model in use, so a default pointing anywhere
	// else means the machine cannot serve the API without an extra multi-gigabyte
	// pull.
	if cfg.Ollama.Model != "qwen3:0.6b" {
		t.Fatalf("model = %q, want qwen3:0.6b", cfg.Ollama.Model)
	}

	if cfg.Ollama.EmbedModel != "nomic-embed-text" {
		t.Fatalf("embed model = %q, want nomic-embed-text", cfg.Ollama.EmbedModel)
	}

	if cfg.Ollama.PlannerModelOrDefault() != "qwen3:0.6b" {
		t.Fatalf("planner model = %q, want it to fall back to the general model", cfg.Ollama.PlannerModelOrDefault())
	}

	if cfg.Ollama.ContextLength != 8192 {
		t.Fatalf("context length = %d, want 8192", cfg.Ollama.ContextLength)
	}

	if cfg.Retrieval.EmbeddingDimensions != 768 || cfg.Retrieval.ChunkTokens != 500 {
		t.Fatalf("retrieval = %+v", cfg.Retrieval)
	}

	if cfg.Telemetry.Enabled {
		t.Fatal("telemetry should default to disabled without an endpoint")
	}
}

func TestLoadTrimsOriginsAndParsesValues(t *testing.T) {
	requiredEnv(t)
	setEnv(t, "ALLOWED_ORIGINS", " https://testA.example.com , https://testB.example.com ")
	setEnv(t, "SHUTDOWN_GRACE", "30s")
	setEnv(t, "DB_MAX_OPEN_CONNS", "50")
	setEnv(t, "BUDDI_MODEL", "gemma4:e2b")
	setEnv(t, "BUDDI_PLANNER_MODEL", "qwen3:4b")
	setEnv(t, "OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	setEnv(t, "OTEL_TRACES_SAMPLER_ARG", "0.5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []string{"https://testA.example.com", "https://testB.example.com"}
	if len(cfg.AllowedOrigins) != 2 || cfg.AllowedOrigins[0] != want[0] || cfg.AllowedOrigins[1] != want[1] {
		t.Fatalf("origins = %#v, want %#v", cfg.AllowedOrigins, want)
	}

	if cfg.ShutdownGrace != 30*time.Second || cfg.Database.MaxOpenConns != 50 {
		t.Fatalf("config = %+v", cfg)
	}

	if cfg.Ollama.PlannerModelOrDefault() != "qwen3:4b" {
		t.Fatalf("planner model = %q, want qwen3:4b", cfg.Ollama.PlannerModelOrDefault())
	}

	if !cfg.Telemetry.Enabled || cfg.Telemetry.SampleRatio != 0.5 {
		t.Fatalf("telemetry = %+v", cfg.Telemetry)
	}
}

func TestLoadRejectsMissingRequiredEnv(t *testing.T) {
	keys := []string{"SECRET_KEY", "DATABASE_URI", "JWT_SECRET"}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			requiredEnv(t)
			t.Setenv(key, "")

			if _, err := Load(); err == nil {
				t.Fatalf("expected error for missing %s", key)
			}
		})
	}
}

func TestLoadRejectsMalformedDuration(t *testing.T) {
	requiredEnv(t)
	setEnv(t, "JWT_EXPIRY", "not-a-duration")

	_, err := Load()
	if !errors.Is(err, ErrInvalidEnv) {
		t.Fatalf("err = %v, want ErrInvalidEnv", err)
	}
}

func TestValidateRejectsShortSigningSecretOutsideDevelopment(t *testing.T) {
	requiredEnv(t)
	setEnv(t, "ENVIRONMENT", "production")

	_, err := Load()
	if !errors.Is(err, ErrInvalidEnv) {
		t.Fatalf("err = %v, want ErrInvalidEnv", err)
	}
}

func TestValidateAllowsShortSigningSecretInDevelopment(t *testing.T) {
	requiredEnv(t)
	setEnv(t, "ENVIRONMENT", "development")

	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestValidateRejectsUnusableValues(t *testing.T) {
	cases := map[string]struct {
		key   string
		value string
	}{
		"refresh token outliving access token":    {"REFRESH_EXPIRY", "5m"},
		"bcrypt cost out of range":                {"BCRYPT_COST", "99"},
		"idle conns above open conns":             {"DB_MAX_IDLE_CONNS", "50"},
		"context length too small":                {"BUDDI_OLLAMA_CONTEXT_LENGTH", "128"},
		"negative temperature":                    {"BUDDI_OLLAMA_TEMPERATURE", "-1"},
		"overlap larger than chunk":               {"CHUNK_OVERLAP", "900"},
		"embedder ceiling below chunk size":       {"EMBED_MAX_TOKENS", "100"},
		"embedding width the schema cannot hold":  {"EMBED_DIMENSIONS", "1024"},
		"top k below one":                         {"RETRIEVAL_TOP_K", "0"},
		"similarity out of range":                 {"RETRIEVAL_MIN_SIMILARITY", "1.5"},
		"index batch below one":                   {"INDEX_BATCH_SIZE", "0"},
		"lease no longer than the poll":           {"INDEX_LEASE", "1s"},
		"planner think level the runtime rejects": {"BUDDI_PLANNER_THINK", "minimal"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			requiredEnv(t)
			setEnv(t, tc.key, tc.value)

			if _, err := Load(); !errors.Is(err, ErrInvalidEnv) {
				t.Fatalf("err = %v, want ErrInvalidEnv for %s=%s", err, tc.key, tc.value)
			}
		})
	}
}
