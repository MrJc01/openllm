package daemon

import (
	"context"
	"fmt"

	"github.com/crom-org/openllm/internal/config"
	"github.com/crom-org/openllm/internal/engines"
	"github.com/crom-org/openllm/internal/models"
	"github.com/crom-org/openllm/internal/providers"
)

// findBestOffer busca a oferta de GPU mais barata que atenda o modelo na
// engine indicada. Usada pelo scale e pelos stacks (a CLI tem a própria
// versão interativa para deploys individuais).
func findBestOffer(cfg *config.Config, engine, model string, maxCostPerHour float64) (providers.Machine, error) {
	client, ok := providers.GetCompute(cfg.Provider)
	if !ok {
		return providers.Machine{}, fmt.Errorf("provider %s is not registered or not a compute provider", cfg.Provider)
	}

	// VRAM: catálogo → ModelVRAM da engine → default da engine
	targetVram := engines.VRAMFor(engine, model)
	if entry, ok := models.ResolveCatalog(model); ok && entry.VRAMGB > 0 {
		targetVram = entry.VRAMGB
	}

	// TPS só faz sentido para texto; outras modalidades filtram por VRAM
	targetTps := 0.0
	if entry, ok := models.ResolveCatalog(model); ok && entry.Modality == "text" {
		targetTps = cfg.TpsTarget
	}

	results, err := client.Search(context.Background(), providers.SearchRequest{
		MinVRAM: targetVram,
		MinTPS:  targetTps,
		Model:   model,
	})
	if err != nil {
		return providers.Machine{}, err
	}

	var candidates []providers.Machine
	for _, m := range results {
		if maxCostPerHour > 0 && m.CostPerHour > maxCostPerHour {
			continue
		}
		candidates = append(candidates, m)
	}
	if len(candidates) == 0 {
		return providers.Machine{}, fmt.Errorf("no available GPU machines satisfy the requirements (vram=%.0fGB%s)", targetVram, costFilterSuffix(maxCostPerHour))
	}
	return PickBestMachine(candidates), nil
}

// PickBestMachine escolhe a melhor oferta: entre as candidatas dentro de uma
// tolerância de preço (+20% da mais barata), vence a de maior banda de rede.
// Banda baixa alonga o pull da imagem docker e o download do modelo — o
// gargalo real de deploy; pagar centavos a mais por um host rápido compensa.
// Sem dados de banda (NetMbps == 0), mantém o critério clássico: mais barato.
func PickBestMachine(candidates []providers.Machine) providers.Machine {
	if len(candidates) == 0 {
		return providers.Machine{}
	}
	cheapest := candidates[0]
	for _, m := range candidates {
		if m.CostPerHour < cheapest.CostPerHour {
			cheapest = m
		}
	}
	ceiling := cheapest.CostPerHour * 1.20
	best := cheapest
	for _, m := range candidates {
		faster := m.NetMbps > best.NetMbps
		if best == cheapest && best.NetMbps == 0 {
			// sem banda conhecida na base: qualquer dado de banda vence
			faster = m.NetMbps > 0
		}
		if m.CostPerHour <= ceiling && faster {
			best = m
		}
	}
	return best
}

func costFilterSuffix(maxCostPerHour float64) string {
	if maxCostPerHour > 0 {
		return fmt.Sprintf(", max_cost=%.3f/h", maxCostPerHour)
	}
	return ""
}