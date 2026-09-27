package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/crom-org/openllm/internal/config"
	"github.com/crom-org/openllm/internal/engines"
	"github.com/crom-org/openllm/internal/httpsec"
	"github.com/crom-org/openllm/internal/models"
	"github.com/crom-org/openllm/internal/providers"
	"github.com/crom-org/openllm/internal/proxy"
	"github.com/crom-org/openllm/internal/stack"
	"github.com/crom-org/openllm/internal/storage"
)

type ControlServer struct {
	port    int
	manager *InstanceManager
	db      *storage.DB
	server  *http.Server
	guard   *httpsec.Guard
}

func NewControlServer(port int, manager *InstanceManager, db *storage.DB) *ControlServer {
	return &ControlServer{
		port:    port,
		manager: manager,
		db:      db,
	}
}

// Handler devolve as rotas da API já protegidas pelo guard.
func (s *ControlServer) Handler() (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/search", s.handleSearch)
	mux.HandleFunc("/deploy", s.handleDeploy)
	mux.HandleFunc("/scale", s.handleScale)
	mux.HandleFunc("/swap", s.handleSwap)
	mux.HandleFunc("/models/add", s.handleAddModel)
	mux.HandleFunc("/catalog", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(models.CatalogByEngine())
	})
	mux.HandleFunc("/models/remove", s.handleRemoveModel)
	mux.HandleFunc("/stack", s.handleStack)   // POST = up, DELETE/POST down = down
	mux.HandleFunc("/stacks", s.handleStacks) // GET = lista stacks
	mux.HandleFunc("/stop", s.handleStop)     // acts as destroy
	mux.HandleFunc("/pause", s.handlePause)
	mux.HandleFunc("/resume", s.handleResume)
	mux.HandleFunc("/logs", s.handleLogs)

	if s.guard == nil || s.guard.Token == "" {
		return nil, fmt.Errorf("control API: refusing to start without an API token (%s)", httpsec.TokenEnv)
	}
	return s.guard.Wrap(mux), nil
}

func (s *ControlServer) Start(ctx context.Context) error {
	h, err := s.Handler()
	if err != nil {
		return err
	}
	s.server = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", s.port),
		Handler: h,
	}

	log.Printf("Starting openllmd control API on http://127.0.0.1:%d...", s.port)

	go func() {
		<-ctx.Done()
		s.server.Shutdown(context.Background())
	}()

	err = s.server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

type InstanceStatusResponse struct {
	ID          string   `json:"id"`
	MachineID   string   `json:"machine_id"`
	GPU         string   `json:"gpu"`
	GPUCount    int      `json:"gpu_count"`
	VRAM        float64  `json:"vram"`
	CostPerHour float64  `json:"cost_per_hour"`
	SSHHost     string   `json:"ssh_host"`
	SSHPort     int      `json:"ssh_port"`
	Status      string   `json:"status"`
	Model       string   `json:"model"`
	Engine      string   `json:"engine"`
	GroupID     string   `json:"group_id,omitempty"`
	LocalPort   int      `json:"local_port,omitempty"`
	Models      []string `json:"models,omitempty"` // principal + extras em execução
	// ModelStates: "loading" | "ready" | "failed" por modelo (inclui o alvo de swap/add).
	ModelStates map[string]string `json:"model_states,omitempty"`
	// ModelProgress: progresso do download por modelo ("45% · 2763M de 6046M · ...").
	ModelProgress map[string]string `json:"model_progress,omitempty"`
	// ModelDetails: motivo da falha por modelo (add/swap/reconcile).
	ModelDetails map[string]string `json:"model_details,omitempty"`
	LastPing     string            `json:"last_ping,omitempty"`
	TimeActive   string            `json:"time_active"`
}

type SearchRequestPayload struct {
	Model  string `json:"model"`
	Engine string `json:"engine,omitempty"`
	// ExcludeMachineIDs: ofertas ou host_ids que já falharam neste ciclo (ex: sem acesso ao registry).
	ExcludeMachineIDs []string `json:"exclude_machine_ids,omitempty"`
	// Limit > 1: também devolve as N melhores ofertas em "offers" (uma por host).
	Limit int `json:"limit,omitempty"`
	// MinVRAM: pede placas maiores que o mínimo do modelo (faixa escolhida no painel).
	MinVRAM float64 `json:"min_vram,omitempty"`
	// GPUName: só esta placa (ex: "H100 SXM").
	GPUName string `json:"gpu_name,omitempty"`
	// NumGPUs: nº exato de placas (escolhido na lista de placas do painel).
	NumGPUs int `json:"num_gpus,omitempty"`
}

