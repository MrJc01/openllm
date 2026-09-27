package daemon

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crom-org/openllm/internal/engines"
	"github.com/crom-org/openllm/internal/proxy"
	"github.com/crom-org/openllm/internal/storage"
)

// fakeRunner simula o SSH: o comando de prontidão de cada modelo falha
// notReady[model] vezes antes de passar; registra tudo que foi executado.
type fakeRunner struct {
	mu       sync.Mutex
	def      engines.Definition
	notReady map[string]int
	cmds     []string
}

func (f *fakeRunner) RunCommand(cmd string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, cmd)
	for model, n := range f.notReady {
		if _, ready := modelCmds(f.def, model); cmd == ready && n > 0 {
			f.notReady[model] = n - 1
			return "", errors.New("not ready")
		}
	}
	return "", nil
}

func (f *fakeRunner) pulled(model string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if strings.HasPrefix(c, "nohup") && strings.Contains(c, pullLog(model)) {
			return true
		}
	}
	return false
}

func proxyModels(ps *proxy.ProxyServer) map[string]bool {
	out := map[string]bool{}
	ps.TargetsSnapshot(func(ts []proxy.Target) {
		for _, t := range ts {
			out[t.Model] = true
		}
	})
	return out
}

// seedRunning grava uma instância ollama running e seus modelos persistidos
// e a coloca nos ativos (como o passo K do backgroundSetup).
func seedRunning(t *testing.T, m *InstanceManager, db *storage.DB, id string, rows []storage.InstanceModel) storage.Instance {
	t.Helper()
	inst := storage.Instance{ID: id, Provider: "mock", Status: "running", Model: "llama3.2:3b", Engine: "ollama", GroupID: "ollama/llama3.2:3b", CreatedAt: time.Now()}
	if err := db.SaveInstance(&inst); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := db.PutInstanceModel(id, r.Model, r.Role, r.State); err != nil {
			t.Fatal(err)
		}
	}
	m.activesMu.Lock()
	m.actives[id] = &ActiveInstance{Instance: inst, LocalPort: 40100}
	m.activesMu.Unlock()
	return inst
}

