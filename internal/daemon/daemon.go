package daemon

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crom-org/openllm/internal/config"
	"github.com/crom-org/openllm/internal/providers"
	"github.com/crom-org/openllm/internal/proxy"
	"github.com/crom-org/openllm/internal/ssh"
	"github.com/crom-org/openllm/internal/storage"
)

type Daemon struct {
	cfg         *config.Config
	db          *storage.DB
	manager     *InstanceManager
	proxyServer *proxy.ProxyServer
	controlAPI  *ControlServer
	heartbeat   *HeartbeatServer
	autoscaler  *Autoscaler
}

func NewDaemon(cfg *config.Config) (*Daemon, error) {
	db, err := storage.OpenDB()
	if err != nil {
		return nil, err
	}

	ps := proxy.NewProxyServer(cfg.LocalProxyPort)
	mgr := NewInstanceManager(db, ps)
	ctrl := NewControlServer(cfg.LocalDaemonPort, mgr, db)
	hb := NewHeartbeatServer(cfg.HeartbeatPort, mgr)
	as := NewAutoscaler(cfg, db, mgr, ps)

	return &Daemon{
		cfg:         cfg,
		db:          db,
		manager:     mgr,
		proxyServer: ps,
		controlAPI:  ctrl,
		heartbeat:   hb,
		autoscaler:  as,
	}, nil
}

func (d *Daemon) Start(ctx context.Context) error {
	// Estratégia de balanceamento do proxy (openllm.json: lb_strategy)
	if d.cfg.LBStrategy != "" {
		d.proxyServer.SetStrategy(d.cfg.LBStrategy)
	}

	// Health-checks em background: alvos doentes saem do pool de balanceamento
	d.proxyServer.StartHealthChecks(ctx)

	// Verifica se o provider ativo é do tipo Inference
	if providers.IsInference(d.cfg.Provider) {
		log.Printf("Active provider is an Inference provider (%s). Registering remote API target...", d.cfg.Provider)
		d.setupInferenceTarget()
	} else {
		// 1. Tenta reconectar instâncias que estavam rodando no banco de dados (Compute)
		d.recoverActiveInstances()

		// 2. Inicia loop de monitoramento de saúde das instâncias (Compute)
		go d.monitorHealthLoop(ctx)

		// 3. Inicia autoscaler (Compute) — usa policies default se configurado
		d.autoscaler.Start(ctx)

		// 4. Aplica policies default do config para grupos existentes
		if d.cfg.AutoscalerEnabled {
			d.applyDefaultAutoscalerPolicies()
		}
	}

	// 3. Executa os servidores HTTP em paralelo
	var wg sync.WaitGroup
	errChan := make(chan error, 3)

	runServer := func(name string, f func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("Starting service: %s", name)
			if err := f(ctx); err != nil {
				log.Printf("Service %s failed: %v", name, err)
				errChan <- err
			}
		}()
	}

	runServer("Ollama Proxy Server", d.proxyServer.Start)
	runServer("Control API Server", d.controlAPI.Start)
	runServer("Watchdog Heartbeat Server", d.heartbeat.Start)

	// Aguarda cancelamento do contexto ou erro
	select {
	case <-ctx.Done():
		log.Println("Stopping daemon...")
	case err := <-errChan:
		log.Printf("Fatal daemon service failure: %v", err)
		return err
	}

	wg.Wait()
	d.db.Close()
	return nil
}

// setupInferenceTarget registra o backend remoto correspondente no proxy de forma transparente
func (d *Daemon) setupInferenceTarget() {
	p, ok := providers.GetInference(d.cfg.Provider)
	if !ok {
		return
	}

	// AddTarget faz merge — não derruba backends locais existentes
	d.proxyServer.AddTarget(proxy.Target{
		InstanceID:  "remote-" + d.cfg.Provider,
		Type:        proxy.TargetTypeRemote,
		Model:       d.cfg.Model,
		BaseURL:     p.BaseURL(),
		AuthHeaders: p.AuthHeaders(d.cfg.ActiveAPIKey()),
	})
}

// applyDefaultAutoscalerPolicies cria policies default do config para
// todos os grupos que existem no banco.
func (d *Daemon) applyDefaultAutoscalerPolicies() {
	groups, err := d.db.ListGroups()
	if err != nil {
		log.Printf("[autoscaler] failed to list groups: %v", err)
		return
	}
	for _, g := range groups {
		if g.ID == "" {
			continue
		}
		parts := strings.SplitN(g.ID, "/", 2)
		if len(parts) != 2 {
			continue
		}
		min := d.cfg.AutoscalerMin
		if min < 1 {
			min = 1
		}
		max := d.cfg.AutoscalerMax
		if max < min {
			max = min
		}
		target := d.cfg.AutoscalerTarget
		if target <= 0 {
			target = 2
		}
		d.autoscaler.SetPolicy(g.ID, AutoscalerConfig{
			GroupID:        g.ID,
			MinInstances:   min,
			MaxInstances:   max,
			TargetInFlight: target,
			MaxCostPerHour: d.cfg.AutoscalerMaxCost,
			CooldownSec:    60,
			Enabled:        true,
		})
	}
}