type SearchResponsePayload struct {
	MachineID   string  `json:"machine_id"`
	GPU         string  `json:"gpu"`
	VRAM        float64 `json:"vram"`
	GPUCount    int     `json:"gpu_count"`
	CostPerHour float64 `json:"cost_per_hour"`
	Location    string  `json:"location"`
	NetMbps     float64 `json:"net_mbps"`
	HostID      string  `json:"host_id,omitempty"`
	// Offers: a melhor primeiro, depois as mais baratas (só com limit > 1).
	Offers []SearchResponsePayload `json:"offers,omitempty"`
}

func (s *ControlServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload SearchRequestPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if payload.Model == "" {
		http.Error(w, "model is required", http.StatusBadRequest)
		return
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load local config openllm.json: %v", err), http.StatusInternalServerError)
		return
	}
	if err := cfg.Validate(); err != nil {
		http.Error(w, fmt.Sprintf("invalid configuration: %v", err), http.StatusBadRequest)
		return
	}

	provType, err := providers.ProviderType(cfg.Provider)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid provider: %v", err), http.StatusBadRequest)
		return
	}
	if provType != "compute" {
		http.Error(w, "search requires a compute provider (e.g. vastai)", http.StatusBadRequest)
		return
	}

	// Engine padrão: catálogo → ollama
	engine := payload.Engine
	if engine == "" {
		if entry, ok := models.ResolveCatalog(payload.Model); ok {
			engine = entry.Engine
		} else {
			engine = "ollama"
		}
	}

	// VRAM do catálogo ou engine
	targetVram := engines.VRAMFor(engine, payload.Model)
	if entry, ok := models.ResolveCatalog(payload.Model); ok && entry.VRAMGB > 0 {
		targetVram = entry.VRAMGB
	}

	if payload.MinVRAM > targetVram {
		targetVram = payload.MinVRAM
	}

	// TPS só para texto
	targetTps := 0.0
	if entry, ok := models.ResolveCatalog(payload.Model); ok && entry.Modality == "text" {
		targetTps = cfg.TpsTarget
	}

	client, ok := providers.GetCompute(cfg.Provider)
	if !ok {
		http.Error(w, fmt.Sprintf("provider %s is not a compute provider", cfg.Provider), http.StatusInternalServerError)
		return
	}

	results, err := client.Search(r.Context(), providers.SearchRequest{
		MinVRAM:   targetVram,
		MinTPS:    targetTps,
		Metric:    getMetricNameForModel(payload.Model),
		Model:     payload.Model,
		MinCUDA:   engines.Get(engine).MinCUDA,
		MinDiskGB: engines.Get(engine).DiskGB,
		// Só vLLM e ollama dividem o modelo entre GPUs; as demais (ComfyUI,
		// servidores de áudio) usam uma placa: a VRAM tem que caber nela.
		GPUCount: gpuCount(engine, payload.NumGPUs),
		GPUName:  payload.GPUName,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("search failed: %v", err), http.StatusInternalServerError)
		return
	}

	payload.ExcludeMachineIDs = append(payload.ExcludeMachineIDs, s.manager.BadHosts()...)
	if len(payload.ExcludeMachineIDs) > 0 {
		excluded := make(map[string]bool, len(payload.ExcludeMachineIDs))
		for _, id := range payload.ExcludeMachineIDs {
			excluded[id] = true
		}
		kept := results[:0]
		for _, m := range results {
			if !excluded[m.ID] && (m.HostID == "" || !excluded[m.HostID]) {
				kept = append(kept, m)
			}
		}
		results = kept
	}

	if len(results) == 0 {
		http.Error(w, "no available GPU machines satisfy the requirements", http.StatusNotFound)
		return
	}

	// Usa PickBestMachine do daemon (mesma lógica do scale)
	best := PickBestMachine(results)
	toPayload := func(m providers.Machine) SearchResponsePayload {
		return SearchResponsePayload{
			MachineID:   m.ID,
			GPU:         m.GPU,
			VRAM:        m.VRAM,
			GPUCount:    m.GPUCount,
			CostPerHour: m.CostPerHour,
			Location:    m.Location,
			NetMbps:     m.NetMbps,
			HostID:      m.HostID,
		}
	}
	resp := toPayload(best)
	if payload.Limit > 1 {
		resp.Offers = topOffers(best, results, payload.Limit, toPayload)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *ControlServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Obtém instâncias ativas do manager de memória
	actives := s.manager.GetActiveInstances()
	activeMap := make(map[string]ActiveInstance)
	for _, act := range actives {
		activeMap[act.ID] = act
	}

	// 2. Obtém instâncias recentes do SQLite
	allInstances, err := s.db.ListAllInstances()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to query database: %v", err), http.StatusInternalServerError)
		return
	}

	// Conjunto de modelos persistido (fonte de verdade entre restarts).
	persisted, err := s.db.ListAllInstanceModels()
	if err != nil {
		log.Printf("status: failed to load persisted models: %v", err)
	}

	dbIDs := map[string]bool{}
	for _, inst := range allInstances {
		dbIDs[inst.ID] = true
	}

	// 3. Ativos em memória que não estão no DB (ex: row deletada por falso
	// negativo da API) continuam visíveis no status — nada some do radar.
	for _, act := range actives {
		if dbIDs[act.ID] {
			continue
		}
		allInstances = append(allInstances, act.Instance)
	}

	var resp []InstanceStatusResponse
	for _, inst := range allInstances {
		statusResp := InstanceStatusResponse{
			ID:          inst.ID,
			MachineID:   inst.MachineID,
			GPU:         inst.GPU,
			GPUCount:    inst.GPUCount,
			VRAM:        inst.VRAM,
			CostPerHour: inst.CostPerHour,
			SSHHost:     inst.SSHHost,
			SSHPort:     inst.SSHPort,
			Status:      inst.Status,
			Model:       inst.Model,
			Engine:      inst.Engine,
			GroupID:     inst.GroupID,
		}

		// Calcula tempo ativo
		var duration time.Duration
		if inst.StoppedAt != nil {
			duration = inst.StoppedAt.Sub(inst.CreatedAt)
		} else {
			duration = time.Since(inst.CreatedAt)
		}
		statusResp.TimeActive = duration.Round(time.Second).String()

		// Modelos/estados: banco + estado vivo (memória vence, é mais recente)
		act, activeExists := activeMap[inst.ID]
		var live *ActiveInstance
		if activeExists {
			live = &act
		}
		statusResp.Models, statusResp.ModelStates, statusResp.ModelDetails = mergeModelStatus(inst, persisted[inst.ID], live)

		// Se está ativa em memória, adiciona dados dinâmicos do túnel e heartbeat
		if activeExists {
			statusResp.LocalPort = act.LocalPort
			statusResp.ModelProgress = copyMap(act.ModelProgress)
			if pingTime, pingExists := s.manager.GetLastPing(inst.ID); pingExists {
				statusResp.LastPing = time.Since(pingTime).Round(time.Second).String() + " ago"
			} else {
				statusResp.LastPing = "never"
			}
		}

		resp = append(resp, statusResp)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ControlMaxBody: limite de corpo das requisições da API de controle.
const ControlMaxBody = 1 << 20

// SetGuard define a proteção (token/Host/Origin/JSON/limite) da API.
func (s *ControlServer) SetGuard(g *httpsec.Guard) { s.guard = g }

type DeployRequestPayload struct {
	MachineID    string   `json:"machine_id"`
	Model        string   `json:"model"`
	Engine       string   `json:"engine"`
	Capabilities []string `json:"capabilities,omitempty"`
	CustomImage  string   `json:"custom_image,omitempty"`
	CustomCmd    string   `json:"custom_cmd,omitempty"`
	CustomPort   int      `json:"custom_port,omitempty"`
	// MaxCostPerHour: teto de preço; o preço atual da oferta é reconferido
	// imediatamente antes do aluguel (tolerância de 5%). 0 = sem teto.
	MaxCostPerHour float64 `json:"max_cost_per_hour,omitempty"`
}

func (s *ControlServer) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload DeployRequestPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if payload.MachineID == "" || payload.Model == "" {
		http.Error(w, "machine_id and model are required", http.StatusBadRequest)
		return
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load local config openllm.json: %v", err), http.StatusInternalServerError)
		return
	}

	if err := cfg.Validate(); err != nil {
		http.Error(w, fmt.Sprintf("invalid configuration: %v", err), http.StatusBadRequest)
		return
	}

	provType, err := providers.ProviderType(cfg.Provider)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid provider: %v", err), http.StatusBadRequest)
		return
	}

	if provType == "compute" {
		if payload.MachineID == "" || payload.Model == "" {
			http.Error(w, "machine_id and model are required for compute provider", http.StatusBadRequest)
			return
		}

		instanceID, err := s.manager.DeployInstance(cfg, payload)
		if errors.Is(err, providers.ErrPriceAboveMax) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if errors.Is(err, ErrCustomDisabled) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to deploy: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"success":     "true",
			"instance_id": instanceID,
			"status":      "deploying",
		})
	} else {
		// Inference provider (ex: openrouter)
		if payload.Model == "" {
			http.Error(w, "model is required for inference provider", http.StatusBadRequest)
			return
		}

		p, ok := providers.GetInference(cfg.Provider)
		if !ok {
			http.Error(w, fmt.Sprintf("inference provider %s not found", cfg.Provider), http.StatusInternalServerError)
			return
		}

		instanceID := "remote-" + cfg.Provider

		// Salva no banco de dados SQLite para aparecer no 'status'
		inst := storage.Instance{
			ID:          instanceID,
			Provider:    cfg.Provider,
			MachineID:   "API",
			Status:      "running",
			Model:       payload.Model,
			CreatedAt:   time.Now(),
			GPU:         "Hosted API",
			GPUCount:    1,
			VRAM:        0,
			CostPerHour: 0,
		}
		_ = s.db.SaveInstance(&inst)

		// Registra no proxy (merge — preserva backends já existentes)
		s.manager.proxyServer.AddTarget(proxy.Target{
			InstanceID:  instanceID,
			Type:        proxy.TargetTypeRemote,
			Model:       payload.Model,
			BaseURL:     p.BaseURL(),
			AuthHeaders: p.AuthHeaders(cfg.ActiveAPIKey()),
		})

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"success":     "true",
			"instance_id": instanceID,
			"status":      "running",
		})
	}
}

