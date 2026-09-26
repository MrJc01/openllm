package config

import "testing"

// Especialista: Segurança/QA — validação rejeita configs incompletas com
// mensagens acionáveis.
func TestValidateErrors(t *testing.T) {
	// Provider vazio
	cfg := &Config{Model: "m"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("empty provider must be rejected")
	}

	// API key vazia
	cfg = &Config{Provider: "vastai", Model: "m"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("empty api key must be rejected")
	}

	// Model vazio
	t.Setenv("OPENLLM_VASTAI_API_KEY", "k")
	cfg = &Config{Provider: "vastai"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("empty model must be rejected")
	}

	// Válida + InstancesCount normalizado
	cfg = &Config{Provider: "vastai", Model: "m", InstancesCount: 0}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if cfg.InstancesCount != 1 {
		t.Fatalf("InstancesCount should normalize to 1, got %d", cfg.InstancesCount)
	}
}

// Especialista: Segurança — chaves de providers variados via env var.
func TestActiveAPIKeyEnvNaming(t *testing.T) {
	t.Setenv("OPENLLM_RUNPOD_API_KEY", "rp-key")
	cfg := &Config{Provider: "runpod"}
	if got := cfg.ActiveAPIKey(); got != "rp-key" {
		t.Fatalf("expected env key for runpod, got %q", got)
	}

	t.Setenv("OPENLLM_LAMBDA_LABS_API_KEY", "lambda-key")
	cfg = &Config{Provider: "lambda-labs"} // hífen vira underscore
	if got := cfg.ActiveAPIKey(); got != "lambda-key" {
		t.Fatalf("expected env key for lambda-labs, got %q", got)
	}
}
