package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/crom-org/openllm/internal/config"
	"github.com/crom-org/openllm/internal/engines"
	"github.com/crom-org/openllm/internal/storage"
)

// Fluxo de modelos por instância (principal, extras, add/swap).
//
// Regras:
//   - memória (ActiveInstance) é a visão viva; instance_models é a fonte de
//     verdade entre restarts. Escritas no banco acontecem FORA do activesMu,
//     com version monotônica (escrita atrasada não sobrescreve uma nova).
//   - toda mutação confere a encarnação (m.actives[id] == act): goroutines de
//     uma instância parada/reconectada não mexem na nova.
//   - linhas de modelos não-principais só nascem por ação explícita
//     (add/swap); setModelState só atualiza — remove não é desfeito.

const (
	defaultModelWait      = 20 * time.Minute
	defaultModelPoll      = 3 * time.Second
	defaultReconcileEvery = 2 * time.Minute
	failedModelTTL        = 24 * time.Hour
)

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// cmdRunner é o que o fluxo de modelos precisa do SSH (fakes nos testes).
type cmdRunner interface {
	RunCommand(cmd string) (string, error)
}

func (a *ActiveInstance) context() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

// nextVersion: relógio monotônico, sempre acima dos nanossegundos atuais
// (versões gravadas por execuções anteriores do daemon ficam para trás).
func (m *InstanceManager) nextVersion() int64 {
	for {
		cur := m.modelVersion.Load()
		next := cur + 1
		if now := time.Now().UnixNano(); now > next {
			next = now
		}
		if m.modelVersion.CompareAndSwap(cur, next) {
			return next
		}
	}
}

// currentLocked: act ainda é a encarnação ativa (chamar com activesMu).
func (m *InstanceManager) currentLocked(act *ActiveInstance) bool {
	return m.actives[act.ID] == act
}

func (m *InstanceManager) isCurrent(act *ActiveInstance) bool {
	m.activesMu.RLock()
	defer m.activesMu.RUnlock()
	return m.currentLocked(act)
}

// persist executa uma escrita no banco com 1 nova tentativa; falha final vai
// para o log da instância. ErrModelGone não é erro de persistência.
func (m *InstanceManager) persist(instID, what string, fn func() error) error {
	if m.db == nil {
		return nil
	}
	err := fn()
	if err != nil && !errors.Is(err, storage.ErrModelGone) {
		time.Sleep(50 * time.Millisecond)
		err = fn()
	}
	if err != nil && !errors.Is(err, storage.ErrModelGone) {
		log.Printf("[%s] Warning: persist %s: %v", instID, what, err)
		m.AddLog(instID, fmt.Sprintf("WARNING: could not persist %s: %v", what, err))
	}
	return err
}

func ensureMaps(a *ActiveInstance) {
	if a.ModelStates == nil {
		a.ModelStates = map[string]string{}
	}
	if a.ModelDetails == nil {
		a.ModelDetails = map[string]string{}
	}
	if a.ModelProgress == nil {
		a.ModelProgress = map[string]string{}
	}
}

// setModelState registra o estado (e motivo de falha) de um modelo. Ignora
// encarnações antigas e modelos não rastreados (ex.: removidos no meio).
func (m *InstanceManager) setModelState(act *ActiveInstance, model, state, detail string) {
	m.activesMu.Lock()
	if !m.currentLocked(act) {
		m.activesMu.Unlock()
		return
	}
	primary := model == act.Model
	if _, tracked := act.ModelStates[model]; !primary && !tracked {
		m.activesMu.Unlock()
		return
	}
	ensureMaps(act)
	act.ModelStates[model] = state
	if detail != "" {
		act.ModelDetails[model] = detail
	} else {
		delete(act.ModelDetails, model)
	}
	ver := m.nextVersion()
	id := act.ID
	m.activesMu.Unlock()

	m.persist(id, "state of "+model, func() error {
		ok, err := m.db.UpdateInstanceModelState(id, model, state, detail, ver)
		if err == nil && !ok && primary {
			// principal sem linha (instância anterior à tabela): cria sem sobrescrever
			return m.db.EnsureInstanceModel(id, model, storage.RolePrimary, state, ver)
		}
		return err
	})
}