type StopRequestPayload struct {
	InstanceID string `json:"instance_id"`
}

func (s *ControlServer) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload StopRequestPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if payload.InstanceID == "" {
		http.Error(w, "instance_id is required", http.StatusBadRequest)
		return
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load local config: %v", err), http.StatusInternalServerError)
		return
	}

	err = s.manager.StopInstance(cfg, payload.InstanceID)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to stop instance: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "stopped",
		"instance_id": payload.InstanceID,
		"success":     "true",
	})
}

func (s *ControlServer) handlePause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload StopRequestPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if payload.InstanceID == "" {
		http.Error(w, "instance_id is required", http.StatusBadRequest)
		return
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load local config: %v", err), http.StatusInternalServerError)
		return
	}

	err = s.manager.PauseInstance(cfg, payload.InstanceID)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to pause instance: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "stopped",
		"instance_id": payload.InstanceID,
		"success":     "true",
	})
}

func (s *ControlServer) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload StopRequestPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if payload.InstanceID == "" {
		http.Error(w, "instance_id is required", http.StatusBadRequest)
		return
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load local config: %v", err), http.StatusInternalServerError)
		return
	}

	err = s.manager.ResumeInstance(cfg, payload.InstanceID)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to resume instance: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "deploying",
		"instance_id": payload.InstanceID,
		"success":     "true",
	})
}

