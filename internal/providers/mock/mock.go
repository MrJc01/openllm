// Package mock fornece um ComputeProvider determinístico para testes e
// desenvolvimento local: aluga "máquinas" fictícias sem gastar nada e sem
// rede. Registre-o com import blank: `_ "github.com/crom-org/openllm/internal/providers/mock"`
// e use provider "mock" no openllm.json.
package mock

import (
	"context"
	"fmt"
	"sync"

	"github.com/crom-org/openllm/internal/providers"
)

type Provider struct {
	mu        sync.Mutex
	instances map[string]*providers.InstanceInfo
	counter   int
}

func (p *Provider) Name() string { return "mock" }

func (p *Provider) Search(ctx context.Context, req providers.SearchRequest) ([]providers.Machine, error) {
	machines := []providers.Machine{
		{
			ID:              "mock-offer-001", Provider: "mock", GPU: "RTX 4090",
			VRAM: 24, GPUCount: 1, CostPerHour: 0.35, Location: "BR-Mock",
			EstimatedTPS: 120, EstimatedMetric: 120, Metric: "tps", NetMbps: 5000,
		},
		{
			ID:              "mock-offer-002", Provider: "mock", GPU: "RTX 3090",
			VRAM: 24, GPUCount: 1, CostPerHour: 0.22, Location: "US-Mock",
			EstimatedTPS: 90, EstimatedMetric: 90, Metric: "tps", NetMbps: 2000,
		},
		{
			ID:              "mock-offer-003", Provider: "mock", GPU: "A100",
			VRAM: 80, GPUCount: 1, CostPerHour: 1.10, Location: "EU-Mock",
			EstimatedTPS: 300, EstimatedMetric: 300, Metric: "tps", NetMbps: 10000,
		},
		{
			ID:              "mock-offer-004", Provider: "mock", GPU: "RTX 3060",
			VRAM: 12, GPUCount: 1, CostPerHour: 0.15, Location: "US-Mock",
			EstimatedTPS: 55, EstimatedMetric: 55, Metric: "tps", NetMbps: 1000,
		},
		// Extra offers for image/video/audio modalities
		{
			ID:              "mock-offer-img-001", Provider: "mock", GPU: "RTX 4090",
			VRAM: 24, GPUCount: 1, CostPerHour: 0.35, Location: "BR-Mock",
			EstimatedTPS: 2.5, EstimatedMetric: 2.5, Metric: "itps", NetMbps: 5000,
		},
		{
			ID:              "mock-offer-vid-001", Provider: "mock", GPU: "RTX 4090",
			VRAM: 24, GPUCount: 1, CostPerHour: 0.35, Location: "BR-Mock",
			EstimatedTPS: 1.0, EstimatedMetric: 1.0, Metric: "fps", NetMbps: 5000,
		},
		{
			ID:              "mock-offer-aud-001", Provider: "mock", GPU: "RTX 3060",
			VRAM: 12, GPUCount: 1, CostPerHour: 0.15, Location: "US-Mock",
			EstimatedTPS: 0.08, EstimatedMetric: 0.08, Metric: "rtf", NetMbps: 1000,
		},
	}

	var out []providers.Machine
	for _, m := range machines {
		if m.VRAM < req.MinVRAM {
			continue
		}
		// Metric-aware filter
		if req.MinTPS > 0 && m.EstimatedMetric > 0 {
			if req.Metric == "rtf" {
				if m.EstimatedMetric > req.MinTPS {
					continue
				}
			} else {
				if m.EstimatedMetric < req.MinTPS {
					continue
				}
			}
		}
		if req.GPUCount > 0 && m.GPUCount != req.GPUCount {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (p *Provider) Deploy(ctx context.Context, req providers.DeployRequest) (*providers.InstanceInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counter++
	id := fmt.Sprintf("mock-inst-%03d", p.counter)
	info := &providers.InstanceInfo{
		ID:          id,
		Provider:    "mock",
		MachineID:   req.MachineID,
		GPU:         "RTX 4090",
		GPUCount:    1,
		VRAM:        24,
		CostPerHour: 0.35,
		SSHHost:     "127.0.0.1",
		SSHPort:     2200 + p.counter,
		Status:      "running",
	}
	p.instances[id] = info
	return info, nil
}

func (p *Provider) Destroy(ctx context.Context, instanceID string, apiKey string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Idempotente: destruir instância inexistente é sucesso (semántica de
	// reconciliação — permite seed de instâncias direto no banco em testes)
	delete(p.instances, instanceID)
	return nil
}

func (p *Provider) Pause(ctx context.Context, instanceID string, apiKey string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	inst, ok := p.instances[instanceID]
	if !ok {
		return fmt.Errorf("instance %s not found", instanceID)
	}
	inst.Status = "stopped"
	return nil
}

func (p *Provider) Resume(ctx context.Context, instanceID string, apiKey string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	inst, ok := p.instances[instanceID]
	if !ok {
		return fmt.Errorf("instance %s not found", instanceID)
	}
	inst.Status = "running"
	return nil
}

func (p *Provider) GetStatus(ctx context.Context, instanceID string, apiKey string) (*providers.InstanceInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	inst, ok := p.instances[instanceID]
	if !ok {
		return nil, fmt.Errorf("instance %s not found", instanceID)
	}
	cp := *inst
	return &cp, nil
}

// CountActive expõe o número de instâncias vivas (helper para testes).
func (p *Provider) CountActive() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.instances)
}

// defaultProvider é o singleton registrado no init e retornado por Default.
var defaultProvider = &Provider{instances: map[string]*providers.InstanceInfo{}}

// Default expõe o provider singleton (helper para testes inspecionarem o
// estado das instâncias fictícias, ex.: CountActive, GetStatus).
func Default() *Provider { return defaultProvider }

func init() {
	providers.RegisterCompute(defaultProvider)
}
// OfferPrice devolve o preço da oferta no catálogo fictício (0.35 se desconhecida).
func (p *Provider) OfferPrice(ctx context.Context, machineID, apiKey string) (float64, error) {
	offers, _ := p.Search(ctx, providers.SearchRequest{})
	for _, o := range offers {
		if o.ID == machineID {
			return o.CostPerHour, nil
		}
	}
	return 0.35, nil
}
