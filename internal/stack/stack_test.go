package stack

import "testing"

const validYAML = `
name: multimodal
services:
  - name: llm
    model: deepseek-r1:7b
    instances: 2
    routes:
      - /api/chat
  - name: image
    engine: comfyui
    model: sdxl
`

func TestParseValid(t *testing.T) {
	st, err := Parse([]byte(validYAML), "fallback")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.Name != "multimodal" || len(st.Services) != 2 {
		t.Fatalf("unexpected stack: %+v", st)
	}
	if st.Services[0].Instances != 2 {
		t.Fatal("instances should be parsed")
	}
	// Service sem engine → resolvida depois (no daemon), deve manter vazio
	if st.Services[1].Engine != "comfyui" {
		t.Fatal("engine should be preserved")
	}
}

func TestParseNameFallback(t *testing.T) {
	st, err := Parse([]byte("services:\n  - model: m1\n"), "from-file")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.Name != "from-file" {
		t.Fatalf("expected fallback name, got %q", st.Name)
	}
}

func TestParseRejectsBadRoute(t *testing.T) {
	yaml := "name: x\nservices:\n  - model: m1\n    routes: [chat]\n"
	if _, err := Parse([]byte(yaml), "x"); err == nil {
		t.Fatal("routes must start with '/'")
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	if _, err := Parse([]byte("name: x\nservices: []\n"), "x"); err == nil {
		t.Fatal("stack without services should fail")
	}
	if _, err := Parse([]byte("services:\n  - model: m1\n"), ""); err == nil {
		t.Fatal("stack without name and no fallback should fail")
	}
}
