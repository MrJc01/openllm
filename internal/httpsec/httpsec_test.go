package httpsec

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardWrap(t *testing.T) {
	g := &Guard{Token: "s3cret", AllowedHosts: map[string]bool{"lan.box": true},
		AllowedOrigins: map[string]bool{"http://ok.local": true}, MaxBody: 16, RequireJSON: true}
	h := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	type req struct {
		name, method, host, auth, origin, ctype, body string
		chunked                                       bool
		want                                          int
	}
	ok := "Bearer s3cret"
	cases := []req{
		{name: "ok GET", method: "GET", host: "127.0.0.1:17290", auth: ok, want: 200},
		{name: "ok localhost/::1", method: "GET", host: "[::1]:17290", auth: ok, want: 200},
		{name: "sem token", method: "GET", host: "localhost", want: 401},
		{name: "token errado mesmo tamanho", method: "GET", host: "localhost", auth: "Bearer s3creT", want: 401},
		{name: "token prefixo", method: "GET", host: "localhost", auth: "Bearer s3c", want: 401},
		{name: "esquema errado", method: "GET", host: "localhost", auth: "Basic s3cret", want: 401},
		{name: "bearer minúsculo", method: "GET", host: "localhost", auth: "bearer s3cret", want: 200},
		{name: "DNS rebinding", method: "GET", host: "evil.com:17290", auth: ok, want: 403},
		{name: "host liberado", method: "GET", host: "lan.box:11434", auth: ok, want: 200},
		{name: "origin de página web", method: "GET", host: "localhost", auth: ok, origin: "http://evil.com", want: 403},
		{name: "origin liberado", method: "GET", host: "localhost", auth: ok, origin: "http://ok.local", want: 200},
		{name: "POST text/plain", method: "POST", host: "localhost", auth: ok, ctype: "text/plain", body: "{}", want: 415},
		{name: "POST sem content-type", method: "POST", host: "localhost", auth: ok, body: "{}", want: 415},
		{name: "POST json", method: "POST", host: "localhost", auth: ok, ctype: "application/json; charset=utf-8", body: "{}", want: 200},
		{name: "corpo acima do limite", method: "POST", host: "localhost", auth: ok, ctype: "application/json", body: strings.Repeat("x", 17), want: 413},
		{name: "chunked acima do limite", method: "POST", host: "localhost", auth: ok, ctype: "application/json", body: strings.Repeat("x", 17), chunked: true, want: 413},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "/deploy", strings.NewReader(c.body))
			if c.chunked {
				r.ContentLength = -1
			}
			r.Host = c.host
			for k, v := range map[string]string{"Authorization": c.auth, "Origin": c.origin, "Content-Type": c.ctype} {
				if v != "" {
					r.Header.Set(k, v)
				}
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != c.want {
				t.Fatalf("status %d, quer %d (%s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestGuardWithoutTokenRejectsAll(t *testing.T) {
	g := &Guard{}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer ")
	if g.Authorized(r) {
		t.Fatal("guard sem token não pode autorizar")
	}
}

func TestEnsureToken(t *testing.T) {
	t.Setenv(TokenEnv, "")
	os.Unsetenv(TokenEnv)
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("HF_TOKEN=x"), 0644) // sem \n final
	tok, gen, err := EnsureToken(p)
	if err != nil || !gen || len(tok) != 64 {
		t.Fatalf("tok=%q gen=%v err=%v", tok, gen, err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0600 {
		t.Fatalf("permissão %v, quer 0600", st.Mode().Perm())
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "HF_TOKEN=x\n"+TokenEnv+"="+tok+"\n") {
		t.Fatalf(".env = %q", data)
	}
	if got := ReadTokenFromEnvFile(p); got != tok {
		t.Fatalf("ReadTokenFromEnvFile = %q", got)
	}
	tok2, gen2, _ := EnsureToken(p) // já exportado: reaproveita
	if gen2 || tok2 != tok {
		t.Fatal("segunda chamada não deveria gerar outro token")
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:11434": true, "localhost:1": true, "[::1]:1": true,
		":11434": false, "0.0.0.0:11434": false, "192.168.0.2:1": false, "bad": false,
	} {
		if IsLoopbackAddr(addr) != want {
			t.Errorf("IsLoopbackAddr(%q) != %v", addr, want)
		}
	}
}
