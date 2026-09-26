package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/crom-org/openllm/internal/providers"
)

const (
	baseURL = "https://openrouter.ai/api/v1"
	referer = "https://openllm.crom.dev"
	title   = "openllm"
)

// Client implementa InferenceProvider para o OpenRouter.
// Usa HTTP puro (sem SDK) para manter zero dependências extras.
type Client struct {
	httpClient *http.Client
}

func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) Name() string { return "openrouter" }

// BaseURL retorna a URL base para o proxy redirecionar as requisições.
func (c *Client) BaseURL() string { return baseURL }

// AuthHeaders retorna os headers de autenticação + metadados do OpenRouter.
func (c *Client) AuthHeaders(apiKey string) map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + apiKey,
		"HTTP-Referer":  referer,
		"X-Title":       title,
	}
}

// ============================================================
// Tipos internos da API do OpenRouter
// ============================================================

type orModelPricing struct {
	Prompt     string `json:"prompt"`
	Completion string `json:"completion"`
}

type orModel struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	ContextLength int            `json:"context_length"`
	Pricing       orModelPricing `json:"pricing"`
	Architecture  struct {
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
	SupportedParameters []string `json:"supported_parameters"`
}

type orModelsResponse struct {
	Data []orModel `json:"data"`
}

// ============================================================
// SearchModels: busca modelos disponíveis no OpenRouter
// ============================================================

// SearchModels consulta GET /api/v1/models com filtros opcionais.
// Suporta sort por "price", "throughput", "newest" e filtro por MaxPricePerMToken.
func (c *Client) SearchModels(ctx context.Context, req providers.ModelSearchRequest) ([]providers.InferenceModel, error) {
	sortParam := "pricing-low-to-high" // padrão: mais barato primeiro
	switch req.SortBy {
	case "throughput":
		sortParam = "throughput-high-to-low"
	case "newest":
		sortParam = "newest"
	case "price", "":
		sortParam = "pricing-low-to-high"
	}

	url := fmt.Sprintf("%s/models?sort=%s&limit=100", baseURL, sortParam)

	httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openrouter: failed to fetch models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("openrouter: models endpoint returned status %d: %s", resp.StatusCode, string(body))
	}

	var modelsResp orModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		return nil, fmt.Errorf("openrouter: failed to parse models response: %w", err)
	}

	var results []providers.InferenceModel
	for _, m := range modelsResp.Data {
		// Converte preço: a API retorna USD por token individual
		// Multiplicamos por 1.000.000 para exibir por 1M tokens
		inputPerM := parsePrice(m.Pricing.Prompt) * 1_000_000
		outputPerM := parsePrice(m.Pricing.Completion) * 1_000_000

		isFree := strings.HasSuffix(m.ID, ":free") || (inputPerM == 0 && outputPerM == 0)

		// Filtro por query (busca no ID e nome do modelo)
		if req.Query != "" {
			query := strings.ToLower(req.Query)
			if !strings.Contains(strings.ToLower(m.ID), query) &&
				!strings.Contains(strings.ToLower(m.Name), query) {
				continue
			}
		}

		// Filtro por preço máximo (ignora modelos gratuitos no filtro de preço)
		if req.MaxPricePerMToken > 0 && !isFree {
			avgPrice := (inputPerM + outputPerM) / 2
			if avgPrice > req.MaxPricePerMToken {
				continue
			}
		}

		// Extrai nome do provider a partir do ID (ex: "openai/gpt-4o" → "openai")
		providerName := "openrouter"
		if parts := strings.SplitN(m.ID, "/", 2); len(parts) == 2 {
			providerName = parts[0]
		}

		results = append(results, providers.InferenceModel{
			ID:              m.ID,
			Name:            m.Name,
			ProviderName:    providerName,
			ContextLength:   m.ContextLength,
			PricePerMInput:  inputPerM,
			PricePerMOutput: outputPerM,
			SupportedParams: m.SupportedParameters,
			IsFree:          isFree,
		})
	}

	return results, nil
}

// ============================================================
// EstimateCost: calcula custo estimado de uma chamada
// ============================================================

// EstimateCost calcula o custo em USD para inputTokens de prompt + outputTokens de completion.
// Busca os preços do modelo via API. Retorna 0.0 se não encontrar o modelo.
func (c *Client) EstimateCost(modelID string, inputTokens, outputTokens int64) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Busca apenas o modelo específico
	url := fmt.Sprintf("%s/models/%s", baseURL, modelID)
	httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		return 0
	}
	defer resp.Body.Close()

	var m orModel
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return 0
	}

	inputCost := parsePrice(m.Pricing.Prompt) * float64(inputTokens)
	outputCost := parsePrice(m.Pricing.Completion) * float64(outputTokens)
	return inputCost + outputCost
}

// ============================================================
// Helpers
// ============================================================

// parsePrice converte string de preço da API (ex: "0.0000025") para float64.
// Retorna 0 em caso de erro ou campo vazio.
func parsePrice(s string) float64 {
	if s == "" || s == "0" {
		return 0
	}
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

// ============================================================
// Auto-registro
// ============================================================

// init registra o OpenRouter como InferenceProvider no registry global.
// Basta importar este package com _ "...providers/openrouter" para ativá-lo.
func init() {
	providers.RegisterInference(NewClient())
}
