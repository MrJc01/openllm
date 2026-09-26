package engines

import (
	"os"
	"path/filepath"
	"testing"
)

// Especialista: Segurança — manifest inválido não derruba o boot do daemon
// (é pulado com warning) e campos obrigatórios são validados.
func TestLoadExternalInvalidFilesSkipped(t *testing.T) {
	dir := t.TempDir()

	files := map[string]string{
		"broken.json":       `{ não é json`,
		"no-port.json":      `{"name": "x", "docker_image": "a/b"}`,
		"no-image.json":     `{"name": "y", "remote_port": 80}`,
		"valid.json":        `{"name": "valid", "remote_port": 8080, "default_vram": 4, "docker_image": "ok/img", "on_start_cmd": "run"}`,
		"no-name.json":      `{"remote_port": 7070, "default_vram": 4, "docker_image": "ok/img2", "on_start_cmd": "run"}`,
		"not-loaded.txt":    `{"name": "z", "remote_port": 1, "docker_image": "a"}`,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	loaded := LoadExternal(dir)

	names := map[string]bool{}
	for _, n := range loaded {
		names[n] = true
	}

	if names["x"] || names["y"] || names["z"] {
		t.Fatal("invalid manifests must be skipped")
	}
	if !names["valid"] {
		t.Fatalf("valid manifest not loaded: %v", loaded)
	}
	if !names["no-name"] {
		t.Fatal("manifest without name should default to file basename")
	}
}

// Especialista: Segurança — ReadyTimeout default sanitizado ao carregar.
func TestLoadFileDefaultsApplied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d.json")
	os.WriteFile(path, []byte(`{"name":"d","remote_port":80,"default_vram":4,"docker_image":"i","on_start_cmd":"x"}`), 0644)

	def, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if def.HealthPath != "/" || def.ReadyTimeout != 15 {
		t.Fatalf("defaults not applied: %+v", def)
	}
}
