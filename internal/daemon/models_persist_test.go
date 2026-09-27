package daemon

import (
	"context"
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
// notReady[model] vezes antes de passar (-1 = sempre falha).
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
		if _, ready := modelCmds(f.def, model); cmd == ready && n != 0 {
			if n > 0 {
				f.notReady[model] = n - 1
			}
			return "", errors.New("not ready")
		}
	}
	return "", nil
}

func (f *fakeRunner) set(model string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notReady == nil {
		f.notReady = map[string]int{}
	}
	f.notReady[model] = n
}

func (f *fakeRunner) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cmds)
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

func fastModelTimers(m *InstanceManager) {
	m.modelWait = 300 * time.Millisecond
	m.modelPoll = 5 * time.Millisecond
	m.reconcileEvery = 10 * time.Millisecond
}

// seedRunning grava uma instância ollama running e seus modelos persistidos e
// a coloca nos ativos (como o passo K do backgroundSetup).
func seedRunning(t *testing.T, m *InstanceManager, db *storage.DB, id string, rows []storage.InstanceModel) (*ActiveInstance, context.CancelFunc) {
	t.Helper()
	inst := storage.Instance{ID: id, Provider: "mock", Status: "running", Model: "llama3.2:3b", Engine: "ollama", GroupID: "ollama/llama3.2:3b", CreatedAt: time.Now()}
	if err := db.SaveInstance(&inst); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := db.PutInstanceModel(id, r.Model, r.Role, r.State, r.Detail, 1); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	act := &ActiveInstance{Instance: inst, LocalPort: 40100, ctx: ctx, CancelFunc: cancel}
	m.activesMu.Lock()
	m.actives[id] = act
	m.activesMu.Unlock()
	return act, cancel
}

func dbRows(t *testing.T, db *storage.DB, id string) map[string]storage.InstanceModel {
	t.Helper()
	rows, err := db.ListInstanceModels(id)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]storage.InstanceModel{}
	for _, r := range rows {
		out[r.Model] = r
	}
	return out
}