// Regressão: após restart do daemon, extras persistidos voltam a ser
// roteados; cada modelo é re-verificado e só os ausentes são baixados de novo.
func TestRecoveryRestoresExtrasAndResumes(t *testing.T) {
	tests := []struct {
		name        string
		rows        []storage.InstanceModel
		notReady    map[string]int
		wantRouted  []string // logo após restore (antes da re-verificação)
		wantFinal   []string // roteados ao final
		wantPrimary string
		wantStates  map[string]string
		wantPulled  []string
		wantNoPull  []string
	}{
		{
			name: "extras prontos não são rebaixados; add interrompido é retomado",
			rows: []storage.InstanceModel{
				{Model: "llama3.2:3b", Role: storage.RolePrimary, State: "ready"},
				{Model: "nomic-embed-text", Role: storage.RoleExtra, State: "ready"},
				{Model: "qwen2.5:7b", Role: storage.RoleAdding, State: "loading"},
				{Model: "dead:1b", Role: storage.RoleAdding, State: "failed"},
			},
			notReady:    map[string]int{"qwen2.5:7b": 1},
			wantRouted:  []string{"llama3.2:3b", "nomic-embed-text"},
			wantFinal:   []string{"llama3.2:3b", "nomic-embed-text", "qwen2.5:7b"},
			wantPrimary: "llama3.2:3b",
			wantStates:  map[string]string{"nomic-embed-text": "ready", "qwen2.5:7b": "ready", "dead:1b": "failed"},
			wantPulled:  []string{"qwen2.5:7b"},
			wantNoPull:  []string{"nomic-embed-text", "dead:1b"},
		},
		{
			name: "extra sumido do host é baixado de novo",
			rows: []storage.InstanceModel{
				{Model: "nomic-embed-text", Role: storage.RoleExtra, State: "ready"},
			},
			notReady:    map[string]int{"nomic-embed-text": 1},
			wantRouted:  []string{"llama3.2:3b", "nomic-embed-text"},
			wantFinal:   []string{"llama3.2:3b", "nomic-embed-text"},
			wantPrimary: "llama3.2:3b",
			wantStates:  map[string]string{"nomic-embed-text": "ready"},
			wantPulled:  []string{"nomic-embed-text"},
		},
		{
			name: "swap interrompido é concluído",
			rows: []storage.InstanceModel{
				{Model: "llama3.2:3b", Role: storage.RolePrimary, State: "ready"},
				{Model: "qwen2.5:7b", Role: storage.RoleSwapping, State: "loading"},
			},
			wantRouted:  []string{"llama3.2:3b"},
			wantFinal:   []string{"qwen2.5:7b"},
			wantPrimary: "qwen2.5:7b",
			wantStates:  map[string]string{"qwen2.5:7b": "ready"},
			wantNoPull:  []string{"qwen2.5:7b"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, db, ps := newTestManager(t)
			inst := seedRunning(t, m, db, "r1", tc.rows)
			def := resolveEngineDef(inst)

			rows := m.restoreModels(inst)
			m.updateProxyTargets()
			routed := proxyModels(ps)
			if len(routed) != len(tc.wantRouted) {
				t.Fatalf("roteados após restore = %v, quer %v", routed, tc.wantRouted)
			}
			for _, mdl := range tc.wantRouted {
				if !routed[mdl] {
					t.Fatalf("%s não roteado após restore: %v", mdl, routed)
				}
			}

			fr := &fakeRunner{def: def, notReady: tc.notReady}
			m.resumeModels(fr, inst, def, rows).Wait()

			final := proxyModels(ps)
			if len(final) != len(tc.wantFinal) {
				t.Fatalf("roteados ao final = %v, quer %v", final, tc.wantFinal)
			}
			for _, mdl := range tc.wantFinal {
				if !final[mdl] {
					t.Fatalf("%s não roteado ao final: %v", mdl, final)
				}
			}
			snap := m.GetActiveInstances()[0]
			if snap.Model != tc.wantPrimary {
				t.Fatalf("principal = %s, quer %s", snap.Model, tc.wantPrimary)
			}
			persisted, _ := db.ListInstanceModels("r1")
			pstate := map[string]string{}
			for _, r := range persisted {
				pstate[r.Model] = r.State
			}
			for mdl, st := range tc.wantStates {
				if snap.ModelStates[mdl] != st || pstate[mdl] != st {
					t.Fatalf("%s: memória=%q banco=%q, quer %q", mdl, snap.ModelStates[mdl], pstate[mdl], st)
				}
			}
			for _, mdl := range tc.wantPulled {
				if !fr.pulled(mdl) {
					t.Fatalf("%s deveria ter o pull relançado", mdl)
				}
			}
			for _, mdl := range tc.wantNoPull {
				if fr.pulled(mdl) {
					t.Fatalf("%s não deveria ser baixado de novo", mdl)
				}
			}
			if got, _ := db.GetInstance("r1"); got.Model != tc.wantPrimary {
				t.Fatalf("banco: principal = %s, quer %s", got.Model, tc.wantPrimary)
			}
		})
	}
}