// recoverActiveInstances lê instâncias com status "running" ou "deploying" do DB no boot do daemon
// e tenta re-estabelecer a conexão e túneis SSH em background.
func (d *Daemon) recoverActiveInstances() {
	activeInsts, err := d.db.ListActiveInstances()
	if err != nil {
		log.Printf("Database recover error: %v", err)
		return
	}

	if len(activeInsts) == 0 {
		return
	}

	privKeyPath, _, err := ssh.EnsureKeysExist(d.cfg.ActiveAPIKey())
	if err != nil {
		log.Printf("SSH recovery setup failed: %v", err)
		return
	}

	log.Printf("Found %d active instances from previous execution. Recovering tunnels...", len(activeInsts))
	for _, inst := range activeInsts {
		// Se estava "deploying" ou "running", iniciamos o pipeline de setup em background.
		// "deploying" é um deploy que NUNCA terminou: no resume vale a regra de
		// deploy novo — qualquer falha destrói a máquina (anti-vazamento).
		// "running" é reconexão de máquina que já serviu: preservar em caso
		// de falha transitória.
		destroyOnFail := inst.Status == "deploying"
		go d.manager.backgroundSetup(d.cfg, inst, privKeyPath, destroyOnFail)
	}
}

// monitorHealthLoop verifica se a máquina foi deletada no provedor ou se
// perdeu conexão. Tolerante a falsos "not found" da API: só declara a
// instância morta após notFoundConfirmations falhas CONSECUTIVAS.
func (d *Daemon) monitorHealthLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	notFound := map[string]int{}
	const requiredConfirmations = 3

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Verifica se o provider ainda é compute
			client, ok := providers.GetCompute(d.cfg.Provider)
			if !ok {
				return // encerra o loop se o provider mudou para inference dinamicamente
			}

			actives := d.manager.GetActiveInstances()
			activeSet := map[string]bool{}
			for _, act := range actives {
				activeSet[act.ID] = true

				// 1. Verifica se recebemos heartbeats do watchdog remoto
				lastPing, exists := d.manager.GetLastPing(act.ID)
				timeoutDuration := time.Duration(d.cfg.WatchdogTimeoutMinutes)*time.Minute + 1*time.Minute
				if exists && time.Since(lastPing) > timeoutDuration {
					log.Printf("[%s] WARNING: Missed heartbeat. Trying to reconnect SSH...", act.ID)
					privKeyPath, _, _ := ssh.EnsureKeysExist(d.cfg.ActiveAPIKey())
					go d.manager.backgroundSetup(d.cfg, act.Instance, privKeyPath, false)
					continue
				}

				// 2. Verifica se a máquina ainda existe no provedor.
				// Falsos "not found" (rede, rate limit, loading) são confirmados
				// N vezes consecutivas antes de declarar a instância morta.
				status, err := client.GetStatus(ctx, act.ID, d.cfg.ActiveAPIKey())
				if err != nil {
					if strings.Contains(err.Error(), "not found") {
						notFound[act.ID]++
						log.Printf("[%s] Not found in provider list (confirmation %d/%d)", act.ID, notFound[act.ID], requiredConfirmations)
						if notFound[act.ID] >= requiredConfirmations {
							log.Printf("[%s] Confirmed deleted remotely (x%d). Cleaning up.", act.ID, requiredConfirmations)
							delete(notFound, act.ID)
							d.manager.StopInstance(d.cfg, act.ID)
						}
						continue
					}
					// Erro transitório (rede, rate limit) — não destrói nada
					log.Printf("[%s] Transient verification error: %v. Keeping instance.", act.ID, err)
					continue
				}

				// API respondeu OK → reset da contagem de not-found
				delete(notFound, act.ID)

				if status.Status == "stopped" || status.Status == "failed" {
					log.Printf("[%s] Instance was stopped/failed remotely. Cleaning up.", act.ID)
					d.manager.StopInstance(d.cfg, act.ID)
				}
			}

			// Limpa contadores de instâncias que saíram do pool
			for id := range notFound {
				if !activeSet[id] {
					delete(notFound, id)
				}
			}
		}
	}
}

// ============================================================
// Autoscaler opcional (Fase 2b)
// ============================================================

// AutoscalerConfig define a política de auto-scaling para um grupo.
type AutoscalerConfig struct {
	GroupID        string  // engine/model
	MinInstances   int     // mínimo (>=1)
	MaxInstances   int     // máximo
	TargetInFlight int     // target de requisições em voo por instância
	MaxCostPerHour float64 // teto de custo/hora total do grupo
	CooldownSec    int     // segundos entre decisões de scale (default 60)
	Enabled        bool    // habilitado ou não
}

// Autoscaler monitora concorrência no proxy e ajusta escala do grupo.
type Autoscaler struct {
	cfg        *config.Config
	db         *storage.DB
	manager    *InstanceManager
	proxy      *proxy.ProxyServer
	policy     map[string]AutoscalerConfig // GroupID -> config
	mu         sync.RWMutex
	stopCh     chan struct{}
	wg         sync.WaitGroup
}

