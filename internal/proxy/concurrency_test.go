package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Especialista: Performance — selectTarget e UpdateTargets concorrentes não
// devem gerar data races (rodar com -race) nem panics.
func TestConcurrentUpdatesAndSelects(t *testing.T) {
	p := NewProxyServer(0)
	p.UpdateTargets(healthyTargets())

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Escritores: trocam o pool o tempo todo
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				tgts := healthyTargets()
				if w%2 == 0 {
					tgts = append(tgts, Target{InstanceID: fmt.Sprintf("extra-%d-%d", w, i), Type: TargetTypeLocal, Model: "a", LocalPort: 9999})
				}
				p.UpdateTargets(tgts)
			}
		}(w)
	}

	// Leitores: selecionam alvos sem parar
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					_, _ = p.selectTarget("a", "/api/chat")
					_, _ = p.selectTarget("", "/v1/images/generations")
					_ = p.AvailableModels()
				}
			}
		}(r)
	}

	time.Sleep(300 * time.Millisecond)
	close(done)
	wg.Wait()
}

// Especialista: Performance — 200 requisições concorrentes distribuídas entre
// 2 backends, todas 200, sem race nos contadores.
func TestConcurrentRequestsDistribution(t *testing.T) {
	var hits1, hits2 int64

	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits1, 1)
		w.WriteHeader(200)
	}))
	defer backend1.Close()
	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits2, 1)
		w.WriteHeader(200)
	}))
	defer backend2.Close()

	p := NewProxyServer(0)
	p.AddTarget(Target{InstanceID: "b1", Type: TargetTypeLocal, Model: "m", LocalPort: portOf(parseURL(t, backend1.URL))})
	p.AddTarget(Target{InstanceID: "b2", Type: TargetTypeLocal, Model: "m", LocalPort: portOf(parseURL(t, backend2.URL))})

	var wg sync.WaitGroup
	errs := make(chan int, 200)
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"m"}`))
			rec := httptest.NewRecorder()
			p.handleProxy(rec, req)
			if rec.Code != 200 {
				errs <- rec.Code
			}
		}()
	}
	wg.Wait()
	close(errs)
	for code := range errs {
		t.Fatalf("got non-200: %d", code)
	}

	h1, h2 := atomic.LoadInt64(&hits1), atomic.LoadInt64(&hits2)
	if h1+h2 != 200 {
		t.Fatalf("expected 200 total hits, got %d", h1+h2)
	}
	if h1 < 60 || h2 < 60 { // tolerância para round-robin sob concorrência
		t.Fatalf("distribution too skewed: b1=%d b2=%d", h1, h2)
	}
}

// Especialista: SRE — health-check marca 5xx/indisponível como doente e
// 2xx como saudável.
func TestPingHealth(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ok.Close()
	if !pingHealth(portOf(parseURL(t, ok.URL)), "/") {
		t.Fatal("200 should be healthy")
	}

	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer sick.Close()
	if pingHealth(portOf(parseURL(t, sick.URL)), "/") {
		t.Fatal("503 should be unhealthy")
	}

	// Porta fechada (conexão recusada)
	if pingHealth(1, "/") {
		t.Fatal("connection refused should be unhealthy")
	}
}

// Especialista: SRE — estado de saúde sobrevive a UpdateTargets do mesmo
// target, mas reseta se o target sumir e voltar.
func TestHealthStateSurvivesTargetUpdate(t *testing.T) {
	p := NewProxyServer(0)
	p.UpdateTargets(healthyTargets())

	p.healthMu.Lock()
	p.healthy["i1"] = false
	p.healthMu.Unlock()

	p.UpdateTargets(healthyTargets())
	if p.isHealthy("i1") {
		t.Fatal("health state should be preserved across updates for same instance")
	}

	// Remove e re-adiciona: volta ao default saudável
	p.UpdateTargets(healthyTargets()[1:])
	p.UpdateTargets(healthyTargets())
	if !p.isHealthy("i1") {
		t.Fatal("re-added target should default to healthy")
	}
}

// Especialista: SRE — cleanup de estado não vaza contadores para targets
// removidos (memória estável em deploys repetidos).
func TestCleanupRemovedStateNoLeak(t *testing.T) {
	p := NewProxyServer(0)
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("inst-%d", i)
		p.UpdateTargets([]Target{{InstanceID: id, Type: TargetTypeLocal, Model: "m", LocalPort: 9000 + i}})
		p.getInFlight(id) // força criação do contador
	}

	if len(p.inFlight) == 0 {
		t.Fatal("expected in-flight counters to exist during test")
	}

	// Pool final com apenas 1 target: estado dos 99 anteriores deve sumir
	p.UpdateTargets([]Target{{InstanceID: "inst-99", Type: TargetTypeLocal, Model: "m", LocalPort: 9099}})
	if len(p.inFlight) > 1 {
		t.Fatalf("in-flight map leaked: %d entries", len(p.inFlight))
	}
	if len(p.healthy) > 1 {
		t.Fatalf("healthy map leaked: %d entries", len(p.healthy))
	}
}

// Especialista: QA — rotas têm prioridade sobre match por modelo e a rota "/"
// não captura todo tráfego.
func TestRoutePriorityAndRootRouteExcluded(t *testing.T) {
	// A rota "/" não deve capturar nenhum path (senão um stack com rota "/"
	// roubaria o tráfego dos outros serviços)
	web := Target{InstanceID: "web", Type: TargetTypeLocal, Model: "ui", LocalPort: 1001, Routes: []string{"/"}}
	if web.matchesPath("/outra/coisa") || web.matchesPath("/api/chat") {
		t.Fatal("route '/' must not capture paths")
	}

	p := NewProxyServer(0)
	p.UpdateTargets([]Target{
		web,
		{InstanceID: "llm", Type: TargetTypeLocal, Model: "deepseek", LocalPort: 1002, Routes: []string{"/api/chat"}},
	})

	// "/api/chat" bate na rota do llm mesmo com modelo vazio
	tgt, err := p.selectTarget("", "/api/chat")
	if err != nil || tgt.InstanceID != "llm" {
		t.Fatalf("expected llm by route, got %s err=%v", tgt.InstanceID, err)
	}

	// Com modelo explícito, o match por modelo vence
	tgt, err = p.selectTarget("deepseek", "/api/chat")
	if err != nil || tgt.InstanceID != "llm" {
		t.Fatalf("expected llm by model, got %s err=%v", tgt.InstanceID, err)
	}

	// Path sem rota e sem modelo → qualquer backend saudável serve (sem erro)
	if _, err = p.selectTarget("", "/outra/coisa"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
