package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TargetType distingue entre backends locais (SSH tunnel) e remotos (API externa).
type TargetType string

const (
	// TargetTypeLocal: Ollama rodando localmente via túnel SSH (Vast.ai, RunPod...)
	TargetTypeLocal TargetType = "local"
	// TargetTypeRemote: API externa compatível com OpenAI (OpenRouter, Together.ai...)
	TargetTypeRemote TargetType = "remote"
)

// Target representa um backend de inferência disponível para o load balancer.
type Target struct {
	InstanceID string
	Type       TargetType
	Model      string
	Engine     string
	GroupID    string

	// Routes são paths (prefixos) que este target atende quando configurado
	// em um stack multimodal. Vazio = sem rota específica (match por modelo).
	Routes []string

	// TargetTypeLocal: porta local do túnel SSH
	LocalPort int

	// TargetTypeRemote: URL base e headers de autenticação
	BaseURL     string
	AuthHeaders map[string]string

	// HealthPath é o endpoint de saúde sondado periodicamente. Vazio =
	// sem health-check (target assumido saudável).
	HealthPath string
}

// matchesPath verifica se o path da requisição bate com alguma rota do target.
func (t Target) matchesPath(path string) bool {
	if len(t.Routes) == 0 {
		return false
	}
	for _, route := range t.Routes {
		if route == "/" {
			continue // rota "/" genérica não captura tudo em stacks
		}
		if strings.HasPrefix(path, route) {
			return true
		}
	}
	return false
}

type ProxyServer struct {
	localProxyPort int
	targets        []Target
	targetsMu      sync.RWMutex
	strategy       string // "round-robin" (default) ou "least-connections"

	rrCounters map[string]*uint64
	rrMu       sync.Mutex
	inFlight   map[string]*int64 // conexões em voo por InstanceID
	inFlightMu sync.RWMutex

	healthy   map[string]bool // InstanceID → saudável (default true)
	healthMu  sync.RWMutex

	server *http.Server
}

func NewProxyServer(port int) *ProxyServer {
	return &ProxyServer{
		localProxyPort: port,
		strategy:       "round-robin",
		rrCounters:     make(map[string]*uint64),
		inFlight:       make(map[string]*int64),
		healthy:        make(map[string]bool),
	}
}

// SetStrategy define a estratégia de balanceamento ("round-robin" ou
// "least-connections"). Ignora valores desconhecidos.
func (p *ProxyServer) SetStrategy(s string) {
	if s != "round-robin" && s != "least-connections" {
		return
	}
	p.strategy = s
}

// AddTarget insere ou substitui um único target preservando os demais
// (usado por providers de inferência — não derruba backends locais).
func (p *ProxyServer) AddTarget(t Target) {
	p.targetsMu.Lock()
	defer p.targetsMu.Unlock()
	for i, existing := range p.targets {
		if existing.InstanceID == t.InstanceID {
			p.targets[i] = t
			return
		}
	}
	p.targets = append(p.targets, t)
}

// RemoveTarget remove um backend do pool pelo InstanceID.
func (p *ProxyServer) RemoveTarget(instanceID string) {
	p.targetsMu.Lock()
	defer p.targetsMu.Unlock()
	for i, existing := range p.targets {
		if existing.InstanceID == instanceID {
			p.targets = append(p.targets[:i], p.targets[i+1:]...)
			return
		}
	}
}

// ReplaceLocalTargets substitui APENAS os backends locais (túneis SSH),
// preservando os remotos (providers de inferência). Cleanup de estado
// (contadores/health) para IDs que deixaram de existir.
func (p *ProxyServer) ReplaceLocalTargets(newTargets []Target) {
	p.targetsMu.Lock()
	kept := make([]Target, 0)
	for _, t := range p.targets {
		if t.Type == TargetTypeRemote {
			kept = append(kept, t)
		}
	}
	p.targets = append(kept, newTargets...)
	p.targetsMu.Unlock()

	p.cleanupRemovedState(append(kept, newTargets...))
}

// UpdateTargets substitui a lista inteira de alvos de balanceamento
// (usado em testes e reinicializações completas). O estado de health-check
// de IDs que continuam existindo é preservado.
func (p *ProxyServer) UpdateTargets(newTargets []Target) {
	p.targetsMu.Lock()
	p.targets = newTargets
	p.targetsMu.Unlock()

	p.cleanupRemovedState(newTargets)
}

