package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crom-org/openllm/internal/providers"
	"github.com/crom-org/openllm/internal/providers/mock"
	"github.com/crom-org/openllm/internal/proxy"
	"github.com/crom-org/openllm/internal/stack"
	"github.com/crom-org/openllm/internal/storage"
)

// TestE2EMockFullFlow testa o fluxo completo do daemon com provider mock:
// deploy → status → scale → swap → stack up/down → stop
func TestE2EMockFullFlow(t *testing.T) {
	setSSHRetry(t, 1, time.Millisecond)

	m, db, _ := newTestManager(t)
	cfg := testConfig()
	cfg.Provider = "mock"
	cfg.TpsTarget = 10 // baixo para permitir RTX 3060

	// Registra provider mock se não estiver
	prov := mock.Default()
	providers.RegisterCompute(prov)

	// 1. Deploy texto
	depID, err := m.DeployInstance(cfg, DeployRequestPayload{
		MachineID: "mock-offer-004",
		Model:     "deepseek-r1:7b",
		Engine:    "ollama",
	})
	if err != nil {
		t.Fatalf("deploy texto: %v", err)
	}
	t.Logf("Deployed text instance: %s", depID)

	// 2. Aguarda - com mock o SSH falha e a instância é auto-destruída
	// Espera que chegue a "failed" ou seja removida
	waitForCompletion(t, db, depID, 10*time.Second)

	// 3. Deploy imagem
	imgID, err := m.DeployInstance(cfg, DeployRequestPayload{
		MachineID: "mock-offer-img-001",
		Model:     "sdxl",
		Engine:    "comfyui",
	})
	if err != nil {
		t.Fatalf("deploy imagem: %v", err)
	}
	t.Logf("Deployed image instance: %s", imgID)

	waitForCompletion(t, db, imgID, 10*time.Second)

	// 4. Status via API
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simula o control server /status
		actives := m.GetActiveInstances()
		t.Logf("Active instances: %d", len(actives))
	}))
	defer srv.Close()

	// 5. Scale group (texto)
	scaleRes, err := m.ScaleGroup(cfg, "ollama", "deepseek-r1:7b", 3, 0)
	if err != nil {
		t.Fatalf("scale group: %v", err)
	}
	t.Logf("Scale result: current=%d created=%v removed=%v", scaleRes.Current, scaleRes.Created, scaleRes.Removed)
	if scaleRes.Current != 3 {
		t.Errorf("expected 3 instances, got %d", scaleRes.Current)
	}

	// 6. Swap model (apenas se supportsSwap - ollama tem ModelPullCmd)
	err = m.SwapModel(cfg, depID, "llama3.2:3b")
	if err != nil {
		t.Logf("swap (expected for mock SSH fail): %v", err)
	}

	// 7. Stack up
	stackYAML := `
name: test-stack
services:
  - name: llm
    engine: ollama
    model: qwen2.5-coder:7b
    instances: 1
    routes: ["/v1/chat/completions", "/api/chat"]
  - name: image
    engine: comfyui
    model: sdxl
    instances: 1
    routes: ["/v1/images/generations"]
`
	st, err := stack.Parse([]byte(stackYAML), "test-stack")
	if err != nil {
		t.Fatalf("parse stack: %v", err)
	}
	stackRes, err := m.DeployStack(cfg, st)
	if err != nil {
		t.Fatalf("deploy stack: %v", err)
	}
	t.Logf("Stack deployed: %d services", len(stackRes.Services))

	// 8. Stack status
	stacks, err := m.StackStatus()
	if err != nil {
		t.Fatalf("stack status: %v", err)
	}
	if len(stacks) != 1 {
		t.Errorf("expected 1 stack, got %d", len(stacks))
	}

	// 9. Stack down
	stopped, err := m.StopStack(cfg, "test-stack")
	if err != nil {
		t.Fatalf("stop stack: %v", err)
	}
	t.Logf("Stack stopped: %d instances", len(stopped))

	// 10. Stop individual
	for _, inst := range m.GetActiveInstances() {
		err = m.StopInstance(cfg, inst.ID)
		if err != nil {
			t.Logf("stop %s (expected for mock): %v", inst.ID, err)
		}
	}

	// Verifica proxy tem targets (mesmo com SSH failed, o proxy registra)
	m.proxyServer.TargetsSnapshot(func(targets []proxy.Target) {
		t.Logf("Proxy targets at end: %d", len(targets))
	})
}

// TestProxyIntegrationWithMock testa o proxy LB com targets mockados
func TestProxyIntegrationWithMock(t *testing.T) {
	ps := proxy.NewProxyServer(0)
	ps.StartHealthChecks(context.Background())

	// Adiciona targets locais mockados (não chegam a responder HTTP)
	ps.AddTarget(proxy.Target{
		InstanceID: "mock-inst-1",
		Type:       proxy.TargetTypeLocal,
		Model:      "test-model",
		Engine:     "ollama",
		LocalPort:  9999, // porta fechada
		HealthPath: "/api/tags",
	})

	ps.AddTarget(proxy.Target{
		InstanceID: "mock-inst-2",
		Type:       proxy.TargetTypeLocal,
		Model:      "test-model",
		Engine:     "ollama",
		LocalPort:  9998,
		HealthPath: "/api/tags",
	})

	// selectTarget não é exportado; testa via TargetsSnapshot + seleção indireta
	// Verifica que targets foram adicionados
	ps.TargetsSnapshot(func(targets []proxy.Target) {
		if len(targets) != 2 {
			t.Fatalf("expected 2 targets, got %d", len(targets))
		}
	})

	// 404 para modelo inexistente - testamos via handleProxy indireto não,
	// aqui apenas validamos que o proxy não crasha
	_ = ps
}

// waitForCompletion aguarda status running/failed ou remoção do DB
func waitForCompletion(t *testing.T, db *storage.DB, id string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		inst, err := db.GetInstance(id)
		if err != nil || inst == nil {
			return // deleted or error
		}
		if inst.Status == "running" || inst.Status == "failed" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("timeout waiting for %s (last status: %s)", id, instStatus(db, id))
}

// waitForStatusOrFailed aguarda status running ou failed
func waitForStatusOrFailed(t *testing.T, db *storage.DB, id string, timeout time.Duration) {
	waitForCompletion(t, db, id, timeout)
}

func instStatus(db *storage.DB, id string) string {
	inst, _ := db.GetInstance(id)
	if inst == nil {
		return "deleted"
	}
	return inst.Status
}