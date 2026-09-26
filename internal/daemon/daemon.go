package daemon

import (
	"context"
	"log"
	"strings"
	"sync"
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

	return &Daemon{
		cfg:         cfg,
		db:          db,
		manager:     mgr,
		proxyServer: ps,
		controlAPI:  ctrl,
		heartbeat:   hb,
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
