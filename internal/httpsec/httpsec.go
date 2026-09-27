// Package httpsec protege as APIs HTTP locais do openllm (controle do daemon
// e proxy de inferência): token compartilhado, allowlist de Host (contra DNS
// rebinding), bloqueio de Origin (páginas web), JSON obrigatório e limite de
// corpo.
package httpsec

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"mime"
	"net"
	"net/http"
	"os"
	"strings"
)

// TokenEnv é a variável com o segredo compartilhado.
const TokenEnv = "OPENLLM_API_TOKEN"

// EnsureToken devolve o token de OPENLLM_API_TOKEN; se ausente, gera 32 bytes
// aleatórios (hex), acrescenta ao .env em envPath (0600) e exporta no
// processo. generated indica que um token novo foi criado.
func EnsureToken(envPath string) (token string, generated bool, err error) {
	if t := strings.TrimSpace(os.Getenv(TokenEnv)); t != "" {
		return t, false, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", false, err
	}
	token = hex.EncodeToString(buf)
	f, err := os.OpenFile(envPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return "", false, err
	}
	prefix := ""
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		prefix = "\n" // não gruda na última linha se ela não tiver \n
	}
	if _, err := fmt.Fprintf(f, "%s%s=%s\n", prefix, TokenEnv, token); err != nil {
		return "", false, err
	}
	os.Setenv(TokenEnv, token)
	return token, true, nil
}

// ReadTokenFromEnvFile lê OPENLLM_API_TOKEN do ambiente ou, se ausente, do
// primeiro .env que o definir (clientes: CLI, testes).
func ReadTokenFromEnvFile(paths ...string) string {
	if t := strings.TrimSpace(os.Getenv(TokenEnv)); t != "" {
		return t
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			k, v, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
			if ok && strings.TrimSpace(k) == TokenEnv {
				if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// Guard é o middleware de segurança.
type Guard struct {
	Token          string
	AllowedHosts   map[string]bool // além de localhost/127.0.0.1/::1
	AllowedOrigins map[string]bool // vazio = nenhum Origin aceito
	MaxBody        int64           // bytes; 0 = sem limite
	RequireJSON    bool            // POST/PUT/PATCH exigem application/json
}

// NewGuard monta um Guard a partir das variáveis OPENLLM_ALLOWED_HOSTS e
// OPENLLM_ALLOWED_ORIGINS (listas separadas por vírgula).
func NewGuard(token string, maxBody int64, requireJSON bool) *Guard {
	return &Guard{
		Token:          token,
		AllowedHosts:   splitSet(os.Getenv("OPENLLM_ALLOWED_HOSTS")),
		AllowedOrigins: splitSet(os.Getenv("OPENLLM_ALLOWED_ORIGINS")),
		MaxBody:        maxBody,
		RequireJSON:    requireJSON,
	}
}

func splitSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out[v] = true
		}
	}
	return out
}

// hostOnly remove a porta (e colchetes de IPv6) do Host.
func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}

func (g *Guard) hostAllowed(h string) bool {
	switch h = hostOnly(h); h {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return g.AllowedHosts[h]
	}
}

// Authorized confere "Authorization: Bearer <token>" em tempo constante.
func (g *Guard) Authorized(r *http.Request) bool {
	if g.Token == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return false
	}
	got := strings.TrimSpace(auth[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(g.Token)) == 1
}

// Wrap aplica as checagens na ordem: Host, Origin, token, Content-Type, corpo.
func (g *Guard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.hostAllowed(r.Host) {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !g.AllowedOrigins[strings.ToLower(o)] {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if !g.Authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="openllm"`)
			http.Error(w, "missing or invalid bearer token", http.StatusUnauthorized)
			return
		}
		if g.RequireJSON && (r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch) {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		if g.MaxBody > 0 && r.ContentLength > g.MaxBody {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if g.MaxBody > 0 && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, g.MaxBody)
		}
		next.ServeHTTP(w, r)
	})
}

// IsLoopbackAddr diz se um endereço de escuta ("host:porta") é só loopback.
// Host vazio (":11434") ou 0.0.0.0/:: escutam em todas as interfaces.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