func (s *ControlServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	instanceID := r.URL.Query().Get("instance_id")
	if instanceID == "" {
		http.Error(w, "instance_id is required", http.StatusBadRequest)
		return
	}

	logs := s.manager.GetLogs(instanceID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"instance_id": instanceID,
		"logs":        logs,
	})
}

// ============================================================
// Escala horizontal: POST /scale
// ============================================================

type ScaleRequestPayload struct {
	Model          string  `json:"model"`
	Engine         string  `json:"engine,omitempty"`
	Instances      int     `json:"instances"`
	MaxCostPerHour float64 `json:"max_cost_hour,omitempty"`
}

type ScaleResponsePayload struct {
	Success     bool     `json:"success"`
	GroupID     string   `json:"group_id"`
	Current     int      `json:"current"`
	Created     []string `json:"created,omitempty"`
	Removed     []string `json:"removed,omitempty"`
	InstanceIDs []string `json:"instance_ids,omitempty"`
}

func (s *ControlServer) handleScale(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload ScaleRequestPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if payload.Model == "" || payload.Instances < 1 {
		http.Error(w, "model and instances (>= 1) are required", http.StatusBadRequest)
		return
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load local config openllm.json: %v", err), http.StatusInternalServerError)
		return
	}
	if err := cfg.Validate(); err != nil {
		http.Error(w, fmt.Sprintf("invalid configuration: %v", err), http.StatusBadRequest)
		return
	}
	if provType, err := providers.ProviderType(cfg.Provider); err != nil || provType != "compute" {
		http.Error(w, "scale requires a compute provider (e.g. vastai)", http.StatusBadRequest)
		return
	}

	// Engine padrão: catálogo → ollama
	engine := payload.Engine
	if engine == "" {
		if entry, ok := models.ResolveCatalog(payload.Model); ok {
			engine = entry.Engine
		} else {
			engine = "ollama"
		}
	}

	result, err := s.manager.ScaleGroup(cfg, engine, payload.Model, payload.Instances, payload.MaxCostPerHour)
	if err != nil {
		http.Error(w, fmt.Sprintf("scale failed: %v", err), http.StatusInternalServerError)
		return
	}

	// Lista completa de instâncias ativas do grupo após a operação
	active, _ := s.manager.activeGroupInstances(result.GroupID)
	var ids []string
	for _, inst := range active {
		ids = append(ids, inst.ID)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ScaleResponsePayload{
		Success:     true,
		GroupID:     result.GroupID,
		Current:     result.Current,
		Created:     result.Created,
		Removed:     result.Removed,
		InstanceIDs: ids,
	})
}

