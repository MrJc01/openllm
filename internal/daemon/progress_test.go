package daemon

import (
	"os/exec"
	"strings"
	"testing"
)

func TestSummarizePullProgressCurl(t *testing.T) {
	raw := "  % Total    % Received % Xferd  Average Speed   Time    Time     Time  Current\n" +
		"                                 Dload  Upload   Total   Spent    Left  Speed\n" +
		" 45 6046M   45 2763M    0     0  18.5M      0  0:05:26  0:02:10  0:03:16 17.9M\n" +
		"--parts--\n2763\t/workspace/ComfyUI/models/checkpoints/sd_xl_turbo_1.0_fp16.safetensors.part\n"
	got := summarizePullProgress(raw)
	for _, want := range []string{"45%", "2763M de 6046M", "17.9M/s", "falta 0:03:16", "sd_xl_turbo_1.0_fp16.safetensors (2763 MB)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("faltou %q em %q", want, got)
		}
	}
}

func TestSummarizePullProgressOllamaAndEmpty(t *testing.T) {
	raw := "pulling manifest\n\x1b[?25lpulling 6a0746a1ec1a...  45% ▕████      ▏ 2.1 GB/4.7 GB   30 MB/s   1m20s\n--parts--\n"
	if got := summarizePullProgress(raw); !strings.Contains(got, "45%") || strings.Contains(got, "\x1b") {
		t.Fatalf("ollama: %q", got)
	}
	if got := summarizePullProgress("--parts--\n"); got != "" {
		t.Fatalf("vazio: %q", got)
	}
}

func TestPullLogAndProgressCmdPerModel(t *testing.T) {
	if got := pullLog("Qwen/Qwen2.5 7B:q4"); got != "/var/log/openllm-pull-Qwen_Qwen2.5_7B_q4.log" {
		t.Fatalf("pullLog = %q", got)
	}
	cmd := pullProgressCmd("wan2.2-ti2v-5b", []string{"wan2.2_vae.safetensors", "x'y.safetensors"})
	if out, err := exec.Command("sh", "-n", "-c", cmd).CombinedOutput(); err != nil {
		t.Fatalf("sintaxe: %v %s\n%s", err, out, cmd)
	}
	if !strings.Contains(cmd, "openllm-pull-wan2.2-ti2v-5b.log") || !strings.Contains(cmd, "wan2.2_vae.safetensors") {
		t.Fatalf("progresso não filtra pelo modelo: %s", cmd)
	}
}
