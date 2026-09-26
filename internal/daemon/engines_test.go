package daemon

import (
	"testing"

	"github.com/crom-org/openllm/internal/engines"
)

// Regressão (produção 2026-08-29): deploys com engine custom persistem a
// Definition com a porta correta (ex.: ComfyUI 8188), mas o health-check e o
// túnel resolviam a porta pelo registry builtin (Get("custom") -> fallback
// ollama 11434). A Definition persistida vence.
func TestEffectiveRemotePort(t *testing.T) {
	cfg := testConfig() // RemotePort 11434

	customDef := engines.GetCustom("custom", nil, "img", "cmd", 8188)
	if got := effectiveRemotePort(customDef, cfg, "custom"); got != 8188 {
		t.Fatalf("custom def port: got %d, want 8188", got)
	}

	ollamaDef := engines.Get("ollama")
	if got := effectiveRemotePort(ollamaDef, cfg, "ollama"); got != cfg.RemotePort {
		t.Fatalf("ollama def port: got %d, want %d", got, cfg.RemotePort)
	}

	// Definition sem porta: cai no comportamento antigo (registry/nome)
	emptyDef := engines.Definition{}
	if got := effectiveRemotePort(emptyDef, cfg, "ollama"); got != cfg.RemotePort {
		t.Fatalf("empty def fallback: got %d, want %d", got, cfg.RemotePort)
	}
}