func (p *ProxyServer) cleanupRemovedState(targets []Target) {
	p.rrMu.Lock()
	for model := range p.rrCounters {
		found := false
		for _, t := range targets {
			if t.Model == model {
				found = true
				break
			}
		}
		if !found {
			delete(p.rrCounters, model)
		}
	}
	p.rrMu.Unlock()

	p.inFlightMu.Lock()
	for id := range p.inFlight {
		if !containsID(targets, id) {
			delete(p.inFlight, id)
		}
	}
	p.inFlightMu.Unlock()

	p.healthMu.Lock()
	for id := range p.healthy {
		if !containsID(targets, id) {
			delete(p.healthy, id)
		}
	}
	p.healthMu.Unlock()
}

func containsID(targets []Target, id string) bool {
	for _, t := range targets {
		if t.InstanceID == id {
			return true
		}
	}
	return false
}

// AvailableModels lista os modelos distintos entre os targets saudáveis.
func (p *ProxyServer) AvailableModels() []string {
	p.targetsMu.RLock()
	defer p.targetsMu.RUnlock()
	seen := map[string]bool{}
	models := []string{}
	for _, t := range p.targets {
		if t.Model != "" && !seen[t.Model] {
			seen[t.Model] = true
			models = append(models, t.Model)
		}
	}
	return models
}

// TargetsSnapshot executa fn com uma cópia segura da lista atual de targets
// (usado pelo /status e testes para inspecionar o pool sem corridas).
func (p *ProxyServer) TargetsSnapshot(fn func([]Target)) {
	p.targetsMu.RLock()
	defer p.targetsMu.RUnlock()
	cp := make([]Target, len(p.targets))
	copy(cp, p.targets)
	fn(cp)
}

func (p *ProxyServer) getCounter(model string) *uint64 {
	p.rrMu.Lock()
	defer p.rrMu.Unlock()
	if c, exists := p.rrCounters[model]; exists {
		return c
	}
	var val uint64
	p.rrCounters[model] = &val
	return &val
}

func (p *ProxyServer) getInFlight(id string) *int64 {
	p.inFlightMu.Lock()
	defer p.inFlightMu.Unlock()
	if c, exists := p.inFlight[id]; exists {
		return c
	}
	var val int64
	p.inFlight[id] = &val
	return &val
}

// InFlightFor retorna o contador de requisições em voo para um InstanceID.
// Retorna nil se o target não existe (útil para autoscaler).
func (p *ProxyServer) InFlightFor(instanceID string) *int64 {
	p.inFlightMu.RLock()
	defer p.inFlightMu.RUnlock()
	return p.inFlight[instanceID]
}

// isHealthy reporta se o target está saudável (targets sem health-check são
// sempre considerados saudáveis).
func (p *ProxyServer) isHealthy(id string) bool {
	p.healthMu.RLock()
	defer p.healthMu.RUnlock()
	h, ok := p.healthy[id]
	return !ok || h
}

// StartHealthChecks roda o loop de health-check em background. Targets
// locais com HealthPath são sondados a cada 15s: 2 falhas consecutivas =
// removido do pool; 2 sucessos consecutivos = volta.
func (p *ProxyServer) StartHealthChecks(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.runHealthCheckRound()
			}
		}
	}()
}

func (p *ProxyServer) runHealthCheckRound() {
	p.targetsMu.RLock()
	var localTargets []Target
	for _, t := range p.targets {
		if t.Type == TargetTypeLocal && t.HealthPath != "" && t.LocalPort > 0 {
			localTargets = append(localTargets, t)
		}
	}
	p.targetsMu.RUnlock()

	for _, t := range localTargets {
		go func(t Target) {
			healthyNow := pingHealth(t.LocalPort, t.HealthPath)
			wasHealthy := p.isHealthy(t.InstanceID)

			// Sonda mais uma vez para confirmar transição (histerese)
			if healthyNow != wasHealthy {
				healthyNow = pingHealth(t.LocalPort, t.HealthPath)
			}

			p.healthMu.Lock()
			p.healthy[t.InstanceID] = healthyNow
			p.healthMu.Unlock()

			if wasHealthy && !healthyNow {
				log.Printf("[proxy] target %s (model %s) is UNHEALTHY, removing from pool", t.InstanceID, t.Model)
			} else if !wasHealthy && healthyNow {
				log.Printf("[proxy] target %s (model %s) recovered, back in pool", t.InstanceID, t.Model)
			}
		}(t)
	}
}

func pingHealth(port int, healthPath string) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	urlStr := fmt.Sprintf("http://127.0.0.1:%d%s", port, healthPath)
	resp, err := client.Get(urlStr)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500 // 5xx = engine doente
}