// Findings 1/3: após restart, extras só voltam ao proxy depois de
// confirmados no host; só os ausentes são baixados; add/swap retomados;
// falhas terminais ficam de fora.
func TestRecoveryRestoresExtrasAndResumes(t *testing.T) {
	tests := []struct {
		name        string
		rows        []storage.InstanceModel
		notReady    map[string]int
		wantFinal   []string
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
				{Model: "dead:1b", Role: storage.RoleAdding, State: "failed", Detail: "boom"},
			},
			notReady:    map[string]int{"qwen2.5:7b": 1},
			wantFinal:   []string{"llama3.2:3b", "nomic-embed-text", "qwen2.5:7b"},
			wantPrimary: "llama3.2:3b",
			wantStates:  map[string]string{"nomic-embed-text": "ready", "qwen2.5:7b": "ready", "dead:1b": "failed"},
			wantPulled:  []string{"qwen2.5:7b"},
			wantNoPull:  []string{"nomic-embed-text", "dead:1b"},
		},
		{
			name:        "extra sumido do host é baixado de novo",
			rows:        []storage.InstanceModel{{Model: "nomic-embed-text", Role: storage.RoleExtra, State: "ready"}},
			notReady:    map[string]int{"nomic-embed-text": 1},
			wantFinal:   []string{"llama3.2:3b", "nomic-embed-text"},
			wantPrimary: "llama3.2:3b",
			wantStates:  map[string]string{"nomic-embed-text": "ready"},
			wantPulled:  []string{"nomic-embed-text"},
		},
		{
			name:        "extra que nunca fica pronto não é roteado e falha com motivo",
			rows:        []storage.InstanceModel{{Model: "nomic-embed-text", Role: storage.RoleExtra, State: "ready"}},
			notReady:    map[string]int{"nomic-embed-text": -1},
			wantFinal:   []string{"llama3.2:3b"},
			wantPrimary: "llama3.2:3b",
			wantStates:  map[string]string{"nomic-embed-text": "failed"},
			wantPulled:  []string{"nomic-embed-text"},
		},
		{
			name: "swap interrompido é concluído",
			rows: []storage.InstanceModel{
				{Model: "llama3.2:3b", Role: storage.RolePrimary, State: "ready"},
				{Model: "qwen2.5:7b", Role: storage.RoleSwapping, State: "loading"},
			},
			wantFinal:   []string{"qwen2.5:7b"},
			wantPrimary: "qwen2.5:7b",
			wantStates:  map[string]string{"qwen2.5:7b": "ready"},
			wantNoPull:  []string{"qwen2.5:7b"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, db, ps := newTestManager(t)
			fastModelTimers(m)
			act, _ := seedRunning(t, m, db, "r1", tc.rows)
			def := resolveEngineDef(act.Instance)

			rows, err := m.restoreModels(act)
			if err != nil {
				t.Fatal(err)
			}
			m.updateProxyTargets()
			// Antes da re-verificação só o principal é roteado.
			if routed := proxyModels(ps); len(routed) != 1 || !routed["llama3.2:3b"] {
				t.Fatalf("roteados após restore = %v, quer só o principal", routed)
			}

			fr := &fakeRunner{def: def, notReady: tc.notReady}
			m.resumeModels(act.context(), act, fr, def, rows).Wait()

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
			persisted := dbRows(t, db, "r1")
			for mdl, st := range tc.wantStates {
				if snap.ModelStates[mdl] != st || persisted[mdl].State != st {
					t.Fatalf("%s: memória=%q banco=%q, quer %q", mdl, snap.ModelStates[mdl], persisted[mdl].State, st)
				}
				if st == "failed" && (persisted[mdl].Detail == "" || snap.ModelDetails[mdl] == "") {
					t.Fatalf("%s: falha sem motivo (banco=%q memória=%q)", mdl, persisted[mdl].Detail, snap.ModelDetails[mdl])
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

// Finding 1: extra restaurado só entra no proxy quando o host confirma.
func TestRestoredExtraRoutedOnlyWhenReady(t *testing.T) {
	m, db, ps := newTestManager(t)
	m.modelWait, m.modelPoll = 5*time.Second, 5*time.Millisecond
	act, _ := seedRunning(t, m, db, "r2", []storage.InstanceModel{{Model: "nomic-embed-text", Role: storage.RoleExtra, State: "ready"}})
	def := resolveEngineDef(act.Instance)
	rows, _ := m.restoreModels(act)
	fr := &fakeRunner{def: def, notReady: map[string]int{"nomic-embed-text": -1}}
	wg := m.resumeModels(act.context(), act, fr, def, rows)

	time.Sleep(50 * time.Millisecond) // re-verificação em curso, ainda falhando
	m.updateProxyTargets()
	if proxyModels(ps)["nomic-embed-text"] {
		t.Fatal("extra não verificado não pode ser roteado")
	}
	fr.set("nomic-embed-text", 0) // host passa a ter o modelo
	wg.Wait()
	if !proxyModels(ps)["nomic-embed-text"] {
		t.Fatal("extra verificado deveria ser roteado")
	}
}

// Instância de antes da tabela (sem linhas) ganha a linha do principal.
func TestRestoreModelsBackfillsPrimary(t *testing.T) {
	m, db, _ := newTestManager(t)
	act, _ := seedRunning(t, m, db, "legacy", nil)
	if _, err := m.restoreModels(act); err != nil {
		t.Fatal(err)
	}
	rows := dbRows(t, db, "legacy")
	if len(rows) != 1 || rows[act.Model].Role != storage.RolePrimary {
		t.Fatalf("rows = %+v", rows)
	}
}

// Finding 6: erro de leitura do banco é devolvido e o principal fica "unknown".
func TestRestoreModelsErrorMarksUnknown(t *testing.T) {
	m, db, _ := newTestManager(t)
	act, _ := seedRunning(t, m, db, "e1", nil)
	db.Close()
	if _, err := m.restoreModels(act); err == nil {
		t.Fatal("erro de leitura deveria ser devolvido")
	}
	if st := m.GetActiveInstances()[0].ModelStates[act.Model]; st != "unknown" {
		t.Fatalf("estado = %q, quer unknown", st)
	}
}

// Finding 2: stop durante a re-verificação encerra a goroutine logo (sem
// novos comandos) e não mexe numa nova encarnação com o mesmo id.
func TestStopDuringResumeStopsPromptlyAndSparesNewIncarnation(t *testing.T) {
	m, db, _ := newTestManager(t)
	m.modelWait, m.modelPoll = time.Minute, 5*time.Millisecond
	act, cancel := seedRunning(t, m, db, "s1", []storage.InstanceModel{{Model: "nomic-embed-text", Role: storage.RoleExtra, State: "ready"}})
	def := resolveEngineDef(act.Instance)
	rows, _ := m.restoreModels(act)
	fr := &fakeRunner{def: def, notReady: map[string]int{"nomic-embed-text": -1}}
	wg := m.resumeModels(act.context(), act, fr, def, rows)
	time.Sleep(30 * time.Millisecond)

	// Nova encarnação (reconexão) com o mesmo id, depois cancela a antiga.
	next := &ActiveInstance{Instance: act.Instance, LocalPort: 40200}
	m.activesMu.Lock()
	m.actives["s1"] = next
	m.activesMu.Unlock()
	cancel()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("goroutine de resume não parou após cancelamento")
	}
	n := fr.calls()
	time.Sleep(50 * time.Millisecond)
	if fr.calls() != n {
		t.Fatal("comandos SSH continuaram após o stop")
	}
	if st := m.GetActiveInstances()[0].ModelStates; len(st) != 0 {
		t.Fatalf("encarnação nova foi alterada: %v", st)
	}
	if r := dbRows(t, db, "s1")["nomic-embed-text"]; r.State == "failed" {
		t.Fatal("cancelamento não é falha do modelo")
	}
}

// Finding 3: remove durante um add em curso continua removido (nada é
// ressuscitado por setModelState/finishAdd atrasados).
func TestRemoveDuringAddStaysRemoved(t *testing.T) {
	m, db, ps := newTestManager(t)
	act, _ := seedRunning(t, m, db, "a1", []storage.InstanceModel{{Model: "llama3.2:3b", Role: storage.RolePrimary, State: "ready"}})

	m.activesMu.Lock()
	act.ModelStates = map[string]string{"qwen2.5:7b": "loading"}
	m.activesMu.Unlock()
	if err := m.trackTarget(act, "qwen2.5:7b", storage.RoleAdding); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveModel("a1", "qwen2.5:7b"); err != nil {
		t.Fatal(err)
	}
	m.setModelState(act, "qwen2.5:7b", "ready", "") // progresso atrasado do pull
	m.finishAdd(act, "qwen2.5:7b")

	if _, ok := dbRows(t, db, "a1")["qwen2.5:7b"]; ok {
		t.Fatal("linha ressuscitada após remove")
	}
	if proxyModels(ps)["qwen2.5:7b"] {
		t.Fatal("modelo removido foi roteado")
	}
	if _, ok := m.GetActiveInstances()[0].ModelStates["qwen2.5:7b"]; ok {
		t.Fatal("estado em memória ressuscitado")
	}
}

func TestAddRemoveSwapPersistence(t *testing.T) {
	m, db, ps := newTestManager(t)
	act, _ := seedRunning(t, m, db, "p1", []storage.InstanceModel{{Model: "llama3.2:3b", Role: storage.RolePrimary, State: "ready"}})
	track := func(model, role string) {
		m.activesMu.Lock()
		ensureMaps(act)
		act.ModelStates[model] = "loading"
		m.activesMu.Unlock()
		if err := m.trackTarget(act, model, role); err != nil {
			t.Fatal(err)
		}
	}
	roles := func() map[string]string {
		out := map[string]string{}
		for k, r := range dbRows(t, db, "p1") {
			out[k] = r.Role + "/" + r.State
		}
		return out
	}

	track("nomic-embed-text", storage.RoleAdding)
	if r := roles(); r["nomic-embed-text"] != "adding/loading" {
		t.Fatalf("add em curso: %v", r)
	}
	m.finishAdd(act, "nomic-embed-text")
	if r := roles(); r["nomic-embed-text"] != "extra/ready" || !proxyModels(ps)["nomic-embed-text"] {
		t.Fatalf("add: %v", r)
	}
	if err := m.RemoveModel("p1", "llama3.2:3b"); err == nil {
		t.Fatal("remover o principal deveria falhar")
	}
	if err := m.RemoveModel("p1", "nomic-embed-text"); err != nil {
		t.Fatal(err)
	}
	if r := roles(); len(r) != 1 || proxyModels(ps)["nomic-embed-text"] {
		t.Fatalf("remove: %v", r)
	}

	track("qwen2.5:7b", storage.RoleSwapping)
	m.finishSwap(act, "qwen2.5:7b")
	if r := roles(); len(r) != 1 || r["qwen2.5:7b"] != "primary/ready" {
		t.Fatalf("swap: %v", r)
	}
	if got, _ := db.GetInstance("p1"); got.Model != "qwen2.5:7b" || got.GroupID != "ollama/qwen2.5:7b" {
		t.Fatalf("swap não gravou a instância: %+v", got)
	}

	// Stop limpa as linhas junto com a instância; swap tardio não recria nada.
	if err := m.StopInstance(testConfig(), "p1"); err != nil {
		t.Logf("stop (mock): %v", err)
	}
	if r := roles(); len(r) != 0 {
		t.Fatalf("stop deveria limpar instance_models: %v", r)
	}
	if err := db.CommitSwap("p1", "qwen2.5:7b", "x", "ollama/x", m.nextVersion()); !errors.Is(err, storage.ErrModelGone) {
		t.Fatalf("CommitSwap após stop = %v, quer ErrModelGone", err)
	}
	if got, _ := db.GetInstance("p1"); got != nil {
		t.Fatal("swap tardio recriou a instância")
	}
}

// Reconcile: extra que some do host sai do proxy; quando volta, retorna.
func TestReconcileDropsAndRestoresExtras(t *testing.T) {
	m, db, ps := newTestManager(t)
	act, _ := seedRunning(t, m, db, "c1", []storage.InstanceModel{{Model: "nomic-embed-text", Role: storage.RoleExtra, State: "ready"}})
	m.activesMu.Lock()
	act.ExtraModels = []string{"nomic-embed-text"}
	act.ModelStates = map[string]string{"nomic-embed-text": "ready"}
	m.activesMu.Unlock()
	m.updateProxyTargets()
	def := resolveEngineDef(act.Instance)
	fr := &fakeRunner{def: def, notReady: map[string]int{"nomic-embed-text": -1}}

	m.reconcileOnce(act.context(), act, fr, def)
	if proxyModels(ps)["nomic-embed-text"] {
		t.Fatal("extra ausente do host continuou roteado")
	}
	if r := dbRows(t, db, "c1")["nomic-embed-text"]; r.State != "failed" || r.Detail == "" {
		t.Fatalf("banco: %+v", r)
	}
	fr.set("nomic-embed-text", 0)
	m.reconcileOnce(act.context(), act, fr, def)
	if !proxyModels(ps)["nomic-embed-text"] {
		t.Fatal("extra de volta ao host deveria voltar ao proxy")
	}

	// O loop para com o contexto.
	fastModelTimers(m)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.reconcileLoop(ctx, act, fr, def); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconcileLoop não parou")
	}
}

func TestMergeModelStatus(t *testing.T) {
	inst := storage.Instance{ID: "i", Model: "p"}
	rows := []storage.InstanceModel{
		{Model: "p", Role: storage.RolePrimary, State: "ready"},
		{Model: "e", Role: storage.RoleExtra, State: "ready"},
		{Model: "a", Role: storage.RoleAdding, State: "failed", Detail: "timeout"},
	}
	live := &ActiveInstance{Instance: inst, ExtraModels: []string{"e"}, ModelStates: map[string]string{"a": "loading"}}
	tests := []struct {
		name        string
		rows        []storage.InstanceModel
		live        *ActiveInstance
		wantModels  string
		wantStates  map[string]string
		wantDetails map[string]string
	}{
		{"só banco", rows, nil, "p,e", map[string]string{"p": "ready", "e": "ready", "a": "failed"}, map[string]string{"a": "timeout"}},
		{"memória sobrepõe", rows, live, "p,e", map[string]string{"p": "ready", "e": "ready", "a": "loading"}, nil},
		{"sem nada", nil, nil, "", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			models, states, details := mergeModelStatus(inst, tc.rows, tc.live)
			if strings.Join(models, ",") != tc.wantModels {
				t.Fatalf("models = %v", models)
			}
			if fmt.Sprint(states) != fmt.Sprint(tc.wantStates) || fmt.Sprint(details) != fmt.Sprint(tc.wantDetails) {
				t.Fatalf("states = %v details = %v", states, details)
			}
		})
	}
}

// Rodar com -race: mutações de modelos concorrendo com leitores e o banco.
func TestModelStateConcurrency(t *testing.T) {
	m, db, _ := newTestManager(t)
	act, _ := seedRunning(t, m, db, "c1", nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mdl := fmt.Sprintf("m%d", i%3)
			for j := 0; j < 20; j++ {
				m.setModelState(act, mdl, "loading", "")
				m.setModelProgress(act, mdl, "50%")
				switch j % 4 {
				case 0:
					m.finishAdd(act, mdl)
				case 1:
					_ = m.RemoveModel("c1", mdl)
				case 2:
					_ = m.AddModel("c1", mdl) // sem SSH: erro, mas passa pelo lock
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
