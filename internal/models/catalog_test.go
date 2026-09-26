package models

import "testing"

func TestResolveCatalogExact(t *testing.T) {
	entry, ok := ResolveCatalog("sdxl")
	if !ok || entry.Engine != "comfyui" || entry.Modality != "image" {
		t.Fatalf("expected sdxl→comfyui/image, got %+v ok=%v", entry, ok)
	}
}

func TestResolveCatalogAlias(t *testing.T) {
	entry, ok := ResolveCatalog("stable-diffusion-xl")
	if !ok || entry.Engine != "comfyui" {
		t.Fatalf("expected alias to resolve, got %+v ok=%v", entry, ok)
	}
}

func TestResolveCatalogPrefix(t *testing.T) {
	entry, ok := ResolveCatalog("sdxl-turbo-v2")
	if !ok || entry.Engine != "comfyui" {
		t.Fatalf("expected prefix match, got %+v ok=%v", entry, ok)
	}
}

func TestResolveCatalogMiss(t *testing.T) {
	if _, ok := ResolveCatalog("totally-unknown-model"); ok {
		t.Fatal("expected miss for unknown model")
	}
}

func TestCatalogModality(t *testing.T) {
	cases := map[string]string{
		"deepseek-r1:7b":     "text",
		"sdxl":               "image",
		"whisper-large-v3":   "asr",
		"ltx-video-2b":       "video",
		"xtts-v2":            "tts",
		"nomic-embed-text":   "embedding",
		"some-voice-thing":   "tts",
		"mystery-model-2000": "text", // heurística default
	}
	for model, want := range cases {
		if got := CatalogModality(model); got != want {
			t.Errorf("CatalogModality(%q) = %q, want %q", model, got, want)
		}
	}
}