// selectTarget escolhe o backend para uma requisição.
// Ordem de matching:
//  1. Se algum target tem Routes e o path bate → pool por rota
//  2. Senão, pool por modelo (t.Model == model, ou model vazio → todos)
//  3. Se model informado e nada bate → erro (404 no handler)
//
// Nunca mais faz fallback para targets[0] de modelo errado.
func (p *ProxyServer) selectTarget(model string, path string) (Target, error) {
	p.targetsMu.RLock()
	all := make([]Target, len(p.targets))
	copy(all, p.targets)
	p.targetsMu.RUnlock()

	// 1. Prioridade: rota por path (stack multimodal)
	var routeMatches []Target
	for _, t := range all {
		if t.matchesPath(path) && p.isHealthy(t.InstanceID) {
			routeMatches = append(routeMatches, t)
		}
	}
	if len(routeMatches) > 0 {
		return p.pick(routeMatches, model, model), nil
	}

	// 2. Match por modelo
	var matching []Target
	for _, t := range all {
		if model == "" || t.Model == model || t.Model == "" {
			if p.isHealthy(t.InstanceID) {
				matching = append(matching, t)
			}
		}
	}

	if len(matching) == 0 {
		if model == "" {
			return Target{}, fmt.Errorf("no active instances available")
		}
		return Target{}, fmt.Errorf("model %q not found", model)
	}

	return p.pick(matching, model, model), nil
}

// pick aplica a estratégia de balanceamento sobre o pool candidato.
func (p *ProxyServer) pick(pool []Target, model string, _ string) Target {
	if len(pool) == 1 {
		return pool[0]
	}

	if p.strategy == "least-connections" {
		var best Target
		var bestCount int64 = -1
		for _, t := range pool {
			var c int64
			if ptr := p.getInFlight(t.InstanceID); ptr != nil {
				c = atomic.LoadInt64(ptr)
			}
			if bestCount == -1 || c < bestCount {
				best = t
				bestCount = c
			}
		}
		return best
	}

	// round-robin entre candidatos do mesmo modelo
	counter := p.getCounter(model)
	idx := atomic.AddUint64(counter, 1) - 1
	return pool[idx%uint64(len(pool))]
}

func (p *ProxyServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/tags", p.handleTags)
	mux.HandleFunc("/api/generate", p.handleProxy)
	mux.HandleFunc("/api/chat", p.handleProxy)
	mux.HandleFunc("/api/embeddings", p.handleProxy)
	mux.HandleFunc("/v1/chat/completions", p.handleProxy) // OpenAI compat
	mux.HandleFunc("/", p.handleProxy)

	p.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", p.localProxyPort),
		Handler: mux,
	}

	log.Printf("Starting Ollama proxy server on :%d...", p.localProxyPort)

	go func() {
		<-ctx.Done()
		p.server.Shutdown(context.Background())
	}()

	err := p.server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// ============================================================
// Handler: /api/tags — lista modelos de todos os backends
// ============================================================