// Instância de antes da tabela (sem linhas) ganha a linha do principal.
func TestRestoreModelsBackfillsPrimary(t *testing.T) {
	m, db, _ := newTestManager(t)
	inst := seedRunning(t, m, db, "legacy", nil)
	m.restoreModels(inst)
	rows, _ := db.ListInstanceModels("legacy")
	if len(rows) != 1 || rows[0].Model != inst.Model || rows[0].Role != storage.RolePrimary {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestAddRemoveSwapPersistence(t *testing.T) {
	m, db, ps := newTestManager(t)
	seedRunning(t, m, db, "p1", []storage.InstanceModel{{Model: "llama3.2:3b", Role: storage.RolePrimary, State: "ready"}})

	roles := func() map[string]string {
		rows, _ := db.ListInstanceModels("p1")
		out := map[string]string{}
		for _, r := range rows {
			out[r.Model] = r.Role + "/" + r.State
		}
		return out
	}

	m.persistModel("p1", "nomic-embed-text", storage.RoleAdding, "loading")
	m.finishAdd("p1", "nomic-embed-text")
	if r := roles(); r["nomic-embed-text"] != "extra/ready" {
		t.Fatalf("add: %v", r)
	}
	if !proxyModels(ps)["nomic-embed-text"] {
		t.Fatal("extra não roteado")
	}

	if err := m.RemoveModel("p1", "llama3.2:3b"); err == nil {
		t.Fatal("remover o principal deveria falhar")
	}
	if err := m.RemoveModel("p1", "nomic-embed-text"); err != nil {
		t.Fatal(err)
	}
	if r := roles(); len(r) != 1 {
		t.Fatalf("remove deveria apagar a linha: %v", r)
	}
	if proxyModels(ps)["nomic-embed-text"] {
		t.Fatal("extra removido ainda roteado")
	}

	m.persistModel("p1", "qwen2.5:7b", storage.RoleSwapping, "loading")
	m.finishSwap("p1", "qwen2.5:7b")
	if r := roles(); len(r) != 1 || r["qwen2.5:7b"] != "primary/ready" {
		t.Fatalf("swap: %v", r)
	}
	if got, _ := db.GetInstance("p1"); got.Model != "qwen2.5:7b" || got.GroupID != "ollama/qwen2.5:7b" {
		t.Fatalf("swap não gravou a instância: %+v", got)
	}

	// Stop limpa as linhas junto com a instância.
	if err := m.StopInstance(testConfig(), "p1"); err != nil {
		t.Logf("stop (mock): %v", err)
	}
	if r := roles(); len(r) != 0 {
		t.Fatalf("stop deveria limpar instance_models: %v", r)
	}
}

func TestMergeModelStatus(t *testing.T) {
	inst := storage.Instance{ID: "i", Model: "p"}
	rows := []storage.InstanceModel{
		{Model: "p", Role: storage.RolePrimary, State: "ready"},
		{Model: "e", Role: storage.RoleExtra, State: "ready"},
		{Model: "a", Role: storage.RoleAdding, State: "failed"},
	}
	live := &ActiveInstance{Instance: inst, ExtraModels: []string{"e"}, ModelStates: map[string]string{"a": "loading"}}
	tests := []struct {
		name       string
		rows       []storage.InstanceModel
		live       *ActiveInstance
		wantModels string
		wantStates map[string]string
	}{
		{"só banco", rows, nil, "p,e", map[string]string{"p": "ready", "e": "ready", "a": "failed"}},
		{"memória sobrepõe", rows, live, "p,e", map[string]string{"p": "ready", "e": "ready", "a": "loading"}},
		{"sem nada", nil, nil, "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			models, states := mergeModelStatus(inst, tc.rows, tc.live)
			if strings.Join(models, ",") != tc.wantModels {
				t.Fatalf("models = %v", models)
			}
			if fmt.Sprint(states) != fmt.Sprint(tc.wantStates) {
				t.Fatalf("states = %v, quer %v", states, tc.wantStates)
			}
		})
	}
}

// Rodar com -race: todas as mutações de modelos concorrendo com leitores.
func TestModelStateConcurrency(t *testing.T) {
	m, db, _ := newTestManager(t)
	seedRunning(t, m, db, "c1", nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mdl := fmt.Sprintf("m%d", i%3)
			for j := 0; j < 20; j++ {
				m.setModelState("c1", mdl, "loading")
				m.setModelProgress("c1", mdl, "50%")
				switch j % 4 {
				case 0:
					m.finishAdd("c1", mdl)
				case 1:
					_ = m.RemoveModel("c1", mdl)
				case 2:
					_ = m.AddModel("c1", mdl) // falha (sem pull via SSH real) mas lê o estado
				case 3:
					m.updateProxyTargets()
				}
				for _, a := range m.GetActiveInstances() {
					_ = a.Models()
					_ = len(a.ModelStates)
				}
			}
		}(i)
	}
	wg.Wait()
}
