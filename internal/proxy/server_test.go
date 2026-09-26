package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func parseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("bad url: %v", err)
	}
	return u
}

func portOf(u *url.URL) int {
	port, _ := strconv.Atoi(u.Port())
	return port
}

func healthyTargets() []Target {
	return []Target{
		{InstanceID: "i1", Type: TargetTypeLocal, Model: "a", LocalPort: 1001},
		{InstanceID: "i2", Type: TargetTypeLocal, Model: "a", LocalPort: 1002},
		{InstanceID: "i3", Type: TargetTypeLocal, Model: "b", LocalPort: 1003},
	}
}

func TestSelectTargetByModel(t *testing.T) {
	p := NewProxyServer(0)
	p.UpdateTargets(healthyTargets())

	tgt, err := p.selectTarget("a", "/api/chat")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tgt.Model != "a" {
		t.Fatalf("expected model a, got %s", tgt.Model)
	}
}

func TestSelectTargetUnknownModelFails(t *testing.T) {
	p := NewProxyServer(0)
	p.UpdateTargets(healthyTargets())

	// G3 corrigido: modelo desconhecido NÃO deve cair no targets[0]
	if _, err := p.selectTarget("nao-existe", "/api/chat"); err == nil {
		t.Fatal("expected error for unknown model, got nil")
	}
}

func TestSelectTargetEmptyModelUsesAll(t *testing.T) {
	p := NewProxyServer(0)
	p.UpdateTargets(healthyTargets())

	if _, err := p.selectTarget("", "/api/chat"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSelectTargetRoundRobinDistribution(t *testing.T) {
	p := NewProxyServer(0)
	p.UpdateTargets(healthyTargets())

	counts := map[string]int{}
	for i := 0; i < 20; i++ {
		tgt, err := p.selectTarget("a", "/api/chat")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		counts[tgt.InstanceID]++
	}
	if counts["i1"] != 10 || counts["i2"] != 10 {
		t.Fatalf("round-robin not even: %v", counts)
	}
}

func TestSelectTargetLeastConnections(t *testing.T) {
	p := NewProxyServer(0)
	p.SetStrategy("least-connections")
	p.UpdateTargets(healthyTargets())

	// Ocupa i1 com uma requisição "em voo"
	c1 := p.getInFlight("i1")
	atomic.AddInt64(c1, 1)
	defer atomic.AddInt64(c1, -1)

	tgt, err := p.selectTarget("a", "/api/chat")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tgt.InstanceID != "i2" {
		t.Fatalf("expected least-connections to pick i2, got %s", tgt.InstanceID)
	}
}

func TestSelectTargetUnhealthyEjected(t *testing.T) {
	p := NewProxyServer(0)
	p.UpdateTargets(healthyTargets())

	p.healthMu.Lock()
	p.healthy["i1"] = false
	p.healthMu.Unlock()

	for i := 0; i < 5; i++ {
		tgt, err := p.selectTarget("a", "/api/chat")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tgt.InstanceID == "i1" {
			t.Fatal("unhealthy target should not be selected")
		}
	}
}

func TestSelectTargetRouteMatching(t *testing.T) {
	p := NewProxyServer(0)
	targets := []Target{
		{InstanceID: "llm", Type: TargetTypeLocal, Model: "deepseek", LocalPort: 1001, Routes: []string{"/api/chat"}},
		{InstanceID: "img", Type: TargetTypeLocal, Model: "sdxl", LocalPort: 1002, Routes: []string{"/v1/images"}},
	}
	p.UpdateTargets(targets)

	tgt, err := p.selectTarget("", "/v1/images/generations")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tgt.InstanceID != "img" {
		t.Fatalf("expected route to pick img target, got %s", tgt.InstanceID)
	}

	tgt, err = p.selectTarget("", "/api/chat")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tgt.InstanceID != "llm" {
		t.Fatalf("expected route to pick llm target, got %s", tgt.InstanceID)
	}
}

func TestReplaceLocalTargetsPreservesRemote(t *testing.T) {
	p := NewProxyServer(0)
	p.AddTarget(Target{InstanceID: "remote-or", Type: TargetTypeRemote, Model: "gpt-4o", BaseURL: "https://x"})
	p.ReplaceLocalTargets(healthyTargets())

	models := p.AvailableModels()
	found := map[string]bool{}
	for _, m := range models {
		found[m] = true
	}
	if !found["gpt-4o"] || !found["a"] || !found["b"] {
		t.Fatalf("expected remote+local merged, got %v", models)
	}
}

func TestHandleProxyModelNotFound404(t *testing.T) {
	p := NewProxyServer(0)
	p.UpdateTargets(healthyTargets())

	body, _ := json.Marshal(map[string]string{"model": "fantasma"})
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytesReader(body))
	rec := httptest.NewRecorder()

	p.handleProxy(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if _, ok := resp["available"]; !ok {
		t.Fatal("expected 'available' field in 404 response")
	}
}

func TestHandleProxyLoadBalancesAcrossBackends(t *testing.T) {
	p := NewProxyServer(0)
	var mu sync.Mutex
	hits := map[string]int{}

	for _, id := range []string{"b1", "b2"} {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[id]++
			mu.Unlock()
			w.Write([]byte(`{"ok":true}`))
		}))
		defer backend.Close()

		parts := parseURL(t, backend.URL)
		p.AddTarget(Target{InstanceID: id, Type: TargetTypeLocal, Model: "m", LocalPort: portOf(parts)})
	}

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/chat", bytesReader([]byte(`{"model":"m"}`)))
		rec := httptest.NewRecorder()
		p.handleProxy(rec, req)
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["b1"] != 5 || hits["b2"] != 5 {
		t.Fatalf("expected even distribution, got %v", hits)
	}
}

func TestProxyStartAndHealthChecks(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer backend.Close()

	p := NewProxyServer(0)
	p.AddTarget(Target{InstanceID: "hb", Type: TargetTypeLocal, Model: "m", LocalPort: portOf(parseURL(t, backend.URL)), HealthPath: "/"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.runHealthCheckRound()

	if !p.isHealthy("hb") {
		t.Fatal("healthy backend should be reported healthy")
	}
	_ = ctx
}
