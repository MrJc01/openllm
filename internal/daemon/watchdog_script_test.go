package daemon

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// O script renderizado precisa ser shell válido (o fmt com %% é fácil de errar).
func TestWatchdogScriptIsValidShell(t *testing.T) {
	script := fmt.Sprintf(watchdogScript, "123", "key", 17291, "123", 300)
	p := filepath.Join(t.TempDir(), "wd.sh")
	if err := os.WriteFile(p, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", p).CombinedOutput(); err != nil {
		t.Fatalf("invalid shell: %v\n%s\n---\n%s", err, out, script)
	}
}

// Roda o script de verdade (intervalo encurtado) contra um servidor local:
// registra "ping ok" com o daemon vivo e "ping failed" quando ele some.
func TestWatchdogScriptPings(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl não disponível")
	}
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("pong"))
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	script := fmt.Sprintf(watchdogScript, "123", "key", port, "123", 3600)
	script = strings.Replace(script, "sleep 30", "sleep 0.2", 1)
	p := filepath.Join(t.TempDir(), "wd.sh")
	os.WriteFile(p, []byte(script), 0700)

	cmd := exec.Command("sh", p)
	var out strings.Builder
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	up = false
	time.Sleep(700 * time.Millisecond)
	cmd.Process.Kill()
	cmd.Wait()

	if !strings.Contains(out.String(), "ping ok") || !strings.Contains(out.String(), "ping failed") {
		t.Fatalf("expected ok then failed, got:\n%s", out.String())
	}
}
