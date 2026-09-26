package engines

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetCustomFallsBackSafely(t *testing.T) {
	// G1 corrigido: capabilities "text" sozinho → ollama (não mais custom→ollama quebrado)
	def := GetCustom("", []string{"text"}, "", "", 0)
	if def.Name != "ollama" {
		t.Fatalf("expected ollama for text-only caps, got %s", def.Name)
	}

	def = GetCustom("", []string{"image"}, "", "", 0)
	if def.Name != "localai-image" {
		t.Fatalf("expected localai-image for image caps, got %s", def.Name)
	}

	def = GetCustom("", []string{"audio"}, "", "", 0)
	if def.Name != "faster-whisper" {
		t.Fatalf("expected faster-whisper for audio caps, got %s", def.Name)
	}

	def = GetCustom("", []string{"text", "image"}, "", "", 0)
	if def.Name != "localai" {
		t.Fatalf("expected localai for multimodal caps, got %s", def.Name)
	}
}

func TestGetCustomWithImage(t *testing.T) {
	def := GetCustom("", nil, "foo/bar:latest", "run --x", 9000)
	if def.Name != "custom" || def.DockerImage != "foo/bar:latest" || def.RemotePort != 9000 {
		t.Fatalf("unexpected custom def: %+v", def)
	}
	if def.HealthPath != "/" {
		t.Fatalf("custom engine should have default health path, got %q", def.HealthPath)
	}
}

func TestRenderModelPlaceholder(t *testing.T) {
	got := Render("python -m vllm --model {{.Model}} &", "Qwen/Qwen2.5")
	want := "python -m vllm --model Qwen/Qwen2.5 &"
	if got != want {
		t.Fatalf("Render = %q, want %q", got, want)
	}
	if Render("pull {{model}}", "m1") != "pull m1" {
		t.Fatal("lowercase placeholder should work")
	}
}

func TestExternalManifests(t *testing.T) {
	dir := t.TempDir()
	manifest := `{
		"name": "test-engine",
		"remote_port": 9090,
		"default_vram": 10,
		"docker_image": "test/engine:latest",
		"on_start_cmd": "test --model {{.Model}} &",
		"health_path": "/healthz"
	}`
	if err := os.WriteFile(filepath.Join(dir, "test-engine.json"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}

	loaded := LoadExternal(dir)
	found := false
	for _, name := range loaded {
		if name == "test-engine" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected test-engine to be loaded, got %v", loaded)
	}

	def, ok := TryGet("test-engine")
	if !ok || def.RemotePort != 9090 || def.HealthPath != "/healthz" {
		t.Fatalf("external engine not registered correctly: %+v", def)
	}

	// .json.example não deve ser carregado (apenas *.json)
	if err := os.WriteFile(filepath.Join(dir, "skip.json.example"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if len(LoadExternal(dir)) != len(loaded) {
		t.Fatal("non-.json files should not be loaded")
	}
}

func TestRemotePortOverride(t *testing.T) {
	if RemotePort("", 12345) != 12345 {
		t.Fatal("empty engine should use configured default port")
	}
	if RemotePort("ollama", 12345) != 12345 {
		t.Fatal("ollama should use configured default port")
	}
	if RemotePort("comfyui", 12345) != 18188 {
		t.Fatal("comfyui should use its own port")
	}
}
