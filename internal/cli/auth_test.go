package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthTransportOnlyLocal(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer srv.Close()
	c := &http.Client{Transport: &authTransport{base: http.DefaultTransport, token: func() string { return "tok" }}}
	if _, err := c.Get(srv.URL); err != nil { // httptest escuta em 127.0.0.1
		t.Fatal(err)
	}
	if got != "Bearer tok" {
		t.Fatalf("Authorization = %q", got)
	}
	for _, h := range []string{"console.vast.ai", "10.0.0.2", "evil.localhost.com"} {
		if isLocalHost(h) {
			t.Fatalf("%s não é local", h)
		}
	}
}
