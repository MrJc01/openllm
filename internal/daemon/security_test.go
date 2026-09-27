package daemon

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crom-org/openllm/internal/httpsec"
	"github.com/crom-org/openllm/internal/providers"
)

// priceProvider: provedor falso com preço de oferta configurável; registra
// se o aluguel chegou a ser feito.
type priceProvider struct {
	mu       sync.Mutex
	price    float64
	deployed int
}

func (p *priceProvider) Name() string { return "pricefake" }
func (p *priceProvider) Search(context.Context, providers.SearchRequest) ([]providers.Machine, error) {
	return nil, nil
}
func (p *priceProvider) Deploy(ctx context.Context, req providers.DeployRequest) (*providers.InstanceInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deployed++
	return &providers.InstanceInfo{ID: "pf-1", Status: "deploying"}, nil
}
func (p *priceProvider) Destroy(context.Context, string, string) error { return nil }
func (p *priceProvider) Pause(context.Context, string, string) error   { return nil }
func (p *priceProvider) Resume(context.Context, string, string) error  { return nil }
func (p *priceProvider) GetStatus(context.Context, string, string) (*providers.InstanceInfo, error) {
	return nil, errors.New("not found")
}
func (p *priceProvider) OfferPrice(context.Context, string, string) (float64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.price, nil
}

func TestDeployRechecksOfferPrice(t *testing.T) {
	prov := &priceProvider{}
	providers.RegisterCompute(prov)
	m, _, _ := newTestManager(t)
	cfg := testConfig()
	cfg.Provider = "pricefake"
	cfg.Providers["pricefake"] = cfg.Providers["mock"]

	tests := []struct {
		price, max float64
		wantErr    bool
	}{
		{price: 0.30, max: 0.25, wantErr: true},  // subiu 20%
		{price: 0.26, max: 0.25, wantErr: false}, // dentro dos 5%
		{price: 0.50, max: 0, wantErr: false},    // sem teto
	}
	for _, tc := range tests {
		prov.mu.Lock()
		prov.price, prov.deployed = tc.price, 0
		prov.mu.Unlock()
		_, err := m.DeployInstance(cfg, DeployRequestPayload{Model: "llama3.2:3b", MachineID: "123", MaxCostPerHour: tc.max})
		prov.mu.Lock()
		deployed := prov.deployed
		prov.mu.Unlock()
		if tc.wantErr {
			if !errors.Is(err, providers.ErrPriceAboveMax) || deployed != 0 {
				t.Fatalf("price %.2f max %.2f: err=%v deployed=%d, quer recusa sem aluguel", tc.price, tc.max, err, deployed)
			}
		} else if err != nil || deployed != 1 {
			t.Fatalf("price %.2f max %.2f: err=%v deployed=%d", tc.price, tc.max, err, deployed)
		}
	}
}

func TestDeployCustomImageNeedsOptIn(t *testing.T) {
	m, _, _ := newTestManager(t)
	t.Setenv("OPENLLM_ALLOW_CUSTOM", "")
	_, err := m.DeployInstance(testConfig(), DeployRequestPayload{Model: "x", MachineID: "mock-offer-001", CustomImage: "evil/img"})
	if !errors.Is(err, ErrCustomDisabled) {
		t.Fatalf("err = %v, quer ErrCustomDisabled", err)
	}
}

func TestControlAPIRequiresTokenAndJSON(t *testing.T) {
	m, db, _ := newTestManager(t)
	s := NewControlServer(0, m, db)
	if _, err := s.Handler(); err == nil {
		t.Fatal("API sem token não pode subir")
	}
	s.SetGuard(httpsec.NewGuard("tok", ControlMaxBody, true))
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, auth, ctype, origin, body string) int {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "127.0.0.1:17290"
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		if ctype != "" {
			r.Header.Set("Content-Type", ctype)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	cases := []struct {
		name                            string
		method, path, auth, ctype, orig string
		body                            string
		want                            int
	}{
		{"status sem token", "GET", "/status", "", "", "", "", 401},
		{"status com token", "GET", "/status", "Bearer tok", "", "", "", 200},
		{"deploy CSRF text/plain", "POST", "/deploy", "Bearer tok", "text/plain", "", `{"machine_id":"1","model":"m"}`, 415},
		{"deploy de página web", "POST", "/deploy", "Bearer tok", "application/json", "http://evil.com", `{}`, 403},
		{"deploy sem token", "POST", "/deploy", "", "application/json", "", `{}`, 401},
		{"corpo > 1MB", "POST", "/deploy", "Bearer tok", "application/json", "", strings.Repeat(" ", ControlMaxBody+1), 413},
	}
	for _, c := range cases {
		if got := do(c.method, c.path, c.auth, c.ctype, c.orig, c.body); got != c.want {
			t.Errorf("%s: status %d, quer %d", c.name, got, c.want)
		}
	}
}

func TestHeartbeatOnlyKnownInstances(t *testing.T) {
	m, db, _ := newTestManager(t)
	seedRunning(t, m, db, "hb-1", nil)
	hb := NewHeartbeatServer(0, m)
	cases := []struct {
		method, url string
		want        int
	}{
		{"GET", "/ping?instance_id=hb-1", 200},
		{"GET", "/ping?instance_id=nope", 404},
		{"GET", "/ping", 400},
		{"POST", "/ping?instance_id=hb-1", 404},
		{"GET", "/status", 404},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		hb.handlePing(rec, httptest.NewRequest(c.method, c.url, nil))
		if rec.Code != c.want {
			t.Errorf("%s %s: status %d, quer %d", c.method, c.url, rec.Code, c.want)
		}
	}
	if _, ok := m.GetLastPing("nope"); ok {
		t.Fatal("ping de instância desconhecida foi registrado")
	}
}
