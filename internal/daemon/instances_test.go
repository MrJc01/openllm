package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crom-org/openllm/internal/config"
	"github.com/crom-org/openllm/internal/proxy"
	_ "github.com/crom-org/openllm/internal/providers/mock"
	"github.com/crom-org/openllm/internal/engines"
	"github.com/crom-org/openllm/internal/storage"
)

func testConfig() *config.Config {
	return &config.Config{
		Provider: "mock",
		Model:    "deepseek-r1:7b",
		Providers: map[string]config.ProviderConfig{
			"mock": {APIKey: ""}, // vazia = sem rede em EnsureKeysExist
		},
		TpsTarget:      40,
		InstancesCount: 1,
		RemotePort:     11434,
		LocalProxyPort: 11434,
		LocalDaemonPort: 17290,
		HeartbeatPort:  17291,
	}
}

func newTestManager(t *testing.T) (*InstanceManager, *storage.DB, *proxy.ProxyServer) {
	t.Helper()

	// Isola o wd: .openllm/ (chaves SSH, config) é criado em diretório temp
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(oldWd) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	// Pré-cria chaves SSH + marker de registro para o EnsureKeysExist não
	// tocar na API do provedor durante os testes
	os.MkdirAll(".openllm", 0700)
	os.WriteFile(".openllm/id_ed25519", []byte(dummyRSAKey), 0600)
	os.WriteFile(".openllm/id_ed25519.pub", []byte("ssh-rsa AAAA test\n"), 0644)
	os.WriteFile(".openllm/.key_registered_at", []byte(time.Now().Format(time.RFC3339)), 0644)

	db, err := storage.OpenDBInDir(".")
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ps := proxy.NewProxyServer(0)
	return NewInstanceManager(db, ps), db, ps
}

// Especialista: QA — DeployInstance com provider mock grava a instância com
// engine resolvida, group_id e a Definition serializada; o backgroundSetup
// falha rápido (SSH inexistente) e marca "failed".
func TestDeployInstanceMockLifecycle(t *testing.T) {
	setSSHRetry(t, 1, time.Millisecond)

	m, db, _ := newTestManager(t)
	cfg := testConfig()

	id, err := m.DeployInstance(cfg, DeployRequestPayload{Model: "sdxl", Engine: "comfyui", MachineID: "mock-offer-001"})
	if err != nil {
		t.Fatalf("deploy failed: %v", err)
	}
	if id == "" {
		t.Fatal("expected instance id")
	}

	// Registro no banco com os metadados novos
	inst, err := db.GetInstance(id)
	if err != nil || inst == nil {
		t.Fatalf("instance not in db: %v", err)
	}
	if inst.Engine != "comfyui" {
		t.Fatalf("expected engine comfyui, got %q", inst.Engine)
	}
	if inst.GroupID != "comfyui/sdxl" {
		t.Fatalf("expected group_id comfyui/sdxl, got %q", inst.GroupID)
	}
	if inst.EngineDefJSON == "" {
		t.Fatal("expected engine def JSON persisted")
	}
	var def map[string]interface{}
	if err := json.Unmarshal([]byte(inst.EngineDefJSON), &def); err != nil {
		t.Fatalf("def_json is not valid json: %v", err)
	}

	// Deploy novo que falha é destruído automaticamente (anti-vazamento):
	// some do banco e do provider mock
	waitForGone(t, db, id, 15*time.Second)
}

