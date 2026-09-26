package daemon

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/crom-org/openllm/internal/engines"
	"github.com/crom-org/openllm/internal/models"
)

func shOK(t *testing.T, cmd string) {
	t.Helper()
	if out, err := exec.Command("sh", "-n", "-c", cmd).CombinedOutput(); err != nil {
		t.Fatalf("sintaxe inválida: %v %s\n%s", err, out, cmd)
	}
}

func TestModelCmdsFromCatalogFiles(t *testing.T) {
	def := engines.Get("comfyui")
	for _, m := range []string{"sdxl", "wan2.1-1.3b", "ltx-video-2b", "z-image-turbo", "wan2.2-ti2v-5b", "ace-step-1.5", "trellis2", "qwen-image-2512", "stable-audio-open", "flux2-klein-4b"} {
		e, ok := models.ResolveCatalog(m)
		if !ok || len(e.Files) == 0 {
			t.Fatalf("%s sem files no catálogo", m)
		}
		pull, ready := modelCmds(def, m)
		shOK(t, pull)
		shOK(t, ready)
		for _, f := range e.Files {
			if !strings.Contains(pull, f.URL) || !strings.Contains(ready, f.Name()) {
				t.Fatalf("%s: arquivo %s fora dos comandos", m, f.Name())
			}
			if !strings.HasPrefix(f.URL, "https://huggingface.co/") || !strings.HasSuffix(f.Name(), ".safetensors") || !strings.HasSuffix(f.URL, ".safetensors") {
				t.Fatalf("%s: URL estranha %s", m, f.URL)
			}
		}
		if !strings.Contains(pull, "main[.]py") {
			t.Fatalf("%s: pull não usa a pasta do ComfyUI em execução", m)
		}
	}
}

func TestModelCmdsFallbacks(t *testing.T) {
	def := engines.Get("comfyui")
	if _, ready := modelCmds(def, "sd-1.5"); !strings.Contains(ready, "v1-5-pruned-emaonly") {
		t.Fatal("sd-1.5 deveria usar a checagem embutida")
	}
	if pull, ready := modelCmds(def, "modelo-inexistente"); pull != "true" || !strings.Contains(ready, "exit 1") {
		t.Fatalf("desconhecido: pull=%q ready=%q", pull, ready)
	}
	if pull, _ := modelCmds(engines.Get("ollama"), "llama3.2:3b"); !strings.Contains(pull, "ollama pull llama3.2:3b") {
		t.Fatalf("ollama: %q", pull)
	}
}

func TestModelCmdsServeAs(t *testing.T) {
	pull, ready := modelCmds(engines.Get("speaches"), "whisper-large-v3-turbo")
	for _, c := range []string{pull, ready} {
		shOK(t, c)
		if !strings.Contains(c, "deepdml/faster-whisper-large-v3-turbo-ct2") || strings.Contains(c, "{{") {
			t.Fatalf("serve_as não aplicado: %q", c)
		}
	}
	if _, ready := modelCmds(engines.Get("kokoro"), "kokoro"); !strings.Contains(ready, "/v1/audio/speech") {
		t.Fatalf("kokoro ready: %q", ready)
	}
}
