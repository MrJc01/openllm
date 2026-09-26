package storage

import (
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// setupDB prepara um diretório temporário com banco novo (ou legado) e
// retorna o cwd original para restore.
func setupDB(t *testing.T, legacy bool) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(oldWd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(".openllm", 0755); err != nil {
		t.Fatal(err)
	}

	if legacy {
		// Cria manualmente o schema ANTIGO (sem engine, group_id, def_json)
		conn, err := sql.Open("sqlite3", ".openllm/openllm.db")
		if err != nil {
			t.Fatal(err)
		}
		_, err = conn.Exec(`CREATE TABLE instances (
			id TEXT PRIMARY KEY, provider TEXT, machine_id TEXT, gpu TEXT,
			gpu_count INTEGER, vram REAL, cost_per_hour REAL, ssh_host TEXT,
			ssh_port INTEGER, status TEXT, model TEXT, created_at DATETIME, stopped_at DATETIME
		);`)
		if err != nil {
			t.Fatal(err)
		}
		_, err = conn.Exec(`INSERT INTO instances (id, provider, machine_id, gpu, gpu_count, vram, cost_per_hour, ssh_host, ssh_port, status, model, created_at)
			VALUES ('old-1', 'vastai', 'm-1', 'RTX 3090', 1, 24, 0.2, '1.2.3.4', 40001, 'running', 'llama3.2:3b', '2025-01-01 10:00:00')`)
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}

	db, err := OpenDB()
	if err != nil {
		t.Fatalf("OpenDB failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, dir
}

// Especialista: DBA — instância legada (schema pré-engine) deve sobreviver à
// migração e aceitar updates com as colunas novas.
func TestMigrationFromLegacySchema(t *testing.T) {
	db, _ := setupDB(t, true)

	inst, err := db.GetInstance("old-1")
	if err != nil {
		t.Fatalf("legacy instance unreadable after migration: %v", err)
	}
	if inst == nil || inst.Model != "llama3.2:3b" {
		t.Fatalf("unexpected legacy instance: %+v", inst)
	}
	if inst.Engine == "" {
		t.Fatal("engine column migration should yield default value")
	}

	// Update com colunas novas deve funcionar
	inst.Engine = "comfyui"
	inst.GroupID = "comfyui/llama3.2:3b"
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("save with new columns failed: %v", err)
	}
	got, _ := db.GetInstance("old-1")
	if got.Engine != "comfyui" || got.GroupID != "comfyui/llama3.2:3b" {
		t.Fatalf("roundtrip failed: %+v", got)
	}
}

// Especialista: DBA — persistência completa (EngineDefJSON sobrevive a restart).
func TestInstanceDefJSONRoundtrip(t *testing.T) {
	db, _ := setupDB(t, false)

	inst := &Instance{
		ID:            "def-1",
		Provider:      "mock",
		Status:        "running",
		Model:         "sdxl",
		Engine:        "comfyui",
		GroupID:       "comfyui/sdxl",
		EngineDefJSON: `{"name":"comfyui","remote_port":8188,"docker_image":"x"}`,
		CreatedAt:     time.Now(),
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetInstance("def-1")
	if err != nil || got == nil {
		t.Fatalf("instance lost: %v", err)
	}
	if got.EngineDefJSON != inst.EngineDefJSON {
		t.Fatalf("def_json not persisted: %q", got.EngineDefJSON)
	}
	if got.GroupID != "comfyui/sdxl" {
		t.Fatalf("group_id not persisted: %q", got.GroupID)
	}
}

// Especialista: SRE — contagem por grupo e listagem por grupo.
func TestGroupQueriesAndCounting(t *testing.T) {
	db, _ := setupDB(t, false)

	mk := func(id, group, status string, cost float64) {
		db.SaveInstance(&Instance{ID: id, Provider: "mock", Status: status, Model: "m", Engine: "e", GroupID: group, CostPerHour: cost, CreatedAt: time.Now()})
	}
	mk("g1", "ollama/m", "running", 0.10)
	mk("g2", "ollama/m", "deploying", 0.20)
	mk("g3", "ollama/m", "failed", 0.30)
	mk("g4", "comfyui/sdxl", "running", 0.40)

	count, err := db.CountActiveByGroup("ollama/m")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 active (running+deploying), got %d", count)
	}

	byGroup, err := db.ListInstancesByGroup("ollama/m")
	if err != nil {
		t.Fatal(err)
	}
	if len(byGroup) != 3 {
		t.Fatalf("expected 3 instances in group (incl. failed), got %d", len(byGroup))
	}
}

// Especialista: QA — rotas de stack persistem e fazem roundtrip JSON.
func TestGroupRoutesRoundtrip(t *testing.T) {
	db, _ := setupDB(t, false)

	g := &Group{ID: "comfyui/sdxl", Stack: "multimodal", Routes: []string{"/v1/images/generations", "/v1/images/edit"}}
	if err := db.SaveGroup(g); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetGroup("comfyui/sdxl")
	if err != nil || got == nil {
		t.Fatalf("group lost: %v", err)
	}
	if got.Stack != "multimodal" || len(got.Routes) != 2 || got.Routes[0] != "/v1/images/generations" {
		t.Fatalf("routes roundtrip failed: %+v", got)
	}

	// Update substitui rotas
	got.Routes = []string{"/only-one"}
	db.SaveGroup(got)
	again, _ := db.GetGroup("comfyui/sdxl")
	if len(again.Routes) != 1 || again.Routes[0] != "/only-one" {
		t.Fatalf("route update failed: %+v", again)
	}

	// Delete
	if err := db.DeleteGroup("comfyui/sdxl"); err != nil {
		t.Fatal(err)
	}
	if g, _ := db.GetGroup("comfyui/sdxl"); g != nil {
		t.Fatal("group should be deleted")
	}
}