// NewAutoscaler cria o autoscaler (não inicia o loop).
func NewAutoscaler(cfg *config.Config, db *storage.DB, mgr *InstanceManager, ps *proxy.ProxyServer) *Autoscaler {
	return &Autoscaler{
		cfg:     cfg,
		db:      db,
		manager: mgr,
		proxy:   ps,
		policy:  make(map[string]AutoscalerConfig),
		stopCh:  make(chan struct{}),
	}
}

// SetPolicy define/atualiza a política para um grupo.
func (a *Autoscaler) SetPolicy(groupID string, p AutoscalerConfig) {
	if p.MinInstances < 1 {
		p.MinInstances = 1
	}
	if p.MaxInstances < p.MinInstances {
		p.MaxInstances = p.MinInstances
	}
	if p.TargetInFlight <= 0 {
		p.TargetInFlight = 2 // default: 2 reqs em voo por instância
	}
	if p.CooldownSec <= 0 {
		p.CooldownSec = 60
	}
	a.mu.Lock()
	a.policy[groupID] = p
	a.mu.Unlock()
	log.Printf("[autoscaler] policy set for %s: min=%d max=%d targetInFlight=%d maxCost=%.2f/h enabled=%v",
		groupID, p.MinInstances, p.MaxInstances, p.TargetInFlight, p.MaxCostPerHour, p.Enabled)
}

// Start inicia o loop de auto-scaling em background.
func (a *Autoscaler) Start(ctx context.Context) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		log.Println("[autoscaler] loop started")
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		lastDecision := map[string]time.Time{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-a.stopCh:
				return
			case <-ticker.C:
				a.mu.RLock()
				policies := make(map[string]AutoscalerConfig, len(a.policy))
				for k, v := range a.policy {
					policies[k] = v
				}
				a.mu.RUnlock()

				for groupID, p := range policies {
					if !p.Enabled {
						continue
					}
					// Cooldown
					if last, ok := lastDecision[groupID]; ok && time.Since(last) < time.Duration(p.CooldownSec)*time.Second {
						continue
					}
					if err := a.evaluate(ctx, groupID, p); err != nil {
						log.Printf("[autoscaler] %s: %v", groupID, err)
					}
					lastDecision[groupID] = time.Now()
				}
			}
		}
	}()
}

// Stop para o loop.
func (a *Autoscaler) Stop() {
	close(a.stopCh)
	a.wg.Wait()
	log.Println("[autoscaler] loop stopped")
}

// evaluate avalia uma política e decide scale up/down.
func (a *Autoscaler) evaluate(ctx context.Context, groupID string, p AutoscalerConfig) error {
	// 1. Conta instâncias ativas do grupo
	active, err := a.manager.activeGroupInstances(groupID)
	if err != nil {
		return err
	}
	current := len(active)

	// 2. Soma in-flight do proxy para este grupo
	inFlight := a.proxyInFlightForGroup(groupID)

	// 3. Calcula custo atual/hora
	var currentCost float64
	for _, inst := range active {
		currentCost += inst.CostPerHour
	}

	// 4. Decide target baseado em in-flight
	// target = ceil(inFlight / TargetInFlight)
	// clamp entre min/max
	desired := (inFlight + p.TargetInFlight - 1) / p.TargetInFlight
	if desired < p.MinInstances {
		desired = p.MinInstances
	}
	if desired > p.MaxInstances {
		desired = p.MaxInstances
	}

	// 5. Budget check: se desired * avgCost > maxCost, reduz
	if p.MaxCostPerHour > 0 && current > 0 {
		avgCost := currentCost / float64(current)
		if float64(desired)*avgCost > p.MaxCostPerHour {
			desired = int(p.MaxCostPerHour / avgCost)
			if desired < p.MinInstances {
				desired = p.MinInstances
			}
		}
	}

	if desired == current {
		return nil // já no target
	}

	log.Printf("[autoscaler] %s: inFlight=%d current=%d desired=%d (cost=%.2f/h targetInFlight=%d)",
		groupID, inFlight, current, desired, currentCost, p.TargetInFlight)

	// 6. Executa scale
	// Note: ScaleGroup precisa engine/model. Parse groupID.
	parts := strings.SplitN(groupID, "/", 2)
	if len(parts) == 2 {
		_, err = a.manager.ScaleGroup(a.cfg, parts[0], parts[1], desired, 0)
	}
	return err
}

// proxyInFlightForGroup soma conexões em voo das instâncias do grupo.
func (a *Autoscaler) proxyInFlightForGroup(groupID string) int {
	var total int64
	a.proxy.TargetsSnapshot(func(targets []proxy.Target) {
		for _, t := range targets {
			if t.GroupID == groupID {
				if ptr := a.proxy.InFlightFor(t.InstanceID); ptr != nil {
					total += atomic.LoadInt64(ptr)
				}
			}
		}
	})
	return int(total)
}