// setModelProgress guarda a última leitura de progresso do download.
func (m *InstanceManager) setModelProgress(act *ActiveInstance, model, msg string) {
	m.activesMu.Lock()
	defer m.activesMu.Unlock()
	if !m.currentLocked(act) {
		return
	}
	if _, tracked := act.ModelStates[model]; !tracked && model != act.Model {
		return
	}
	ensureMaps(act)
	act.ModelProgress[model] = msg
}

// restoreModels recarrega do banco extras e últimos estados de uma encarnação
// recém-criada. Extras voltam "loading" (fora do proxy) até a re-verificação.
// Em erro de leitura o principal fica "unknown" e o erro é devolvido.
func (m *InstanceManager) restoreModels(act *ActiveInstance) ([]storage.InstanceModel, error) {
	if m.db == nil {
		return nil, nil
	}
	m.activesMu.RLock()
	id, primary := act.ID, act.Model
	m.activesMu.RUnlock()

	rows, err := m.db.ListInstanceModels(id)
	if err != nil {
		m.activesMu.Lock()
		if m.currentLocked(act) {
			ensureMaps(act)
			act.ModelStates[primary] = "unknown"
		}
		m.activesMu.Unlock()
		log.Printf("[%s] Warning: could not load persisted models: %v", id, err)
		return nil, err
	}
	hasPrimary := false
	for _, r := range rows {
		hasPrimary = hasPrimary || r.Model == primary
	}
	if !hasPrimary && primary != "" {
		m.persist(id, "primary model", func() error {
			return m.db.EnsureInstanceModel(id, primary, storage.RolePrimary, "", m.nextVersion())
		})
	}

	m.activesMu.Lock()
	if !m.currentLocked(act) {
		m.activesMu.Unlock()
		return nil, nil
	}
	ensureMaps(act)
	act.ExtraModels = nil
	for _, r := range rows {
		if r.Model == act.Model {
			if _, live := act.ModelStates[r.Model]; !live && r.State != "" {
				act.ModelStates[r.Model] = r.State
			}
			continue
		}
		if r.Role != storage.RoleExtra && r.State == "failed" {
			act.ModelStates[r.Model] = "failed" // add/swap que falhou: terminal
			act.ModelDetails[r.Model] = r.Detail
			continue
		}
		act.ModelStates[r.Model] = "loading" // re-verificação pendente
		if r.Role == storage.RoleExtra {
			act.ExtraModels = append(act.ExtraModels, r.Model)
		}
	}
	m.activesMu.Unlock()
	if len(rows) > 0 {
		m.AddLog(id, fmt.Sprintf("Restored %d persisted model(s) for this instance", len(rows)))
	}
	return rows, nil
}

// resumeModels re-verifica os modelos além do principal: extras entram no
// proxy só quando o comando de prontidão passar (o pull, idempotente, só é
// relançado se faltar); add/swap interrompidos são retomados.
func (m *InstanceManager) resumeModels(ctx context.Context, act *ActiveInstance, runner cmdRunner, def engines.Definition, rows []storage.InstanceModel) *sync.WaitGroup {
	var wg sync.WaitGroup
	for _, r := range rows {
		if r.Role == storage.RolePrimary || r.Model == act.Instance.Model {
			continue
		}
		if r.Role != storage.RoleExtra && r.State == "failed" {
			continue
		}
		wg.Add(1)
		go func(r storage.InstanceModel) {
			defer wg.Done()
			if err := m.ensureModel(ctx, act, runner, def, r.Model); err != nil {
				if ctx.Err() == nil {
					m.AddLog(act.ID, fmt.Sprintf("Recovery warning (%s): %v", r.Model, err))
					m.updateProxyTargets()
				}
				return
			}
			switch r.Role {
			case storage.RoleAdding:
				m.finishAdd(act, r.Model)
			case storage.RoleSwapping:
				m.finishSwap(act, r.Model)
			default:
				m.updateProxyTargets()
				m.AddLog(act.ID, fmt.Sprintf("Model %s verified on host — routing again", r.Model))
			}
		}(r)
	}
	return &wg
}

