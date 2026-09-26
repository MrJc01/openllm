package providers

import (
	"context"
	"fmt"
	"sync"
)

// ============================================================
// Tipos comuns
// ============================================================

// SearchRequest é usado por ComputeProviders para buscar máquinas GPU
type SearchRequest struct {
	MinVRAM  float64
	MinTPS   float64
	Metric   string // "tps", "itps", "fps", "rtf" — usado p/ estimar se a GPU atende
	GPUCount int
	Model    string
}

// Machine representa uma oferta de GPU disponível para aluguel
type Machine struct {
	ID             string  `json:"id"`
	Provider       string  `json:"provider"`
	GPU            string  `json:"gpu"`
	VRAM           float64 `json:"vram"`       // VRAM por GPU em GB
	GPUCount       int     `json:"gpu_count"`
	CostPerHour    float64 `json:"cost_per_hour"`
	EstimatedTPS   float64 `json:"estimated_tps"`     // compat: texto (tokens/s)
	EstimatedMetric float64 `json:"estimated_metric"` // genérico: tps/itps/fps/rtf
	Metric          string  `json:"metric,omitempty"`  // qual métrica o EstimatedMetric representa
	Location       string  `json:"location"`
	MaxBid         float64 `json:"max_bid,omitempty"`
	DirectConfig   string  `json:"direct_config,omitempty"`
	// NetMbps é a banda de rede medida do host (inet_down). Desempata ofertas
	// de preço parecido: banda baixa = deploy lento (pull de imagem) e
	// download de modelo lento.
	NetMbps float64 `json:"net_mbps,omitempty"`
}

// DeployRequest é usado por ComputeProviders para alugar uma instância
type DeployRequest struct {
	MachineID  string
	Model      string
	Image      string
	OnstartCmd string
	APIKey     string
	Engine     string // "ollama" (default), "localai"
	// Env são variáveis de ambiente repassadas ao provedor (ex: Vast.ai
	// aceita "env" no PUT /asks — permite PROVISIONING_SCRIPT, HF_TOKEN,
	// etc. sem mexer no modelo SSH + watchdog). Vem de Definition.Env.
	Env map[string]string
	// DiskGB é o tamanho de disco (GB) solicitado ao provedor.
	// 0 = usa o default do provedor (35GB no Vast.ai).
	DiskGB float64
}

// InstanceInfo representa uma instância de GPU em execução
type InstanceInfo struct {
	ID          string  `json:"id"`
	Provider    string  `json:"provider"`
	MachineID   string  `json:"machine_id"`
	GPU         string  `json:"gpu"`
	GPUCount    int     `json:"gpu_count"`
	VRAM        float64 `json:"vram"`
	CostPerHour float64 `json:"cost_per_hour"`
	SSHHost     string  `json:"ssh_host"`
	SSHPort     int     `json:"ssh_port"`
	Status      string  `json:"status"` // "deploying", "running", "stopped", "failed"
	StatusMsg   string  `json:"status_msg,omitempty"`
	// Label identifica a instância no provedor. Instâncias do openllm usam
	// o prefixo "openllm" — usado como trava de segurança contra destruir
	// instâncias de outros projetos na mesma conta.
	Label string `json:"label,omitempty"`
}

// ModelSearchRequest é usado por InferenceProviders para buscar modelos disponíveis
type ModelSearchRequest struct {
	Query              string
	SortBy             string  // "price", "throughput", "newest"
	MaxPricePerMToken  float64 // Filtro máximo de preço por 1M tokens (0 = sem limite)
}

// InferenceModel representa um modelo disponível em um InferenceProvider
type InferenceModel struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	ProviderName    string   `json:"provider_name"`
	ContextLength   int      `json:"context_length"`
	PricePerMInput  float64  `json:"price_per_m_input"`  // USD por 1M tokens de input
	PricePerMOutput float64  `json:"price_per_m_output"` // USD por 1M tokens de output
	SupportedParams []string `json:"supported_params"`
	IsFree          bool     `json:"is_free"`
}

