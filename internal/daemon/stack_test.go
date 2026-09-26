package daemon

import (
	"testing"
	"time"

	"github.com/crom-org/openllm/internal/stack"
	"github.com/crom-org/openllm/internal/storage"
)

const stackYAML = `
name: test-stack
services:
  - name: llm
    model: deepseek-r1:7b
    instances: 2
    routes:
      - /api/chat
  - name: image
    model: sdxl
    instances: 1
    routes:
      - /v1/images/generations
`

// Especialista: QA — DeployStack registra grupos com rotas e instancia os
// serviços; StopStack derruba APENAS as instâncias do stack.
func TestStackDeployAndStopLifecycle(t *testing.T) {
	m, db, _ := newTestManager(t)
	cfg := testConfig()

	st, err := stack.Parse([]byte(stackYAML), "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	result, err := m.DeployStack(cfg, st)
	if err != nil {
		t.Fatalf("deploy stack: %v", err)
	}
	if result.Name != "test-stack" || len(result.Services) != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}

	// Engine da image resolvida pelo catálogo → comfyui
	if result.Services[1].Engine != "comfyui" {
		t.Fatalf("catalog resolution failed: %+v", result.Services[1])
	}

	// Grupos com stack + rotas persistidos
	g, err := db.GetGroup("comfyui/sdxl")
	if err != nil || g == nil {
		t.Fatalf("group comfyui/sdxl missing: %v", err)
	}
	if g.Stack != "test-stack" || len(g.Routes) != 1 {
		t.Fatalf("group metadata wrong: %+v", g)
	}

	// 3 instâncias criadas no total (2 + 1)
	total := 0
	for _, svc := range result.Services {
		total += len(svc.Created)
	}
	if total != 3 {
		t.Fatalf("expected 3 instances created, got %d", total)
	}

	// Instância fora do stack não deve ser afetada pelo down
	db.SaveInstance(&storage.Instance{
		ID: "outsider", Provider: "mock", Status: "running", Model: "other",
		Engine: "ollama", GroupID: "ollama/other", CostPerHour: 0.5, CreatedAt: time.Now(),
	})

	stopped, err := m.StopStack(cfg, "test-stack")
	if err != nil {
		t.Fatalf("stop stack: %v", err)
	}
	if len(stopped) != 3 {
		t.Fatalf("expected 3 stopped, got %d (%v)", len(stopped), stopped)
	}
	for _, id := range stopped {
		if id == "outsider" {
			t.Fatal("StopStack must not touch instances outside the stack")
		}
	}

	// Grupos do stack removidos; outsider continua
	if g, _ := db.GetGroup("comfyui/sdxl"); g != nil {
		t.Fatal("stack groups should be deleted on down")
	}
	if got, _ := db.GetInstance("outsider"); got == nil {
		t.Fatal("outsider instance must survive stack down")
	}
}

// Especialista: QA — StackStatus agrupa corretamente após o deploy.
func TestStackStatus(t *testing.T) {
	m, db, _ := newTestManager(t)
	cfg := testConfig()

	st, _ := stack.Parse([]byte(stackYAML), "")
	if _, err := m.DeployStack(cfg, st); err != nil {
		t.Fatal(err)
	}

	// Instância de outro "stack" que não deve aparecer
	db.SaveInstance(&storage.Instance{
		ID: "x1", Provider: "mock", Status: "running", Model: "other",
		Engine: "ollama", GroupID: "ollama/other", CreatedAt: time.Now(),
	})

	stacks, err := m.StackStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(stacks) != 1 || stacks[0].Name != "test-stack" {
		t.Fatalf("unexpected stacks: %+v", stacks)
	}
	if len(stacks[0].Services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(stacks[0].Services))
	}
	// YAML: llm=2 réplicas, image=1
	expected := map[string]int{
		"ollama/deepseek-r1:7b": 2,
		"comfyui/sdxl":          1,
	}
	for _, svc := range stacks[0].Services {
		want, ok := expected[svc.GroupID]
		if !ok {
			t.Fatalf("unexpected service in stack: %s", svc.GroupID)
		}
		if svc.Instances != want {
			t.Fatalf("group %s: expected %d active instances, got %d", svc.GroupID, want, svc.Instances)
		}
	}
}