// ensureModel confirma que o modelo está no host; só relança o pull se o
// comando de prontidão falhar.
func (m *InstanceManager) ensureModel(ctx context.Context, act *ActiveInstance, runner cmdRunner, def engines.Definition, model string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pull, ready := modelCmds(def, model)
	if strings.TrimSpace(ready) == "" {
		m.setModelState(act, model, "ready", "")
		return nil
	}
	if _, err := runner.RunCommand(ready); err == nil {
		m.setModelState(act, model, "ready", "")
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(pull) != "" {
		if _, err := runner.RunCommand(backgroundCmd(model, pull)); err != nil {
			reason := fmt.Sprintf("could not relaunch pull: %v", err)
			m.setModelState(act, model, "failed", reason)
			return errors.New(reason)
		}
	}
	return m.waitForModelReady(ctx, act, runner, def, model)
}

// reconcileLoop re-checa periodicamente os extras: um extra que sumiu do host
// sai do proxy ("failed"); um que voltou (ou foi re-baixado) volta a rotear.
func (m *InstanceManager) reconcileLoop(ctx context.Context, act *ActiveInstance, runner cmdRunner, def engines.Definition) {
	t := time.NewTicker(orDefault(m.reconcileEvery, defaultReconcileEvery))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !m.isCurrent(act) {
				return
			}
			m.reconcileOnce(ctx, act, runner, def)
		}
	}
}

func (m *InstanceManager) reconcileOnce(ctx context.Context, act *ActiveInstance, runner cmdRunner, def engines.Definition) {
	m.activesMu.RLock()
	states := map[string]string{}
	for _, x := range act.ExtraModels {
		if st := act.ModelStates[x]; st == "ready" || st == "failed" {
			states[x] = st
		}
	}
	m.activesMu.RUnlock()

	changed := false
	for x, st := range states {
		_, ready := modelCmds(def, x)
		if strings.TrimSpace(ready) == "" || ctx.Err() != nil {
			continue
		}
		_, err := runner.RunCommand(ready)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil && st == "ready":
			m.setModelState(act, x, "failed", fmt.Sprintf("ready check failed during reconcile: %v", err))
			m.AddLog(act.ID, fmt.Sprintf("Model %s no longer available on host — removed from routing", x))
			changed = true
		case err == nil && st == "failed":
			m.setModelState(act, x, "ready", "")
			changed = true
		}
	}
	if changed {
		m.updateProxyTargets()
	}
}

// trackTarget marca um alvo de add/swap como "loading" em memória e grava a
// linha com o papel explícito. Devolve erro se a gravação falhar.
func (m *InstanceManager) trackTarget(act *ActiveInstance, model, role string) error {
	ver := m.nextVersion()
	err := m.persist(act.ID, model+" ("+role+")", func() error {
		return m.db.PutInstanceModel(act.ID, model, role, "loading", "", ver)
	})
	if err != nil {
		m.activesMu.Lock()
		if m.currentLocked(act) {
			delete(act.ModelStates, model)
		}
		m.activesMu.Unlock()
	}
	return err
}

// ============================================================
// Swap de modelo em instância rodando (otimização: não re-renta a máquina)
// ============================================================

// SwapModel troca o modelo de uma instância running sem destruí-la: dispara
// o pull do novo modelo no host remoto (fire-and-forget) e, quando pronto,
// atualiza o grupo/load balancer. O modelo antigo continua servindo durante
// o download.
func (m *InstanceManager) SwapModel(cfg *config.Config, instanceID, newModel string) error {
	act, inst, runner, def, err := m.claimTarget(instanceID, newModel, true)
	if err != nil {
		return err
	}
	m.AddLog(instanceID, fmt.Sprintf("Model swap requested: %s -> %s", inst.Model, newModel))
	// Alvo persistido antes do pull: um restart no meio retoma o swap.
	if err := m.trackTarget(act, newModel, storage.RoleSwapping); err != nil {
		return fmt.Errorf("could not persist swap target: %w", err)
	}
	ctx := act.context()
	go func() {
		full, _ := modelCmds(def, newModel)
		if _, err := runner.RunCommand(backgroundCmd(newModel, full)); err != nil {
			m.setModelState(act, newModel, "failed", fmt.Sprintf("could not launch pull: %v", err))
			m.AddLog(instanceID, fmt.Sprintf("Swap failed: could not launch pull: %v", err))
			return
		}
		if err := m.waitForModelReady(ctx, act, runner, def, newModel); err != nil {
			if ctx.Err() == nil {
				m.AddLog(instanceID, fmt.Sprintf("Swap warning: %v (check %s)", err, pullLog(newModel)))
			}
			return
		}
		m.finishSwap(act, newModel)
	}()
	return nil
}