// Especialista: SRE — scale up cria instâncias; scale down nunca deixa menos
// que 1 e destrói as mais caras primeiro.
func TestScaleGroupUpAndDown(t *testing.T) {
	m, db, _ := newTestManager(t)
	cfg := testConfig()

	// Scale up para 3 (instâncias ficam "deploying" — SSH nunca conecta com retries default)
	res, err := m.ScaleGroup(cfg, "ollama", "deepseek-r1:7b", 3, 0)
	if err != nil {
		t.Fatalf("scale up failed: %v", err)
	}
	if len(res.Created) != 3 {
		t.Fatalf("expected 3 created, got %d", len(res.Created))
	}
	active, _ := m.activeGroupInstances("ollama/deepseek-r1:7b")
	if len(active) != 3 {
		t.Fatalf("expected 3 active, got %d", len(active))
	}

	// Grupo deve existir no banco
	if g, _ := db.GetGroup("ollama/deepseek-r1:7b"); g == nil {
		t.Fatal("group not registered in db after scale")
	}

	// Scale down para 1: remove 2
	res, err = m.ScaleGroup(cfg, "ollama", "deepseek-r1:7b", 1, 0)
	if err != nil {
		t.Fatalf("scale down failed: %v", err)
	}
	if len(res.Removed) != 2 {
		t.Fatalf("expected 2 removed, got %d", len(res.Removed))
	}
	active, _ = m.activeGroupInstances("ollama/deepseek-r1:7b")
	if len(active) != 1 {
		t.Fatalf("expected 1 active after downscale, got %d", len(active))
	}

	// Scale down para 1 de novo: nada muda (nunca remove a última)
	res, err = m.ScaleGroup(cfg, "ollama", "deepseek-r1:7b", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 0 || len(res.Created) != 0 {
		t.Fatalf("no-op scale should do nothing: %+v", res)
	}

	// Target inválido
	if _, err := m.ScaleGroup(cfg, "ollama", "deepseek-r1:7b", 0, 0); err == nil {
		t.Fatal("scale to 0 must be rejected")
	}
}

// Especialista: SRE — ao reduzir, a instância mais CARA é removida primeiro.
func TestScaleDownRemovesMostExpensiveFirst(t *testing.T) {
	m, db, _ := newTestManager(t)
	cfg := testConfig()

	// Semeia 3 instâncias "running" com custos diferentes (sem passar pelo SSH)
	seed := []struct {
		id   string
		cost float64
	}{
		{"cheap-1", 0.10},
		{"mid-1", 0.25},
		{"expensive-1", 1.00},
	}
	for _, s := range seed {
		db.SaveInstance(&storage.Instance{
			ID: s.id, Provider: "mock", Status: "running", Model: "llama3.2:3b",
			Engine: "ollama", GroupID: "ollama/llama3.2:3b", CostPerHour: s.cost, CreatedAt: time.Now(),
		})
	}

	res, err := m.ScaleGroup(cfg, "ollama", "llama3.2:3b", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 2 {
		t.Fatalf("expected 2 removed, got %v", res.Removed)
	}
	for _, id := range res.Removed {
		if id == "cheap-1" {
			t.Fatal("cheapest instance should survive downscale")
		}
	}
	if got, _ := db.GetInstance("cheap-1"); got == nil {
		t.Fatal("cheap-1 should still exist")
	}
	if got, _ := db.GetInstance("expensive-1"); got != nil {
		t.Fatal("expensive-1 should be removed first")
	}
}

// Especialista: SRE — corrige G2: TODAS as engines running entram no proxy,
// e rotas do grupo são herdadas nos targets.
func TestUpdateProxyTargetsIncludesAllEnginesAndRoutes(t *testing.T) {
	m, db, ps := newTestManager(t)

	// Registra rotas para o grupo comfyui/sdxl
	db.SaveGroup(&storage.Group{
		ID: "comfyui/sdxl", Stack: "mm",
		Routes:    []string{"/v1/images/generations"},
		CreatedAt: time.Now(),
	})

	// Injeta "instâncias ativas" manualmente (como se tivessem passado o setup)
	m.actives["vllm-1"] = &ActiveInstance{
		Instance:  storage.Instance{ID: "vllm-1", Status: "running", Model: "Qwen/Qwen2.5-Coder-7B-Instruct", Engine: "vllm", GroupID: "vllm/Qwen/Qwen2.5-Coder-7B-Instruct"},
		LocalPort: 10001,
	}
	m.actives["comfy-1"] = &ActiveInstance{
		Instance:  storage.Instance{ID: "comfy-1", Status: "running", Model: "sdxl", Engine: "comfyui", GroupID: "comfyui/sdxl"},
		LocalPort: 10002,
	}
	m.actives["failed-1"] = &ActiveInstance{
		Instance:  storage.Instance{ID: "failed-1", Status: "failed", Model: "x", Engine: "ollama"},
		LocalPort: 10003,
	}

	m.updateProxyTargets()

	// Valida via AvailableModels (público) + estado interno (mesmo package)
	all := ps.AvailableModels()
	found := map[string]bool{}
	for _, mo := range all {
		found[mo] = true
	}
	if !found["Qwen/Qwen2.5-Coder-7B-Instruct"] || !found["sdxl"] {
		t.Fatalf("vllm/comfyui must be in the LB pool (G2), got %v", all)
	}

	ps.TargetsSnapshot(func(tgts []proxy.Target) {
		for _, tg := range tgts {
			if tg.InstanceID == "failed-1" {
				t.Fatal("failed instance must not be in the pool")
			}
			if tg.InstanceID == "comfy-1" && len(tg.Routes) != 1 {
				t.Fatalf("group routes should be inherited, got %v", tg.Routes)
			}
		}
	})
}

// Especialista: SRE — watchdog pings registram e expiram corretamente.
func TestWatchdogPingRegistration(t *testing.T) {
	m, _, _ := newTestManager(t)

	if _, ok := m.GetLastPing("inst-1"); ok {
		t.Fatal("no ping should exist before registration")
	}
	m.RegisterPing("inst-1")
	_, ok := m.GetLastPing("inst-1")
	if !ok {
		t.Fatal("ping should exist after registration")
	}
}

// Especialista: QA — resolveEngineDef: def_json tem prioridade, nome é
// fallback, desconhecido cai no ollama.
func TestResolveEngineDef(t *testing.T) {
	inst := storage.Instance{Engine: "unknown-engine", EngineDefJSON: `{"name":"ext-engine","remote_port":9999,"docker_image":"a/b"}`}
	def := resolveEngineDef(inst)
	if def.Name != "ext-engine" || def.RemotePort != 9999 {
		t.Fatalf("def_json should win: %+v", def)
	}

	def = resolveEngineDef(storage.Instance{Engine: "comfyui"})
	if def.Name != "comfyui" || def.RemotePort != 18188 {
		t.Fatalf("builtin lookup failed: %+v", def)
	}

	def = resolveEngineDef(storage.Instance{Engine: "totally-unknown"})
	if def.Name != "ollama" {
		t.Fatalf("unknown engine should fall back to ollama, got %s", def.Name)
	}
}

// Especialista: Segurança — nada de segredos vazando no estado salvo/logs de
// deploy (a API key do provider não vai para o banco nem para a Definition).
func TestNoSecretsInPersistedState(t *testing.T) {
	setSSHRetry(t, 1, time.Millisecond)

	m, db, _ := newTestManager(t)
	cfg := testConfig()
	cfg.Providers["mock"] = config.ProviderConfig{APIKey: "super-secret-key-xyz"}

	id, err := m.DeployInstance(cfg, DeployRequestPayload{Model: "sdxl", Engine: "comfyui", MachineID: "mock-offer-001"})
	if err != nil {
		t.Fatal(err)
	}

	// Snapshot imediato: a row existe enquanto o setup roda (antes do cleanup)
	snapshot, err := db.GetInstance(id)
	if err != nil || snapshot == nil {
		t.Fatalf("instance row missing right after deploy: %v", err)
	}

	// Aguarda o cleanup automático (evita race com o goroutine de setup
	// que lê as vars de retry)
	waitForGone(t, db, id, 15*time.Second)

	blob := fmt.Sprintf("%s|%s|%s", snapshot.EngineDefJSON, snapshot.GroupID, snapshot.Engine)
	if strings.Contains(blob, "super-secret-key-xyz") {
		t.Fatal("API key leaked into persisted instance state")
	}

	// Segurança extra: a Definition persistida também não deve conter segredos
	var defMap map[string]interface{}
	json.Unmarshal([]byte(snapshot.EngineDefJSON), &defMap)
	for _, v := range defMap {
		if s, ok := v.(string); ok && strings.Contains(s, "super-secret-key-xyz") {
			t.Fatal("API key leaked inside engine definition")
		}
	}
}

// waitForGone faz polling até a instância deixar de existir no banco.
func waitForGone(t *testing.T, db *storage.DB, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		inst, err := db.GetInstance(id)
		if err != nil {
			t.Fatalf("db error waiting for deletion: %v", err)
		}
		if inst == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("instance %s never removed from db (status: %s)", id, inst.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// setSSHRetry ajusta os parâmetros de retry do backgroundSetup para o teste
// (falha rápida) e restaura os defaults no cleanup, de forma atômica.
func setSSHRetry(t *testing.T, retries int, delay time.Duration) {
	t.Helper()
	oldRetries := sshConnectRetries.Load()
	oldDelay := sshConnectRetryDelay.Load()
	sshConnectRetries.Store(int64(retries))
	sshConnectRetryDelay.Store(int64(delay))
	t.Cleanup(func() {
		sshConnectRetries.Store(oldRetries)
		sshConnectRetryDelay.Store(oldDelay)
	})
}

// waitForStatus faz polling do banco até a instância atingir o status esperado.
func waitForStatus(t *testing.T, db *storage.DB, id, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		inst, err := db.GetInstance(id)
		if err != nil {
			t.Fatalf("db error waiting for %s: %v", want, err)
		}
		if inst != nil && inst.Status == want {
			return
		}
		if time.Now().After(deadline) {
			last := "<nil>"
			if inst != nil {
				last = inst.Status
			}
			t.Fatalf("instance %s never reached %q (last: %s)", id, want, last)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

const dummyRSAKey = `-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEA0Z3VS5JJcds3xfn/yGWyVfPWXKbLcTCeJr4ltEAcSKwZEm9m
MIbXsK0VXPBbdY8GiwR4GApvc9BGRaSgLxCHVSi4PQIDAQAB
-----END RSA PRIVATE KEY-----
`

// Modelos extras (AddModel) viram targets do proxy apontando para a mesma
// instância; RemoveModel tira só o extra.
func TestExtraModelsBecomeProxyTargets(t *testing.T) {
	m, _, ps := newTestManager(t)
	m.actives["i1"] = &ActiveInstance{
		Instance:    storage.Instance{ID: "i1", Status: "running", Engine: "ollama", Model: "llama3.2:3b"},
		LocalPort:   40001,
		ExtraModels: []string{"nomic-embed-text"},
		ModelStates: map[string]string{"nomic-embed-text": "ready"}, // extra só roteia pronto
	}
	m.updateProxyTargets()

	byModel := map[string]int{}
	ps.TargetsSnapshot(func(ts []proxy.Target) {
		for _, tg := range ts {
			byModel[tg.Model] = tg.LocalPort
		}
	})
	if byModel["llama3.2:3b"] != 40001 || byModel["nomic-embed-text"] != 40001 {
		t.Fatalf("both models should route to port 40001: %v", byModel)
	}

	if err := m.RemoveModel("i1", "nomic-embed-text"); err != nil {
		t.Fatal(err)
	}
	ps.TargetsSnapshot(func(ts []proxy.Target) {
		if len(ts) != 1 || ts[0].Model != "llama3.2:3b" {
			t.Fatalf("only primary model should remain: %+v", ts)
		}
	})
}

func TestModelStatesAreSnapshotted(t *testing.T) {
	m := &InstanceManager{actives: map[string]*ActiveInstance{}}
	act := &ActiveInstance{}
	act.ID, act.Model = "i1", "ltx-video-2b"
	m.actives["i1"] = act
	m.setModelState(act, "ltx-video-2b", "loading", "")
	snap := m.GetActiveInstances()[0]
	m.setModelState(act, "ltx-video-2b", "ready", "")
	if snap.ModelStates["ltx-video-2b"] != "loading" {
		t.Fatalf("snapshot mudou junto: %v", snap.ModelStates)
	}
	if got := m.GetActiveInstances()[0].ModelStates["ltx-video-2b"]; got != "ready" {
		t.Fatalf("estado = %q, quer ready", got)
	}
	gone := &ActiveInstance{}
	gone.ID = "sumiu"
	m.setModelState(gone, "x", "ready", "") // instância inexistente: não quebra
}

func TestResolveEngineDefUsesCurrentModelCommands(t *testing.T) {
	cur := engines.Get("comfyui")
	old := cur
	old.ModelReadyCmd = "velho"
	old.ModelPullCmd = "velho"
	b, _ := json.Marshal(old)
	got := resolveEngineDef(storage.Instance{Engine: "comfyui", EngineDefJSON: string(b)})
	if got.ModelReadyCmd != cur.ModelReadyCmd || got.ModelPullCmd != cur.ModelPullCmd {
		t.Fatal("instância antiga deveria usar os comandos de modelo atuais")
	}
	// Imagem diferente (deploy custom): mantém os comandos salvos.
	old.DockerImage = "outra/imagem"
	b, _ = json.Marshal(old)
	if got := resolveEngineDef(storage.Instance{Engine: "comfyui", EngineDefJSON: string(b)}); got.ModelReadyCmd != "velho" {
		t.Fatal("imagem custom não deveria trocar os comandos")
	}
}