type ModelItem struct {
	Name       string `json:"name"`
	Model      string `json:"model"`
	ModifiedAt string `json:"modified_at,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Digest     string `json:"digest,omitempty"`
}

type TagsResponse struct {
	Models []ModelItem `json:"models"`
}

func (p *ProxyServer) handleTags(w http.ResponseWriter, r *http.Request) {
	p.targetsMu.RLock()
	targetsCopy := make([]Target, len(p.targets))
	copy(targetsCopy, p.targets)
	p.targetsMu.RUnlock()

	if len(targetsCopy) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(TagsResponse{Models: []ModelItem{}})
		return
	}

	// Consulta paralela aos backends com timeout curto por target
	type result struct {
		items []ModelItem
	}
	results := make(chan result, len(targetsCopy))
	var wg sync.WaitGroup

	for _, t := range targetsCopy {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			var items []ModelItem
			switch t.Type {
			case TargetTypeLocal:
				client := &http.Client{Timeout: 2 * time.Second}
				urlStr := fmt.Sprintf("http://127.0.0.1:%d/api/tags", t.LocalPort)
				resp, err := client.Get(urlStr)
				if err != nil {
					if t.Model != "" {
						items = append(items, ModelItem{Name: t.Model, Model: t.Model})
					}
					results <- result{items}
					return
				}
				var tagsResp TagsResponse
				if err := json.NewDecoder(resp.Body).Decode(&tagsResp); err == nil {
					items = tagsResp.Models
				}
				resp.Body.Close()
				if len(items) == 0 && t.Model != "" {
					items = append(items, ModelItem{Name: t.Model, Model: t.Model})
				}
			case TargetTypeRemote:
				if t.Model != "" {
					items = append(items, ModelItem{Name: t.Model, Model: t.Model})
				}
			}
			results <- result{items}
		}(t)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var mergedModels []ModelItem
	seen := make(map[string]bool)
	for res := range results {
		for _, m := range res.items {
			if !seen[m.Name] {
				mergedModels = append(mergedModels, m)
				seen[m.Name] = true
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(TagsResponse{Models: mergedModels})
}

// ============================================================
// Handler: proxy genérico — roteia para local ou remoto
// ============================================================

type RequestModelBody struct {
	Model string `json:"model"`
}

func (p *ProxyServer) handleProxy(w http.ResponseWriter, r *http.Request) {
	var modelName string

	if r.Method == http.MethodPost {
		bodyBytes, err := ioutil.ReadAll(r.Body)
		if err == nil {
			r.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
			var reqBody RequestModelBody
			if err := json.Unmarshal(bodyBytes, &reqBody); err == nil {
				modelName = reqBody.Model
			}
		}
	}

	if modelName == "" {
		modelName = r.URL.Query().Get("model")
	}

	target, err := p.selectTarget(modelName, r.URL.Path)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(err.Error(), "model ") && strings.Contains(err.Error(), "not found"):
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":     err.Error(),
				"available": p.AvailableModels(),
			})
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":     err.Error(),
				"available": p.AvailableModels(),
				"hint":      "Start a deployment with: openllm deploy",
			})
		}
		return
	}

	switch target.Type {
	case TargetTypeLocal:
		p.proxyLocal(w, r, target)
	case TargetTypeRemote:
		p.proxyRemote(w, r, target)
	default:
		http.Error(w, "Unknown target type", http.StatusInternalServerError)
	}
}

// proxyLocal faz reverse proxy para uma instância local (via túnel SSH).
func (p *ProxyServer) proxyLocal(w http.ResponseWriter, r *http.Request, t Target) {
	targetURL, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", t.LocalPort))
	if err != nil {
		http.Error(w, "Invalid target URL", http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.FlushInterval = 100 * time.Millisecond

	// least-connections: contabiliza requisição em voo
	var counter *int64
	if p.strategy == "least-connections" {
		counter = p.getInFlight(t.InstanceID)
		atomic.AddInt64(counter, 1)
	}
	defer func() {
		if counter != nil {
			atomic.AddInt64(counter, -1)
		}
	}()

	proxy.ServeHTTP(w, r)
}

// proxyRemote encaminha a requisição para uma API externa (OpenRouter, etc.)
// injetando os headers de autenticação necessários.
func (p *ProxyServer) proxyRemote(w http.ResponseWriter, r *http.Request, t Target) {
	// Contabiliza requisição em voo (para least-connections)
	counter := p.getInFlight(t.InstanceID)
	atomic.AddInt64(counter, 1)
	defer atomic.AddInt64(counter, -1)

	// Monta a URL de destino: baseURL + path original
	targetURL := t.BaseURL + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	// Lê o body original
	bodyBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}

	// Cria novo request para o provider remoto
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewBuffer(bodyBytes))
	if err != nil {
		http.Error(w, "Failed to create upstream request", http.StatusInternalServerError)
		return
	}

	// Copia headers originais relevantes
	for _, h := range []string{"Content-Type", "Accept"} {
		if v := r.Header.Get(h); v != "" {
			outReq.Header.Set(h, v)
		}
	}

	// Injeta headers de autenticação do provider
	for k, v := range t.AuthHeaders {
		outReq.Header.Set(k, v)
	}

	// Garante Content-Type JSON se não definido
	if outReq.Header.Get("Content-Type") == "" {
		outReq.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{
		Timeout: 5 * time.Minute, // Timeout generoso para chamadas de inferência longas
	}

	resp, err := client.Do(outReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("Upstream error: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Mapeia erros comuns do OpenRouter para mensagens amigáveis
	switch resp.StatusCode {
	case http.StatusPaymentRequired: // 402
		http.Error(w, "OpenRouter: insufficient credits. Add credits at https://openrouter.ai/credits", http.StatusPaymentRequired)
		return
	case http.StatusTooManyRequests: // 429
		w.Header().Set("Retry-After", resp.Header.Get("Retry-After"))
		http.Error(w, "OpenRouter: rate limit exceeded. Please retry after a moment.", http.StatusTooManyRequests)
		return
	}

	// Copia headers da resposta
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Streaming: copia a resposta diretamente para o cliente com flush imediato
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			log.Printf("proxy: error reading upstream response: %v", readErr)
			break
		}
	}
}