// claimTarget valida um add/swap e, no mesmo lock, marca o alvo "loading"
// (dois pedidos simultâneos do mesmo modelo não passam ambos).
func (m *InstanceManager) claimTarget(instanceID, model string, swap bool) (*ActiveInstance, storage.Instance, cmdRunner, engines.Definition, error) {
	m.activesMu.Lock()
	defer m.activesMu.Unlock()
	act, ok := m.actives[instanceID]
	var def engines.Definition
	if !ok {
		if swap {
			return nil, storage.Instance{}, nil, def, fmt.Errorf("instance %s is not active (only running instances can swap)", instanceID)
		}
		return nil, storage.Instance{}, nil, def, fmt.Errorf("instance %s is not active", instanceID)
	}
	inst := act.Instance
	def = resolveEngineDef(inst)
	var runner cmdRunner
	if act.SSHClient != nil { // evita interface não-nil com ponteiro nil
		runner = act.SSHClient
	}
	already := act.ModelStates[model] == "loading"
	if !swap {
		for _, mdl := range act.Models() {
			already = already || mdl == model
		}
	}
	switch {
	case inst.Status != "running":
		return nil, inst, nil, def, fmt.Errorf("instance %s is not running (status: %s)", instanceID, inst.Status)
	case strings.TrimSpace(def.ModelPullCmd) == "" && swap:
		return nil, inst, nil, def, fmt.Errorf("engine does not support model swap (no model_pull_cmd)")
	case strings.TrimSpace(def.ModelPullCmd) == "":
		return nil, inst, nil, def, fmt.Errorf("engine %s does not support multiple models (no model_pull_cmd)", def.Name)
	case swap && model == inst.Model:
		return nil, inst, nil, def, fmt.Errorf("model %s is already the primary model of %s", model, instanceID)
	case already:
		return nil, inst, nil, def, fmt.Errorf("model %s is already served by %s", model, instanceID)
	case runner == nil:
		return nil, inst, nil, def, fmt.Errorf("instance %s has no SSH connection", instanceID)
	}
	ensureMaps(act)
	act.ModelStates[model] = "loading"
	delete(act.ModelDetails, model)
	return act, inst, runner, def, nil
}

// finishSwap promove newModel a principal: banco numa transação (aborta se o
// alvo/instância sumiu), depois a memória, conferindo a encarnação.
func (m *InstanceManager) finishSwap(act *ActiveInstance, newModel string) {
	m.activesMu.RLock()
	_, tracked := act.ModelStates[newModel]
	ok := m.currentLocked(act) && tracked
	id, old, engine := act.ID, act.Model, act.Engine
	m.activesMu.RUnlock()
	if !ok {
		return
	}
	ver := m.nextVersion()
	err := m.persist(id, "swap to "+newModel, func() error {
		return m.db.CommitSwap(id, old, newModel, GroupID(engine, newModel), ver)
	})
	if errors.Is(err, storage.ErrModelGone) {
		m.AddLog(id, fmt.Sprintf("Swap to %s aborted: target or instance removed meanwhile", newModel))
		return
	}

	m.activesMu.Lock()
	_, tracked = act.ModelStates[newModel]
	if !m.currentLocked(act) || !tracked || act.Model != old {
		m.activesMu.Unlock()
		return
	}
	ensureMaps(act)
	act.ExtraModels = removeString(act.ExtraModels, newModel)
	act.Model = newModel
	act.GroupID = GroupID(act.Engine, newModel)
	delete(act.ModelStates, old)
	delete(act.ModelProgress, old)
	delete(act.ModelDetails, old)
	act.ModelStates[newModel] = "ready"
	delete(act.ModelDetails, newModel)
	m.activesMu.Unlock()

	m.updateProxyTargets()
	m.AddLog(id, fmt.Sprintf("Model swapped to %s — ready!", newModel))
}

