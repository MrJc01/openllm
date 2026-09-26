package daemon

import (
	"testing"

	"github.com/crom-org/openllm/internal/providers"
)

// Especialista: QA — findBestOffer respeita VRAM do catálogo, TPS para texto
// e filtro de custo máximo.
func TestFindBestOfferCheapest(t *testing.T) {
	cfg := testConfig() // TpsTarget 40

	// sdxl → vram 12 (catálogo) → 3060 ($0.15) é a mais barata que atende
	offer, err := findBestOffer(cfg, "comfyui", "sdxl", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if offer.ID != "mock-offer-004" {
		t.Fatalf("expected cheapest 3060 (mock-offer-004), got %s ($%.2f)", offer.ID, offer.CostPerHour)
	}
}

func TestFindBestOfferLargeModelNeedsA100(t *testing.T) {
	cfg := testConfig()

	// deepseek-r1:70b → vram 44 (catálogo) → só a A100 (80GB) atende
	offer, err := findBestOffer(cfg, "ollama", "deepseek-r1:70b", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if offer.GPU != "A100" {
		t.Fatalf("expected A100 for 44GB requirement, got %s", offer.GPU)
	}
}

func TestFindBestOfferMaxCostFilter(t *testing.T) {
	cfg := testConfig()

	// sdxl caberia na 3060 ($0.15), mas o teto é $0.10 → nenhum resultado
	if _, err := findBestOffer(cfg, "comfyui", "sdxl", 0.10); err == nil {
		t.Fatal("expected error when max cost excludes all offers")
	}
}

func TestFindBestOfferTPSFilterForText(t *testing.T) {
	cfg := testConfig()
	cfg.TpsTarget = 200 // só a A100 (300 tps) atende

	offer, err := findBestOffer(cfg, "ollama", "deepseek-r1:7b", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if offer.GPU != "A100" {
		t.Fatalf("expected A100 for 200 tps target, got %s", offer.GPU)
	}
}

func TestFindBestOfferNoTPSFilterForImage(t *testing.T) {
	cfg := testConfig()
	cfg.TpsTarget = 200 // alto, mas modalidade image não filtra por TPS

	offer, err := findBestOffer(cfg, "comfyui", "sdxl", 0)
	if err != nil {
		t.Fatalf("image models must not filter by TPS: %v", err)
	}
	if offer.ID != "mock-offer-004" {
		t.Fatalf("expected 3060, got %s", offer.ID)
	}
}

// Especialista: SRE — entre ofertas de preço parecido (±20% do mais barato),
// vence a de maior banda de rede: o gargalo real do deploy é o pull da imagem
// docker, não o preço por hora. Hosts lentos derrubaram deploys em produção.
func TestPickBestMachinePrefersBandwidthWithinPriceTolerance(t *testing.T) {
	cases := []struct {
		name string
		in   []providers.Machine
		want string
	}{
		{
			name: "paga 15% a mais por banda alta",
			in: []providers.Machine{
				{ID: "barato-lento", CostPerHour: 0.05, NetMbps: 100},
				{ID: "rapido", CostPerHour: 0.0575, NetMbps: 2000},
			},
			want: "rapido",
		},
		{
			name: "fora da tolerancia de preco, fica no barato",
			in: []providers.Machine{
				{ID: "barato", CostPerHour: 0.05, NetMbps: 100},
				{ID: "caro-rapido", CostPerHour: 0.10, NetMbps: 5000},
			},
			want: "barato",
		},
		{
			name: "sem banda conhecida: mais barato",
			in: []providers.Machine{
				{ID: "barato", CostPerHour: 0.05},
				{ID: "caro", CostPerHour: 0.06},
			},
			want: "barato",
		},
		{
			name: "mais barato ja e o mais rapido",
			in: []providers.Machine{
				{ID: "otimo", CostPerHour: 0.05, NetMbps: 5000},
				{ID: "outro", CostPerHour: 0.06, NetMbps: 1000},
			},
			want: "otimo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PickBestMachine(tc.in); got.ID != tc.want {
				t.Fatalf("got %s, want %s", got.ID, tc.want)
			}
		})
	}
}
