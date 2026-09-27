package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crom-org/openllm/internal/httpsec"
)

func TestProxyListenDefaultsToLoopbackAndNeedsToken(t *testing.T) {
	p := NewProxyServer(11434)
	if _, err := p.ListenAddr(); err == nil {
		t.Fatal("proxy sem token não pode subir")
	}
	p.SetSecurity("", &httpsec.Guard{Token: "t"})
	if addr, err := p.ListenAddr(); err != nil || addr != "127.0.0.1:11434" {
		t.Fatalf("addr=%q err=%v, quer 127.0.0.1:11434", addr, err)
	}
	p.SetSecurity("0.0.0.0:11435", &httpsec.Guard{})
	if _, err := p.ListenAddr(); err == nil {
		t.Fatal("listener público sem token deveria ser recusado")
	}
	p.SetSecurity("0.0.0.0:11435", &httpsec.Guard{Token: "t"})
	if addr, err := p.ListenAddr(); err != nil || addr != "0.0.0.0:11435" {
		t.Fatalf("addr=%q err=%v", addr, err)
	}
}

func TestProxyHandlerRequiresToken(t *testing.T) {
	p := NewProxyServer(0)
	p.SetSecurity("", &httpsec.Guard{Token: "t", MaxBody: ProxyMaxBody})
	h := p.Handler()
	for _, c := range []struct {
		auth string
		want int
	}{{"", 401}, {"Bearer x", 401}, {"Bearer t", 200}} {
		r := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
		r.Host = "localhost:11434"
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != c.want {
			t.Fatalf("auth %q: status %d, quer %d", c.auth, rec.Code, c.want)
		}
	}
}
