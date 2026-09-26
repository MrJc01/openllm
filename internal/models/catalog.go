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
	Engine   string   `json:"engine"`
	Modality string   `json:"modality"`
	VRAMGB   float64  `json:"vram_gb"`
	Pull     string   `json:"pull,omitempty"`
	Aliases  []string `json:"aliases,omitempty"`
	// Files: arquivos que a engine precisa (ComfyUI: pasta em models/ + URL).
	// O daemon gera o download e a checagem de "pronto" a partir desta lista.
	Files []ModelFile `json:"files,omitempty"`
	// Workflow: arquivo em uitest/lib/workflows usado para gerar (ComfyUI).
	Workflow string `json:"workflow,omitempty"`
	// ServeAs: id do modelo no servidor quando difere do nome do catálogo
	// (ex: speaches usa o id do HF). O proxy roteia pelo nome do catálogo.
	ServeAs string `json:"serve_as,omitempty"`
	// Voice: voz padrão (TTS).
	Voice string `json:"voice,omitempty"`
	// SizeGB: quanto o modelo baixa (0 = já vem na imagem da engine).
	SizeGB float64 `json:"size_gb,omitempty"`
	// ReadyMin: tempo medido nos testes reais, do deploy até "pronto" (min).
	ReadyMin float64  `json:"ready_min,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	// Throughput / quality metrics (usados por search/scale para filtrar ofertas)
	// Text: "tps" (tokens/s) | Image: "itps" (iterações/s) | Video: "fps" | Audio: "rtf" (real-time factor, menor=pior)
	Metric          string  `json:"metric,omitempty"`
	MetricTarget    float64 `json:"metric_target,omitempty"`    // mínimo aceitável (para rtf, máximo aceitável)
	EstimatedMetric float64 `json:"estimated_metric,omitempty"` // valor estimado em RTX 4090 de referência
}

// ModelFile é um arquivo de modelo: Dir é a pasta em models/ do ComfyUI
// (checkpoints, diffusion_models, text_encoders, vae, clip_vision...).
type ModelFile struct {
	Dir string `json:"dir"`
	URL string `json:"url"`
	// As: nome do arquivo no disco quando difere do da URL
	// (ex: google-t5/t5-base/model.safetensors → t5-base.safetensors).
	As string `json:"as,omitempty"`
}

// Name é o nome do arquivo no disco (As, ou o último trecho da URL).
func (f ModelFile) Name() string {
	if f.As != "" {
		return f.As
	}
	return f.URL[strings.LastIndex(f.URL, "/")+1:]
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