// ============================================================
// Stacks multimodais: POST /stack (up), POST /stack down (down), GET /stacks
// ============================================================

type StackUpPayload struct {
	Name string `json:"name,omitempty"`
	YAML string `json:"yaml"`
}

func (s *ControlServer) handleStack(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/down"):
		s.handleStackDown(w, r)
	case r.Method == http.MethodPost:
		s.handleStackUp(w, r)
	case r.Method == http.MethodGet:
		s.handleStacks(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *ControlServer) handleStackUp(w http.ResponseWriter, r *http.Request) {
	var payload StackUpPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	st, err := stack.Parse([]byte(payload.YAML), payload.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load local config openllm.json: %v", err), http.StatusInternalServerError)
		return
	}
	if err := cfg.Validate(); err != nil {
		http.Error(w, fmt.Sprintf("invalid configuration: %v", err), http.StatusBadRequest)
		return
	}
	if provType, err := providers.ProviderType(cfg.Provider); err != nil || provType != "compute" {
		http.Error(w, "stacks require a compute provider (e.g. vastai)", http.StatusBadRequest)
		return
	}

	result, err := s.manager.DeployStack(cfg, st)
	if err != nil {
		http.Error(w, fmt.Sprintf("stack deploy failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"stack":   result,
	})
}

type StackDownPayload struct {
	Name string `json:"name"`
}

func (s *ControlServer) handleStackDown(w http.ResponseWriter, r *http.Request) {
	var payload StackDownPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if payload.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load local config: %v", err), http.StatusInternalServerError)
		return
	}

	stopped, err := s.manager.StopStack(cfg, payload.Name)
	if err != nil {
		http.Error(w, fmt.Sprintf("stack down failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":       true,
		"stack":         payload.Name,
		"stopped_count": len(stopped),
		"stopped":       stopped,
	})
}

func (s *ControlServer) handleStacks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	results, err := s.manager.StackStatus()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to list stacks: %v", err), http.StatusInternalServerError)
		return
	}
	if results == nil {
		results = []StackResult{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

// ============================================================
// Swap de modelo: POST /swap
// ============================================================

type SwapRequestPayload struct {
	InstanceID string `json:"instance_id"`
	Model      string `json:"model"`
}

func (s *ControlServer) handleSwap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload SwapRequestPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if payload.InstanceID == "" || payload.Model == "" {
		http.Error(w, "instance_id and model are required", http.StatusBadRequest)
		return
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load local config: %v", err), http.StatusInternalServerError)
		return
	}
	if err := cfg.Validate(); err != nil {
		http.Error(w, fmt.Sprintf("invalid configuration: %v", err), http.StatusBadRequest)
		return
	}

	if err := s.manager.SwapModel(cfg, payload.InstanceID, payload.Model); err != nil {
		http.Error(w, fmt.Sprintf("swap failed: %v", err), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"instance_id": payload.InstanceID,
		"swapping_to": payload.Model,
	})
}

// --- Heartbeat Server ---
type HeartbeatServer struct {
	port    int
	manager *InstanceManager
	server  *http.Server
}

func NewHeartbeatServer(port int, manager *InstanceManager) *HeartbeatServer {
	return &HeartbeatServer{
		port:    port,
		manager: manager,
	}
}

func (s *HeartbeatServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", s.handlePing)

	s.server = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", s.port),
		Handler: mux,
	}

	log.Printf("Starting watchdog heartbeat server on :%d...", s.port)

	go func() {
		<-ctx.Done()
		s.server.Shutdown(context.Background())
	}()

	err := s.server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *HeartbeatServer) handlePing(w http.ResponseWriter, r *http.Request) {
	// Sem token (chega pelo túnel reverso das máquinas alugadas): só GET /ping
	// de uma instância que o daemon conhece.
	if r.Method != http.MethodGet || r.URL.Path != "/ping" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	instanceID := r.URL.Query().Get("instance_id")
	if instanceID == "" {
		http.Error(w, "missing instance_id parameter", http.StatusBadRequest)
		return
	}
	if !s.manager.KnownInstance(instanceID) {
		http.Error(w, "unknown instance", http.StatusNotFound)
		return
	}

	// Registra o ping no manager
	s.manager.RegisterPing(instanceID)

	// Retorna OK para o watchdog remoto saber que estamos vivos
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"alive"}`))
}

// handleAddModel: POST {instance_id, model} — serve um modelo a mais na
// mesma instância, sem trocar o atual.
func (s *ControlServer) handleAddModel(w http.ResponseWriter, r *http.Request) {
	s.handleModelChange(w, r, s.manager.AddModel, "adding")
}

// handleRemoveModel: POST {instance_id, model} — deixa de rotear um extra.
func (s *ControlServer) handleRemoveModel(w http.ResponseWriter, r *http.Request) {
	s.handleModelChange(w, r, s.manager.RemoveModel, "removed")
}

func (s *ControlServer) handleModelChange(w http.ResponseWriter, r *http.Request, fn func(id, model string) error, state string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload SwapRequestPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.InstanceID == "" || payload.Model == "" {
		http.Error(w, "instance_id and model are required", http.StatusBadRequest)
		return
	}
	if err := fn(payload.InstanceID, payload.Model); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"instance_id": payload.InstanceID,
		"model":       payload.Model,
		"state":       state,
	})
}

// singleGPU devolve 1 para engines que usam uma única GPU (a busca então não
// soma a VRAM de várias placas); 0 = sem restrição.
func singleGPU(engine string) int {
	switch engine {
	case "vllm", "ollama":
		return 0
	}
	return 1
}

// topOffers: a melhor oferta e as demais mais baratas, no máximo uma por host
// físico (ofertas do mesmo host costumam falhar juntas).
func topOffers(best providers.Machine, all []providers.Machine, limit int, conv func(providers.Machine) SearchResponsePayload) []SearchResponsePayload {
	sorted := append([]providers.Machine(nil), all...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CostPerHour < sorted[j].CostPerHour })
	seen := map[string]bool{}
	key := func(m providers.Machine) string {
		if m.HostID != "" {
			return "h" + m.HostID
		}
		return "m" + m.ID
	}
	out := []SearchResponsePayload{conv(best)}
	seen[key(best)] = true
	for _, m := range sorted {
		if len(out) >= limit {
			break
		}
		if !seen[key(m)] {
			seen[key(m)] = true
			out = append(out, conv(m))
		}
	}
	return out
}

// gpuCount: o nº pedido no painel vale para vLLM/ollama (dividem o modelo);
// as demais engines usam uma placa só.
func gpuCount(engine string, asked int) int {
	if single := singleGPU(engine); single == 1 || asked <= 0 {
		return single
	}
	return asked
}

// mergeModelStatus combina o conjunto persistido com o estado em memória.
// Models: principal + extras roteáveis (memória, se ativa; senão o banco).
// States/Details: último valor persistido sobreposto pelo vivo.
func mergeModelStatus(inst storage.Instance, rows []storage.InstanceModel, live *ActiveInstance) ([]string, map[string]string, map[string]string) {
	states, details := map[string]string{}, map[string]string{}
	var models []string
	if live != nil {
		models = live.Models()
	} else if len(rows) > 0 {
		models = []string{inst.Model}
	}
	for _, r := range rows {
		if r.State != "" {
			states[r.Model] = r.State
		}
		if r.Detail != "" {
			details[r.Model] = r.Detail
		}
		if live == nil && r.Role == storage.RoleExtra && r.Model != inst.Model {
			models = append(models, r.Model)
		}
	}
	if live != nil {
		for k, v := range live.ModelStates {
			states[k] = v
			if d := live.ModelDetails[k]; d != "" {
				details[k] = d
			} else {
				delete(details, k)
			}
		}
	}
	if len(states) == 0 {
		states = nil
	}
	if len(details) == 0 {
		details = nil
	}
	return models, states, details
}