// AddModel baixa e aquece um modelo adicional numa instância em execução,
// sem trocar o principal. Só engines com model_pull_cmd (ex: ollama).
func (m *InstanceManager) AddModel(instanceID, newModel string) error {
	act, inst, runner, def, err := m.claimTarget(instanceID, newModel, false)
	if err != nil {
		return err
	}
	m.AddLog(instanceID, fmt.Sprintf("Add model requested: %s (keeps %s)", newModel, inst.Model))
	if err := m.trackTarget(act, newModel, storage.RoleAdding); err != nil {
		return fmt.Errorf("could not persist new model: %w", err)
	}
	ctx := act.context()
	go func() {
		full, _ := modelCmds(def, newModel)
		if _, err := runner.RunCommand(backgroundCmd(newModel, full)); err != nil {
			m.setModelState(act, newModel, "failed", fmt.Sprintf("could not launch pull: %v", err))
			m.AddLog(instanceID, fmt.Sprintf("Add model failed: could not launch pull: %v", err))
			return
		}
		if err := m.waitForModelReady(ctx, act, runner, def, newModel); err != nil {
			if ctx.Err() == nil {
				m.AddLog(instanceID, fmt.Sprintf("Add model warning: %v", err))
			}
			return
		}
		m.finishAdd(act, newModel)
	}()
	return nil
}

// finishAdd passa a rotear o modelo. Aborta se foi removido durante o
// download (a linha sumiu) ou se a encarnação mudou.
func (m *InstanceManager) finishAdd(act *ActiveInstance, newModel string) {
	m.activesMu.RLock()
	_, tracked := act.ModelStates[newModel]
	ok := m.currentLocked(act) && tracked && newModel != act.Model
	id := act.ID
	m.activesMu.RUnlock()
	if !ok {
		return
	}
	ver := m.nextVersion()
	err := m.persist(id, "extra model "+newModel, func() error {
		return m.db.PromoteExtraModel(id, newModel, ver)
	})
	if errors.Is(err, storage.ErrModelGone) {
		m.AddLog(id, fmt.Sprintf("Model %s was removed while downloading — not routed", newModel))
		return
	}

	m.activesMu.Lock()
	_, tracked = act.ModelStates[newModel]
	if !m.currentLocked(act) || !tracked {
		m.activesMu.Unlock()
		return
	}
	ensureMaps(act)
	act.ExtraModels = append(removeString(act.ExtraModels, newModel), newModel)
	act.ModelStates[newModel] = "ready"
	delete(act.ModelDetails, newModel)
	models := act.Models()
	m.activesMu.Unlock()

	m.updateProxyTargets()
	m.AddLog(id, fmt.Sprintf("Model %s added — now serving %v", newModel, models))
}

// RemoveModel para de rotear um modelo extra (ou cancela um add/swap em
// curso) e o apaga do conjunto persistido. O principal só sai via swap/stop.
func (m *InstanceManager) RemoveModel(instanceID, model string) error {
	m.activesMu.Lock()
	act, ok := m.actives[instanceID]
	if !ok {
		m.activesMu.Unlock()
		return fmt.Errorf("instance %s is not active", instanceID)
	}
	if act.Model == model {
		m.activesMu.Unlock()
		return fmt.Errorf("model %s is the primary model of %s (use swap or stop)", model, instanceID)
	}
	act.ExtraModels = removeString(act.ExtraModels, model)
	delete(act.ModelStates, model)
	delete(act.ModelProgress, model)
	delete(act.ModelDetails, model)
	m.activesMu.Unlock()

	m.persist(instanceID, "removal of "+model, func() error {
		return m.db.DeleteInstanceModel(instanceID, model)
	})
	m.updateProxyTargets()
	m.AddLog(instanceID, fmt.Sprintf("Model %s removed from routing", model))
	return nil
}
