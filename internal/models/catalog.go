package models

import (
	_ "embed"
	"encoding/json"
	"strings"
)

// CatalogEntry descreve um modelo conhecido do catálogo: qual engine usar,
// qual a modalidade (text/image/audio/video/tts/asr/embedding), VRAM mínima e
// como baixá-lo (pull), quando a engine não baixa sozinha.
type CatalogEntry struct {
	Engine         string   `json:"engine"`
	Modality       string   `json:"modality"`
	VRAMGB         float64  `json:"vram_gb"`
	Pull           string   `json:"pull,omitempty"`
	Aliases        []string `json:"aliases,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	// Throughput / quality metrics (usados por search/scale para filtrar ofertas)
	// Text: "tps" (tokens/s) | Image: "itps" (iterações/s) | Video: "fps" | Audio: "rtf" (real-time factor, menor=pior)
	Metric        string  `json:"metric,omitempty"`
	MetricTarget  float64 `json:"metric_target,omitempty"`  // mínimo aceitável (para rtf, máximo aceitável)
	EstimatedMetric float64 `json:"estimated_metric,omitempty"` // valor estimado em RTX 4090 de referência
}

type catalogFile struct {
	Models map[string]CatalogEntry `json:"models"`
}

//go:embed catalog.json
var catalogJSON []byte

var catalog map[string]CatalogEntry

func init() {
	var c catalogFile
	if err := json.Unmarshal(catalogJSON, &c); err != nil {
		catalog = map[string]CatalogEntry{}
		return
	}
	catalog = c.Models
}

// ResolveCatalog encontra uma entrada do catálogo para o modelo informado.
// Estratégia: match exato → alias → prefixo (ex: "sdxl-turbo" resolve "sdxl").
func ResolveCatalog(modelName string) (CatalogEntry, bool) {
	nameLower := strings.ToLower(strings.TrimSpace(modelName))
	if nameLower == "" {
		return CatalogEntry{}, false
	}

	// 1. Match exato
	if entry, ok := catalog[nameLower]; ok {
		return entry, true
	}

	// 2. Alias
	for name, entry := range catalog {
		for _, alias := range entry.Aliases {
			if strings.EqualFold(alias, modelName) {
				entry := entry
				entry.Aliases = append(entry.Aliases, name)
				return entry, true
			}
		}
	}

	// 3. Prefixo: modelo conhecido no início do nome informado
	// (ex: "sdxl-turbo-v2" → "sdxl"; evita capturar chaves curtas demais)
	bestKey := ""
	for name := range catalog {
		if len(name) < 4 {
			continue
		}
		if strings.HasPrefix(nameLower, name) && len(name) > len(bestKey) {
			bestKey = name
		}
	}
	if bestKey != "" {
		return catalog[bestKey], true
	}

	return CatalogEntry{}, false
}

// CatalogModality retorna a modalidade do modelo, inferindo do nome quando
// não está no catálogo (heurística simples por palavras-chave).
func CatalogModality(modelName string) string {
	if entry, ok := ResolveCatalog(modelName); ok {
		return entry.Modality
	}
	n := strings.ToLower(modelName)
	switch {
	case containsAny(n, "whisper", "asr", "stt", "transcri"):
		return "asr"
	case containsAny(n, "tts", "xtts", "voice", "speech"):
		return "tts"
	case containsAny(n, "video", "ltx", "wan", "mochi", "hunyuan"):
		return "video"
	case containsAny(n, "sdxl", "sd-", "flux", "diffusion", "stable", "image"):
		return "image"
	case containsAny(n, "embed"):
		return "embedding"
	default:
		return "text"
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ListCatalog retorna todos os modelos do catálogo ordenados por nome.
func ListCatalog() map[string]CatalogEntry {
	out := make(map[string]CatalogEntry, len(catalog))
	for k, v := range catalog {
		out[k] = v
	}
	return out
}
// CatalogByEngine agrupa os modelos do catálogo por engine (para UIs).
func CatalogByEngine() map[string]map[string]CatalogEntry {
	out := map[string]map[string]CatalogEntry{}
	for name, e := range catalog {
		if out[e.Engine] == nil {
			out[e.Engine] = map[string]CatalogEntry{}
		}
		out[e.Engine][name] = e
	}
	return out
}
