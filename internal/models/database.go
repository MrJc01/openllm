package models

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

//go:embed models.json
var modelsJSON []byte

type ModelInfo struct {
	ParamsB     float64 `json:"params_b"`
	VramGB      float64 `json:"vram_gb"`
	TpsRtx4090  float64 `json:"tps_rtx4090"`
}

type ModelDB struct {
	Known             map[string]ModelInfo  `json:"known"`
	GpuTpsMultipliers map[string]float64    `json:"gpu_tps_multipliers"`
}

var db *ModelDB

func init() {
	var err error
	db, err = LoadDatabase()
	if err != nil {
		// Fallback to empty if embed fails for some reason
		db = &ModelDB{
			Known:             make(map[string]ModelInfo),
			GpuTpsMultipliers: make(map[string]float64),
		}
	}
}

func LoadDatabase() (*ModelDB, error) {
	var database ModelDB
	if err := json.Unmarshal(modelsJSON, &database); err != nil {
		return nil, err
	}
	return &database, nil
}

// ParseParamsSize tenta extrair o número de bilhões de parâmetros do nome do modelo (ex: llama3.3:70b -> 70.0)
func ParseParamsSize(modelName string) (float64, bool) {
	// Procura padrões como "7b", "70b", "1.5b", "3.8b", "70B"
	re := regexp.MustCompile(`(?i)(?:^|:|\-)([0-9]+(?:\.[0-9]+)?)[bB](?:$|\-|:)`)
	matches := re.FindStringSubmatch(modelName)
	if len(matches) > 1 {
		val, err := strconv.ParseFloat(matches[1], 64)
		if err == nil {
			return val, true
		}
	}
	// Tenta procurar qualquer número próximo a 'b'
	re2 := regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*[bB]`)
	matches2 := re2.FindStringSubmatch(modelName)
	if len(matches2) > 1 {
		val, err := strconv.ParseFloat(matches2[1], 64)
		if err == nil {
			return val, true
		}
	}
	return 0, false
}

// GetRequirements retorna a VRAM estimada (GB) e o TPS base estimado na RTX 4090
func GetRequirements(modelName string) (vramGB float64, baseTpsRtx4090 float64) {
	// Normaliza o nome do modelo
	nameLower := strings.ToLower(modelName)
	
	// Verifica se está nos conhecidos
	if info, exists := db.Known[nameLower]; exists {
		return info.VramGB, info.TpsRtx4090
	}
	// Tenta buscar por prefixo (ex: deepseek-r1:70b-q4_K_M -> deepseek-r1:70b)
	for k, info := range db.Known {
		if strings.HasPrefix(nameLower, k) || strings.HasPrefix(k, nameLower) {
			return info.VramGB, info.TpsRtx4090
		}
	}

	// Se não for conhecido, estimamos
	params, found := ParseParamsSize(modelName)
	if !found {
		// Se não encontrou tamanho, assume um modelo padrão de 7B
		params = 7.0
	}

	// Fórmula de estimativa:
	// VRAM em GB = params * 0.65 (assumindo quantização de 4 bits + overhead de context/runner) + 1.5GB de overhead fixo
	vramGB = (params * 0.65) + 1.5

	// TPS base estimado na RTX 4090:
	// Para 7B a RTX 4090 dá ~120 TPS. Para 70B dá ~12 TPS.
	// Uma boa aproximação é: TPS = 800 / params
	baseTpsRtx4090 = 800.0 / params
	if baseTpsRtx4090 > 250 {
		baseTpsRtx4090 = 250 // teto razoável
	}
	if baseTpsRtx4090 < 5 {
		baseTpsRtx4090 = 5 // piso razoável
	}

	return vramGB, baseTpsRtx4090
}

// EstimateTPS estima o TPS para uma determinada GPU e quantidade
func EstimateTPS(modelName string, gpuModel string, gpuCount int) float64 {
	_, baseTps := GetRequirements(modelName)
	
	// Normaliza nome da GPU para tentar encontrar multiplicador
	gpuModel = strings.ToUpper(gpuModel)
	multiplier := 0.5 // multiplicador padrão para GPU desconhecida (relativo a RTX 4090)
	
	found := false
	for k, m := range db.GpuTpsMultipliers {
		if strings.Contains(gpuModel, strings.ToUpper(k)) || strings.Contains(strings.ToUpper(k), gpuModel) {
			multiplier = m
			found = true
			break
		}
	}

	if !found {
		// Heurística básica baseada em VRAM se conseguirmos inferir, ou apenas assume 0.5
		if strings.Contains(gpuModel, "A100") {
			multiplier = 1.6
		} else if strings.Contains(gpuModel, "H100") {
			multiplier = 3.0
		} else if strings.Contains(gpuModel, "4090") {
			multiplier = 1.0
		} else if strings.Contains(gpuModel, "3090") {
			multiplier = 0.72
		}
	}

	// O TPS cresce linearmente com a contagem de GPUs, mas com alguma perda de eficiência
	efficiency := 1.0
	if gpuCount > 1 {
		efficiency = 0.9 // Multi-GPU overhead
	}
	
	estimated := baseTps * multiplier * float64(gpuCount) * efficiency
	return estimated
}
