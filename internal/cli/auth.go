package cli

import (
	"net/http"
	"strings"

	"github.com/crom-org/openllm/internal/httpsec"
)

// authTransport injeta "Authorization: Bearer <OPENLLM_API_TOKEN>" nas
// chamadas ao daemon/proxy locais (nunca em hosts remotos).
type authTransport struct {
	base  http.RoundTripper
	token func() string
}

func (t *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if isLocalHost(r.URL.Hostname()) && r.Header.Get("Authorization") == "" {
		if tok := t.token(); tok != "" {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	return t.base.RoundTrip(r)
}

func isLocalHost(h string) bool {
	switch strings.ToLower(h) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// localToken: ambiente ou .env do diretório atual (o mesmo que o daemon usa).
func localToken() string {
	return httpsec.ReadTokenFromEnvFile(".env", "openllm/.env")
}

func init() {
	base := http.DefaultClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	http.DefaultClient.Transport = &authTransport{base: base, token: localToken}
}