// ============================================================
// Interfaces
// ============================================================

// ComputeProvider representa um provedor de infraestrutura GPU (Vast.ai, RunPod, etc.)
// Você aluga hardware, instala Ollama e cria túnel SSH.
type ComputeProvider interface {
	Name() string
	Search(ctx context.Context, req SearchRequest) ([]Machine, error)
	Deploy(ctx context.Context, req DeployRequest) (*InstanceInfo, error)
	Destroy(ctx context.Context, instanceID string, apiKey string) error
	Pause(ctx context.Context, instanceID string, apiKey string) error
	Resume(ctx context.Context, instanceID string, apiKey string) error
	GetStatus(ctx context.Context, instanceID string, apiKey string) (*InstanceInfo, error)
}

// InferenceProvider representa um provedor de inferência via API (OpenRouter, Together.ai, etc.)
// Você chama uma API externa com um Bearer token — sem GPU, sem SSH.
type InferenceProvider interface {
	Name() string
	// SearchModels busca modelos disponíveis com estimativa de custo
	SearchModels(ctx context.Context, req ModelSearchRequest) ([]InferenceModel, error)
	// BaseURL retorna a URL base para o proxy redirecionar as requisições
	BaseURL() string
	// AuthHeaders retorna os headers de autenticação a injetar nas requisições
	AuthHeaders(apiKey string) map[string]string
	// EstimateCost calcula o custo em USD para N tokens de input + output
	EstimateCost(modelID string, inputTokens, outputTokens int64) float64
}

// ============================================================
// Registry global de providers
// ============================================================

var (
	mu                 sync.RWMutex
	computeProviders   = map[string]ComputeProvider{}
	inferenceProviders = map[string]InferenceProvider{}
)

// RegisterCompute registra um ComputeProvider no registry global.
// Deve ser chamado no init() de cada package de provider.
func RegisterCompute(p ComputeProvider) {
	mu.Lock()
	defer mu.Unlock()
	computeProviders[p.Name()] = p
}

// RegisterInference registra um InferenceProvider no registry global.
// Deve ser chamado no init() de cada package de provider.
func RegisterInference(p InferenceProvider) {
	mu.Lock()
	defer mu.Unlock()
	inferenceProviders[p.Name()] = p
}

// GetCompute retorna um ComputeProvider pelo nome.
func GetCompute(name string) (ComputeProvider, bool) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := computeProviders[name]
	return p, ok
}

// GetInference retorna um InferenceProvider pelo nome.
func GetInference(name string) (InferenceProvider, bool) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := inferenceProviders[name]
	return p, ok
}

// ListCompute retorna os nomes de todos os ComputeProviders registrados.
func ListCompute() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(computeProviders))
	for name := range computeProviders {
		names = append(names, name)
	}
	return names
}

// ListInference retorna os nomes de todos os InferenceProviders registrados.
func ListInference() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(inferenceProviders))
	for name := range inferenceProviders {
		names = append(names, name)
	}
	return names
}

// IsCompute verifica se o nome dado corresponde a um ComputeProvider.
func IsCompute(name string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := computeProviders[name]
	return ok
}

// IsInference verifica se o nome dado corresponde a um InferenceProvider.
func IsInference(name string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := inferenceProviders[name]
	return ok
}

// ProviderType retorna o tipo ("compute", "inference", ou erro se não encontrado)
func ProviderType(name string) (string, error) {
	mu.RLock()
	defer mu.RUnlock()
	if _, ok := computeProviders[name]; ok {
		return "compute", nil
	}
	if _, ok := inferenceProviders[name]; ok {
		return "inference", nil
	}
	return "", fmt.Errorf("unknown provider: %q. Available compute: %v, inference: %v",
		name, listKeys(computeProviders), listKeys(inferenceProviders))
}

func listKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
