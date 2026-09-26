package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestActiveAPIKeyFromEnv(t *testing.T) {
	cfg := &Config{Provider: "vastai"}
	t.Setenv("OPENLLM_VASTAI_API_KEY", "env-key-123")
	if got := cfg.ActiveAPIKey(); got != "env-key-123" {
		t.Fatalf("expected env key, got %q", got)
	}
}

func TestActiveAPIKeyFromFile(t *testing.T) {
	cfg := &Config{
		Provider: "vastai",
		Providers: map[string]ProviderConfig{
			"vastai": {APIKey: "file-key"},
		},
	}
	t.Setenv("OPENLLM_VASTAI_API_KEY", "")
	if got := cfg.ActiveAPIKey(); got != "file-key" {
		t.Fatalf("expected file key, got %q", got)
	}
}

func TestLoadConfigLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	legacy := `{"vastai_api_key": "legacy-key", "model": "m1"}`
	if err := os.WriteFile(ConfigFileName, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LegacyVastAiApiKey != "" {
		t.Fatal("legacy field should be cleared after migration")
	}
	if cfg.Providers["vastai"].APIKey != "legacy-key" {
		t.Fatalf("expected migrated key, got %q", cfg.Providers["vastai"].APIKey)
	}

	// O arquivo salvo não deve mais conter o campo legado
	data, _ := os.ReadFile(filepath.Join(dir, ConfigFileName))
	if string(data) == legacy {
		t.Fatal("config file should be rewritten after migration")
	}
}
