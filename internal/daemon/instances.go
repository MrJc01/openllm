package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crom-org/openllm/internal/config"
	"github.com/crom-org/openllm/internal/engines"
	"github.com/crom-org/openllm/internal/models"
	"github.com/crom-org/openllm/internal/providers"
	"github.com/crom-org/openllm/internal/proxy"
	"github.com/crom-org/openllm/internal/ssh"
	"github.com/crom-org/openllm/internal/stack"
	"github.com/crom-org/openllm/internal/storage"
)

// Parâmetros de retry da conexão SSH. Atômicos para permitir que testes de
// integração os ajustem com segurança enquanto goroutines de setup rodam
// (produção usa os defaults: 60 tentativas, 5s de intervalo).
var (
	sshConnectRetries    atomic.Int64
	sshConnectRetryDelay atomic.Int64
)

func init() {
	sshConnectRetries.Store(60)
	sshConnectRetryDelay.Store(int64(5 * time.Second))
}

func sshRetryConfig() (int, time.Duration) {
	return int(sshConnectRetries.Load()), time.Duration(sshConnectRetryDelay.Load())
}

type ActiveInstance struct {
	storage.Instance
	LocalPort  int
	SSHClient  *ssh.SSHClient
	CancelFunc context.CancelFunc
	// ExtraModels: modelos adicionais servidos pela mesma engine (ollama
	// mantém vários carregados enquanto couberem na VRAM). O proxy roteia
	// cada um para esta instância. Persistido em instance_models e restaurado
	// na reconexão (restoreModels).
	ExtraModels []string
	// ModelStates: estado de cada modelo nesta máquina ("loading", "ready",
	// "failed"), inclusive o alvo de um swap/add ainda baixando. Espelhado em
	// instance_models (último estado conhecido sobrevive a restarts).
	ModelStates map[string]string
	// ModelProgress: última leitura do download de cada modelo (texto curto).
	ModelProgress map[string]string
	// ModelDetails: motivo da última falha de cada modelo.
	ModelDetails map[string]string
	// ctx: vida desta encarnação (cancelado no stop/reconexão); goroutines de
	// modelos param com ele.
	ctx context.Context
}

// ModelStatesCopy devolve uma cópia dos estados (seguro para serializar).
func (a *ActiveInstance) ModelStatesCopy() map[string]string {
	if len(a.ModelStates) == 0 {
		return nil
	}
	return copyMap(a.ModelStates)
}

func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// closeConn cancela os túneis e fecha o SSH (tolerante a campos nulos).
func (a *ActiveInstance) closeConn() {
	if a.CancelFunc != nil {
		a.CancelFunc()
	}
	if a.SSHClient != nil {
		a.SSHClient.Close()
	}
}

// Models devolve o modelo principal seguido dos extras.
func (a *ActiveInstance) Models() []string {
	return append([]string{a.Model}, a.ExtraModels...)
}

type InstanceManager struct {
	db          *storage.DB
	proxyServer *proxy.ProxyServer
	actives     map[string]*ActiveInstance
	activesMu   sync.RWMutex
	lastPings   map[string]time.Time
	pingsMu     sync.RWMutex
	logs        map[string][]string
	logsMu      sync.RWMutex
	// badHosts: máquinas físicas que falharam no pull da imagem; /search as
	// evita por badHostTTL (host sem rede costuma continuar sem rede).
	badHosts   map[string]time.Time
	badHostsMu sync.Mutex
	// Tempos do fluxo de modelos (zero = default; testes encurtam).
	modelWait      time.Duration // espera máxima por um modelo (20 min)
	modelPoll      time.Duration // intervalo do comando de prontidão (3s)
	reconcileEvery time.Duration // re-checagem dos extras roteados (2 min)
	// modelVersion: relógio monotônico das escritas em instance_models.
	modelVersion atomic.Int64
}

const badHostTTL = 24 * time.Hour

func (m *InstanceManager) markBadHost(hostID string) {
	if hostID == "" || hostID == "0" {
		return
	}
	m.badHostsMu.Lock()
	defer m.badHostsMu.Unlock()
	m.loadBadHostsLocked()
	m.badHosts[hostID] = time.Now()
	if data, err := json.Marshal(m.badHosts); err == nil {
		_ = ioutil.WriteFile(badHostsPath(), data, 0600)
	}
}

// badHostsPath: persistido no state dir (.openllm) para sobreviver a restarts.
func badHostsPath() string {
	dir, err := config.GetStateDir()
	if err != nil {
		dir = config.StateDirName
	}
	return filepath.Join(dir, "bad_hosts.json")
}

func (m *InstanceManager) loadBadHostsLocked() {
	if m.badHosts != nil {
		return
	}
	m.badHosts = map[string]time.Time{}
	if data, err := ioutil.ReadFile(badHostsPath()); err == nil {
		_ = json.Unmarshal(data, &m.badHosts)
	}
}

// BadHosts devolve os hosts ainda dentro do TTL de exclusão.
func (m *InstanceManager) BadHosts() []string {
	m.badHostsMu.Lock()
	defer m.badHostsMu.Unlock()
	m.loadBadHostsLocked()
	var out []string
	for id, at := range m.badHosts {
		if time.Since(at) < badHostTTL {
			out = append(out, id)
		} else {
			delete(m.badHosts, id)
		}
	}
	return out
}

func NewInstanceManager(db *storage.DB, ps *proxy.ProxyServer) *InstanceManager {
	return &InstanceManager{
		db:          db,
		proxyServer: ps,
		actives:     make(map[string]*ActiveInstance),
		lastPings:   make(map[string]time.Time),
		logs:        make(map[string][]string),
	}
}

// ErrCustomDisabled: custom_image/custom_cmd exigem OPENLLM_ALLOW_CUSTOM=1.
var ErrCustomDisabled = errors.New("custom_image/custom_cmd are disabled (set OPENLLM_ALLOW_CUSTOM=1 on the daemon to enable)")

// KnownInstance: instância ativa em memória ou deploying/running no banco
// (o heartbeat só aceita pings dessas).
func (m *InstanceManager) KnownInstance(id string) bool {
	m.activesMu.RLock()
	_, ok := m.actives[id]
	m.activesMu.RUnlock()
	if ok {
		return true
	}
	if m.db == nil {
		return false
	}
	inst, err := m.db.GetInstance(id)
	return err == nil && inst != nil && (inst.Status == "deploying" || inst.Status == "running")
}

// GroupID deriva o identificador canônico do grupo de escala: engine/model.
func GroupID(engine, model string) string {
	return engine + "/" + model
}

func (m *InstanceManager) AddLog(instanceID string, msg string) {
	m.logsMu.Lock()
	defer m.logsMu.Unlock()
	timestamp := time.Now().Format("15:04:05")
	formattedMsg := fmt.Sprintf("[%s] %s", timestamp, msg)
	m.logs[instanceID] = append(m.logs[instanceID], formattedMsg)
	// Limita aos últimos 100 logs por instância
	if len(m.logs[instanceID]) > 100 {
		m.logs[instanceID] = m.logs[instanceID][len(m.logs[instanceID])-100:]
	}
	log.Printf("[%s] %s", instanceID, msg)
	appendInstanceLog(instanceID, time.Now().Format(time.RFC3339)+" "+msg)
}

// appendInstanceLog guarda o histórico completo de cada instância em
// .openllm/logs/instances/<id>.log (a memória só mantém as últimas 100).
func appendInstanceLog(instanceID, line string) {
	dir, err := config.GetStateDir()
	if err != nil {
		return
	}
	dir = filepath.Join(dir, "logs", "instances")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, instanceID+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line + "\n")
}

func (m *InstanceManager) GetLogs(instanceID string) []string {
	m.logsMu.RLock()
	defer m.logsMu.RUnlock()
	if l, exists := m.logs[instanceID]; exists {
		result := make([]string, len(l))
		copy(result, l)
		return result
	}
	return []string{}
}

func (m *InstanceManager) RegisterPing(instanceID string) {
	m.pingsMu.Lock()
	defer m.pingsMu.Unlock()
	m.lastPings[instanceID] = time.Now()
	m.AddLog(instanceID, "Heartbeat ping recebido do watchdog")
}

func (m *InstanceManager) GetLastPing(instanceID string) (time.Time, bool) {
	m.pingsMu.RLock()
	defer m.pingsMu.RUnlock()
	t, exists := m.lastPings[instanceID]
	return t, exists
}

// FindFreePort aloca uma porta TCP local livre do sistema
func FindFreePort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// resolveEngineDef recupera a Definition completa da engine de uma instância.
// Instâncias novas carregam a Definition serializada (coluna def_json); mais
// antigas fazem lookup pelo nome. "custom" sem def salva usa o fallback.
func resolveEngineDef(inst storage.Instance) engines.Definition {
	if inst.EngineDefJSON != "" {
		var def engines.Definition
		if err := json.Unmarshal([]byte(inst.EngineDefJSON), &def); err == nil && def.Name != "" {
			// O container (imagem, env, portas) é o do deploy, mas os comandos
			// de modelo vêm do registry atual: senão uma máquina antiga nunca
			// recebe correções (ex: ready do SDXL caía no SD 1.5 e mentia).
			if cur, ok := engines.TryGet(def.Name); ok && cur.DockerImage == def.DockerImage {
				def.ModelPullCmd = cur.ModelPullCmd
				def.ModelReadyCmd = cur.ModelReadyCmd
				def.WarmupCmd = cur.WarmupCmd
			}
			return def
		}
	}
	if def, ok := engines.TryGet(inst.Engine); ok {
		return def
	}
	return engines.Get("ollama")
}

// DeployInstance inicia a thread de deploy em background
func (m *InstanceManager) DeployInstance(cfg *config.Config, payload DeployRequestPayload) (string, error) {
	if payload.Engine == "" {
		payload.Engine = "ollama"
	}
	if (payload.CustomImage != "" || payload.CustomCmd != "") && os.Getenv("OPENLLM_ALLOW_CUSTOM") != "1" {
		return "", ErrCustomDisabled
	}

	client, ok := providers.GetCompute(cfg.Provider)
	if !ok {
		return "", fmt.Errorf("provider %s is not registered or is not a compute provider", cfg.Provider)
	}

	// 1. Garante que chaves SSH existem e estão registradas no Vast.ai
	privKeyPath, _, err := ssh.EnsureKeysExist(cfg.ActiveAPIKey())
	if err != nil {
		return "", fmt.Errorf("failed to configure SSH keys: %w", err)
	}

	// 2. Cria a requisição de deploy com Imagem Docker nativa da Engine
	engineDef := engines.GetCustom(payload.Engine, payload.Capabilities, payload.CustomImage, payload.CustomCmd, payload.CustomPort)
	image := engineDef.DockerImage
	if image == "" {
		image = "nvidia/cuda:12.1.1-devel-ubuntu22.04"
	}

	ctx := context.Background()
	req := providers.DeployRequest{
		MachineID:  payload.MachineID,
		Model:      payload.Model,
		Image:      image,
		OnstartCmd: engines.Render(engineDef.OnStartCmd, payload.Model),
		APIKey:     cfg.ActiveAPIKey(),
		Engine:     engineDef.Name,
		Env:        renderEnv(engineDef.Env, payload.Model),
		DiskGB:     engineDef.DiskGB,
	}

	// Preço reconferido imediatamente antes do aluguel (ofertas mudam de preço).
	if payload.MaxCostPerHour > 0 {
		pricer, ok := client.(providers.OfferPricer)
		if !ok {
			return "", fmt.Errorf("provider %s cannot verify offer price for max_cost_per_hour", cfg.Provider)
		}
		price, err := pricer.OfferPrice(ctx, payload.MachineID, cfg.ActiveAPIKey())
		if err != nil {
			return "", fmt.Errorf("could not verify offer price: %w", err)
		}
		if err := providers.CheckOfferPrice(price, payload.MaxCostPerHour); err != nil {
			return "", err
		}
	}

	log.Printf("Starting deploy on %s for machine %s, model %s, engine %s...", cfg.Provider, payload.MachineID, payload.Model, engineDef.Name)
	info, err := client.Deploy(ctx, req)
	if err != nil {
		return "", err
	}

	// 3. Salva no SQLite como "deploying"
	defJSON, _ := json.Marshal(engineDef)
	inst := storage.Instance{
		ID:            info.ID,
		Provider:      cfg.Provider,
		MachineID:     payload.MachineID,
		Status:        "deploying",
		Model:         payload.Model,
		Engine:        engineDef.Name,
		GroupID:       GroupID(engineDef.Name, payload.Model),
		EngineDefJSON: string(defJSON),
		CreatedAt:     time.Now(),
		CostPerHour:   info.CostPerHour, // será atualizado quando rodar
	}
	if err := m.db.SaveInstance(&inst); err != nil {
		log.Printf("Database warning: failed to save deploying instance: %v", err)
	}

	// 4. Dispara a rotina de setup em background
	go m.backgroundSetup(cfg, inst, privKeyPath, true)

	return info.ID, nil
}

// backgroundSetup executa todo o ciclo: espera a máquina ativar, conecta SSH,
// instala engine, watchdog e túneis.
//
// destroyOnFail: true para deploys NOVOS — qualquer falha destrói a máquina
// (evita instância alugada queimando dinheiro sem ninguém usar). false para
// reconexões (instância já existia — preservar em caso de falha transitória).
func (m *InstanceManager) backgroundSetup(cfg *config.Config, inst storage.Instance, privKeyPath string, destroyOnFail bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		if inst.Status != "running" {
			cancel() // cancela o contexto se falhar
		}
	}()

	client, ok := providers.GetCompute(cfg.Provider)
	if !ok {
		log.Printf("[%s] Error: provider %s is not a compute provider", inst.ID, cfg.Provider)
		m.failSetup(cfg, inst, "invalid provider type", destroyOnFail)
		return
	}
	var sshInfo *providers.InstanceInfo
	var err error

	// A. Aguarda a máquina ficar ativa e obter SSH Host/Port
	log.Printf("[%s] Waiting for instance to become active...", inst.ID)

	deployTimeout := time.Duration(cfg.DeployTimeoutMinutes) * time.Minute
	if cfg.DeployTimeoutMinutes <= 0 {
		deployTimeout = 30 * time.Minute
	}
	timeoutChan := time.After(deployTimeout)
	tick := time.After(0) // 1ª checagem imediata; depois a cada 30s
	const maxPullErrors = 4
	pullErrors := 0

WaitLoop:
	for {
		select {
		case <-timeoutChan:
			log.Printf("[%s] Error: timeout waiting for instance setup", inst.ID)
			m.failSetup(cfg, inst, "timeout waiting for container setup", destroyOnFail)
			return
		case <-tick:
			tick = time.After(30 * time.Second)
			sshInfo, err = client.GetStatus(context.Background(), inst.ID, cfg.ActiveAPIKey())
			if err != nil {
				log.Printf("[%s] GetStatus error: %v", inst.ID, err)
				if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "no such") {
					// Instância confirmada deletada no provedor. Se já estava
					// running (reconexão), limpa o fantasma; se era deploy
					// novo, marca failed — mas nunca fica em loop eterno.
					m.cleanupDeletedInstance(inst)
					return
				}
				continue
			}

			// Atualiza custos e specs no db se mudou
			inst.GPU = sshInfo.GPU
			inst.GPUCount = sshInfo.GPUCount
			inst.VRAM = sshInfo.VRAM
			inst.CostPerHour = sshInfo.CostPerHour
			inst.SSHHost = sshInfo.SSHHost
			inst.SSHPort = sshInfo.SSHPort
			m.db.SaveInstance(&inst)

			if sshInfo.StatusMsg != "" {
				m.AddLog(inst.ID, fmt.Sprintf("Vast.ai Progress [%s]: %s", sshInfo.Status, strings.TrimSpace(sshInfo.StatusMsg)))
			}

			// Host sem acesso ao registry (ex: TLS handshake timeout no pull)
			// não se recupera sozinho: desiste após ~2min em vez de pagar 30min.
			if sshInfo.Status != "running" && strings.Contains(sshInfo.StatusMsg, "Error response from daemon") {
				pullErrors++
				if pullErrors >= maxPullErrors {
					log.Printf("[%s] Error: host %s cannot pull image: %s", inst.ID, sshInfo.MachineID, strings.TrimSpace(sshInfo.StatusMsg))
					m.markBadHost(sshInfo.MachineID)
					m.failSetup(cfg, inst, "host cannot pull docker image: "+strings.TrimSpace(sshInfo.StatusMsg), destroyOnFail)
					return
				}
			} else {
				pullErrors = 0
			}

			if sshInfo.Status == "running" && sshInfo.SSHHost != "" && sshInfo.SSHPort != 0 {
				log.Printf("[%s] Instance is running. SSH target: %s:%d", inst.ID, sshInfo.SSHHost, sshInfo.SSHPort)
				break WaitLoop
			}

			if sshInfo.Status == "failed" || sshInfo.Status == "stopped" {
				log.Printf("[%s] Instance setup failed or stopped", inst.ID)
				m.failSetup(cfg, inst, "instance reported failed state", destroyOnFail)
				return
			}
		}
	}

	// B. Aguarda SSH aceitar conexões (às vezes o container inicia mas o daemon SSH demora mais)
	var sshClient *ssh.SSHClient
	maxRetries, retryDelay := sshRetryConfig()
	// "connection refused" é normal enquanto o sshd sobe. Já "unable to
	// authenticate" repetido significa que o túnel do proxy Vast desta
	// máquina falhou e outro container responde na porta: não se resolve
	// esperando, então desiste em ~1 min em vez de ~6.
	const maxAuthFailures = 12
	authFailures := 0
	for i := 0; i < maxRetries; i++ {
		sshClient, err = ssh.Connect(sshInfo.SSHHost, sshInfo.SSHPort, privKeyPath)
		if err == nil {
			break
		}
		// Proxy falhando: tenta o SSH direto pelo IP público do host.
		if i >= 2 {
			if fresh, ferr := client.GetStatus(context.Background(), inst.ID, cfg.ActiveAPIKey()); ferr == nil && fresh.DirectSSHPort > 0 {
				if c, derr := ssh.Connect(fresh.DirectSSHHost, fresh.DirectSSHPort, privKeyPath); derr == nil {
					m.AddLog(inst.ID, fmt.Sprintf("SSH proxy failing; connected directly to %s:%d", fresh.DirectSSHHost, fresh.DirectSSHPort))
					sshClient, err = c, nil
					sshInfo.SSHHost, sshInfo.SSHPort = fresh.DirectSSHHost, fresh.DirectSSHPort
					break
				}
			}
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			authFailures++
			if authFailures >= maxAuthFailures {
				m.markBadHost(sshInfo.MachineID)
				break
			}
		} else {
			authFailures = 0
		}
		log.Printf("[%s] SSH not ready yet, retrying... (%v)", inst.ID, err)
		time.Sleep(retryDelay)
	}

	if err != nil {
		log.Printf("[%s] Failed to connect to SSH: %v", inst.ID, err)
		m.failSetup(cfg, inst, fmt.Sprintf("failed to connect via SSH: %v", err), destroyOnFail)
		return
	}
	// ATENÇÃO: a conexão SSH pertence ao ActiveInstance (túneis vivem nela).
	// NÃO fechar aqui — o fechamento acontece no StopInstance/redeploy. Um
	// defer Close neste ponto mata os túneis assim que o setup termina e o
	// watchdog remoto auto-destrói a máquina por falta de pings.

	// Recupera a Definition da engine (dirigida por dados)
	def := resolveEngineDef(inst)

	// C. Instala a engine (comando da Definition) e copia o watchdog
	log.Printf("[%s] Preparing remote container environment (engine: %s)...", inst.ID, inst.Engine)

	if err := m.setupEngine(sshClient, inst, def); err != nil {
		log.Printf("[%s] Engine setup warning/error: %v. Proceeding...", inst.ID, err)
	}

	// Watchdog: script shell (~1KB, 1 comando SSH) quando o host tem curl.
	// Copiar o binário Go (8MB) pelo proxy SSH levava minutos em hosts
	// distantes. O binário fica como fallback para imagens sem curl.
	_, _ = sshClient.RunCommand("pkill -f openllm-watchdog || true")
	watchdogLaunch, err := m.installWatchdog(sshClient, inst, cfg)
	if err != nil {
		log.Printf("[%s] Failed to install watchdog: %v", inst.ID, err)
		m.failSetup(cfg, inst, fmt.Sprintf("failed to install watchdog: %v", err), destroyOnFail)
		return
	}

	// D. Inicia a engine (download do modelo em background, se houver pull cmd)
	log.Printf("[%s] Starting engine %s (model: %s)...", inst.ID, inst.Engine, inst.Model)

	if err := m.startEngine(sshClient, inst, def); err != nil {
		log.Printf("[%s] Engine start command error: %v", inst.ID, err)
	}

	// E. Aguarda a engine reportar pronta (health-check por SSH, sem sleep fixo)
	// A porta vem da Definition persistida no deploy — engines custom têm porta
	// própria (ex.: ComfyUI 8188) e o registry builtin não conhece "custom";
	// usar o registry pelo nome fazia o health/túnel apontarem para a porta do
	// ollama (11434) mesmo com a porta correta persistida em def_json.
	remoteEnginePort := effectiveRemotePort(def, cfg, inst.Engine)
	if err := m.waitForEngineReady(sshClient, inst, def, remoteEnginePort); err != nil {
		m.AddLog(inst.ID, fmt.Sprintf("WARNING: %v. Continuing (tunnel will be live anyway).", err))
	}

	// F. Executa o Watchdog em background no host remoto
	// Ele pingará local:17291 que mapeia via túnel reverso para o daemon
	_, err = sshClient.RunCommand(watchdogLaunch)
	if err != nil {
		log.Printf("[%s] Warning: failed to start watchdog on host: %v", inst.ID, err)
	}

	// G. Configura túnel reverso (heartbeat)
	log.Printf("[%s] Establishing reverse SSH tunnel for watchdog...", inst.ID)
	remoteHeartbeatAddr := fmt.Sprintf("127.0.0.1:%d", cfg.HeartbeatPort)
	localHeartbeatAddr := fmt.Sprintf("127.0.0.1:%d", cfg.HeartbeatPort)
	err = sshClient.StartReverseTunnel(ctx, remoteHeartbeatAddr, localHeartbeatAddr)
	if err != nil {
		log.Printf("[%s] Failed to start reverse tunnel: %v", inst.ID, err)
		m.failSetup(cfg, inst, fmt.Sprintf("failed to setup reverse tunnel: %v", err), destroyOnFail)
		return
	}

	// H. Aloca porta local para a engine
	localEnginePort, err := FindFreePort()
	if err != nil {
		log.Printf("[%s] Failed to find free port for engine tunnel: %v", inst.ID, err)
		m.failSetup(cfg, inst, fmt.Sprintf("no free ports available: %v", err), destroyOnFail)
		return
	}

	// I. Configura túnel direto (local forward) para a engine
	log.Printf("[%s] Establishing direct SSH forward on local port %d to remote %s (port %d)...", inst.ID, localEnginePort, inst.Engine, remoteEnginePort)
	localForwardAddr := fmt.Sprintf("127.0.0.1:%d", localEnginePort)
	remoteEngineAddr := fmt.Sprintf("127.0.0.1:%d", remoteEnginePort)
	err = sshClient.StartLocalForward(ctx, localForwardAddr, remoteEngineAddr)
	if err != nil {
		log.Printf("[%s] Failed to start direct tunnel: %v", inst.ID, err)
		m.failSetup(cfg, inst, fmt.Sprintf("failed to setup direct port forward: %v", err), destroyOnFail)
		return
	}

	// J. Persiste host/porta (status continua "deploying" até o modelo estar pronto)
	inst.SSHHost = sshInfo.SSHHost
	inst.SSHPort = sshInfo.SSHPort
	m.db.SaveInstance(&inst)

	// K. Adiciona na lista de ativos
	m.activesMu.Lock()
	act := &ActiveInstance{
		Instance:   inst,
		LocalPort:  localEnginePort,
		SSHClient:  sshClient,
		CancelFunc: cancel,
		ctx:        ctx,
	}
	m.actives[inst.ID] = act
	m.activesMu.Unlock()

	// K2. Restaura extras/estados persistidos (reconexão após restart)
	persisted, rerr := m.restoreModels(act)
	if rerr != nil {
		m.AddLog(inst.ID, fmt.Sprintf("WARNING: persisted models unavailable (%v); extras not restored", rerr))
	}

	// L. Atualiza alvos no load balancer
	m.updateProxyTargets()

	// Registra o primeiro ping manualmente para iniciar
	m.RegisterPing(inst.ID)

	// M. Aguarda o modelo ficar disponível (pull em background no host remoto).
	// O watchdog e os túneis já estão ativos — a instância não vaza se o
	// daemon morrer durante o download. "Running" só aparece quando o modelo
	// pode de fato ser consumido.
	m.resumeModels(ctx, act, sshClient, def, persisted)
	go m.reconcileLoop(ctx, act, sshClient, def)
	if err := m.waitForModelReady(ctx, act, sshClient, def, inst.Model); err != nil {
		m.AddLog(inst.ID, fmt.Sprintf("WARNING: %v", err))
	} else if _, ready := modelCmds(def, inst.Model); strings.TrimSpace(ready) == "" {
		m.setModelState(act, inst.Model, "ready", "") // sem comando de prontidão
	}

	// N. Modelo pronto (ou timeout avisado) → marca running no banco E nos
	// ativos em memória (o load balancer filtra pelo status dos ativos)
	inst.Status = "running"
	m.db.SaveInstance(&inst)

	m.activesMu.Lock()
	if a, ok := m.actives[inst.ID]; ok {
		a.Status = "running"
	}
	m.activesMu.Unlock()
	m.updateProxyTargets()

	log.Printf("[%s] SUCCESS: Instance fully deployed and ready!", inst.ID)
}

func (m *InstanceManager) markFailed(inst storage.Instance, reason string) {
	log.Printf("[%s] Deploy failed: %s", inst.ID, reason)
	inst.Status = "failed"
	m.db.SaveInstance(&inst)
}

// failSetup encerra um setup com falha. Para deploys novos (destroyOnFail),
// destrói a instância no provedor — a máquina não pode continuar alugada
// sem ninguém usando (vazamento de dinheiro). A destruição passa pela guarda
// de label em StopInstance (nunca toca instâncias de outros projetos).
// Para reconexões, preserva a instância e apenas registra a falha.
func (m *InstanceManager) failSetup(cfg *config.Config, inst storage.Instance, reason string, destroyOnFail bool) {
	m.markFailed(inst, reason)
	if !destroyOnFail {
		return
	}
	m.AddLog(inst.ID, fmt.Sprintf("Auto-destroying instance (deploy failed: %s)", reason))
	if err := m.StopInstance(cfg, inst.ID); err != nil {
		log.Printf("[%s] Warning: auto-cleanup after failed deploy: %v", inst.ID, err)
	}
}

// cleanupDeletedInstance remove uma instância que o provedor confirmou como
// deletada: fecha túneis, tira do pool de ativos e apaga do banco. Usada
// tanto por deploys novos quanto por reconexões — evita loops infinitos de
// reconexão sobre instâncias que não existem mais.
func (m *InstanceManager) cleanupDeletedInstance(inst storage.Instance) {
	m.activesMu.Lock()
	if active, ok := m.actives[inst.ID]; ok {
		active.closeConn()
		delete(m.actives, inst.ID)
	}
	m.activesMu.Unlock()

	m.db.DeleteInstance(inst.ID)
	m.updateProxyTargets()
	log.Printf("[%s] Instance confirmed deleted at provider — local state cleaned up", inst.ID)
}

// stopLabelGuard é o prefixo de label que identifica instâncias gerenciadas
// pelo openllm. Instâncias sem esse prefixo NUNCA são destruídas (a conta
// pode ser compartilhada com outros projetos).
const stopLabelGuard = "openllm"

func (m *InstanceManager) StopInstance(cfg *config.Config, instanceID string) error {
	m.activesMu.Lock()
	active, exists := m.actives[instanceID]
	if exists {
		// Cancela os túneis e fecha conexão SSH
		active.closeConn()
		delete(m.actives, instanceID)
	}
	m.activesMu.Unlock()

	// Destrói no provedor — com trava de segurança: se a instância existe no
	// provedor e o label não é openllm*, recusa (protege projetos vizinhos).
	client, ok := providers.GetCompute(cfg.Provider)
	var err error
	if ok {
		if st, serr := client.GetStatus(context.Background(), instanceID, cfg.ActiveAPIKey()); serr == nil && st != nil {
			if st.Label != "" && !strings.HasPrefix(strings.ToLower(st.Label), stopLabelGuard) {
				m.AddLog(instanceID, fmt.Sprintf("SECURITY: refusing to destroy instance %s — label %q is not managed by openllm", instanceID, st.Label))
				log.Printf("[SECURITY] refusing to destroy instance %s (label %q)", instanceID, st.Label)
				return fmt.Errorf("instance %s is not managed by openllm (label %q)", instanceID, st.Label)
			}
		}
		err = client.Destroy(context.Background(), instanceID, cfg.ActiveAPIKey())
	} else {
		log.Printf("Warning: provider %s is not registered or not a compute provider", cfg.Provider)
	}
	if err != nil {
		log.Printf("Warning: failed to destroy instance %s on %s: %v", instanceID, cfg.Provider, err)
	}

	// Atualiza banco de dados: remove a instância, pois foi destruída
	m.db.DeleteInstance(instanceID)

	m.updateProxyTargets()
	return err
}

func (m *InstanceManager) PauseInstance(cfg *config.Config, instanceID string) error {
	m.activesMu.Lock()
	active, exists := m.actives[instanceID]
	if exists {
		// Cancela os túneis e fecha conexão SSH
		active.closeConn()
		delete(m.actives, instanceID)
	}
	m.activesMu.Unlock()

	client, ok := providers.GetCompute(cfg.Provider)
	if !ok {
		return fmt.Errorf("provider %s is not registered or not a compute provider", cfg.Provider)
	}

	err := client.Pause(context.Background(), instanceID, cfg.ActiveAPIKey())
	if err != nil {
		return err
	}

	inst, dbErr := m.db.GetInstance(instanceID)
	if dbErr == nil {
		inst.Status = "stopped"
		m.db.SaveInstance(inst)
	}
	return nil
}

func (m *InstanceManager) ResumeInstance(cfg *config.Config, instanceID string) error {
	client, ok := providers.GetCompute(cfg.Provider)
	if !ok {
		return fmt.Errorf("provider %s is not registered or not a compute provider", cfg.Provider)
	}

	err := client.Resume(context.Background(), instanceID, cfg.ActiveAPIKey())
	if err != nil {
		return err
	}

	inst, dbErr := m.db.GetInstance(instanceID)
	if dbErr != nil {
		return dbErr
	}

	inst.Status = "deploying"
	m.db.SaveInstance(inst)

	privKeyPath, _, err := ssh.EnsureKeysExist(cfg.ActiveAPIKey())
	if err != nil {
		return fmt.Errorf("failed to configure SSH keys: %w", err)
	}

	// Re-executa o fluxo de background que espera a máquina subir, reconecta o SSH e injeta o watchdog
	go m.backgroundSetup(cfg, *inst, privKeyPath, false)

	return nil
}

// ============================================================
// Escala horizontal (Fase 2)
// ============================================================

// ScaleResult descreve o plano de ação executado por ScaleGroup.
type ScaleResult struct {
	GroupID     string   `json:"group_id"`
	Engine      string   `json:"engine"`
	Model       string   `json:"model"`
	Current     int      `json:"current"`
	Created     []string `json:"created,omitempty"`
	Removed     []string `json:"removed,omitempty"`
	CostPerHour float64  `json:"cost_per_hour"`
}

// ScaleGroup ajusta o número de instâncias ativas de um modelo para `target`.
// Cria novas (buscando a oferta mais barata) ou destrói as mais caras.
func (m *InstanceManager) ScaleGroup(cfg *config.Config, engine, model string, target int, maxCostPerHour float64) (*ScaleResult, error) {
	if target < 1 {
		return nil, fmt.Errorf("target instances must be >= 1 (got %d)", target)
	}
	if engine == "" {
		engine = "ollama"
	}
	groupID := GroupID(engine, model)

	client, ok := providers.GetCompute(cfg.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %s is not a compute provider", cfg.Provider)
	}
	_ = client

	result := &ScaleResult{GroupID: groupID, Engine: engine, Model: model}

	// Garante que o grupo existe no banco (com rotas herdadas, se já existia)
	if g, _ := m.db.GetGroup(groupID); g == nil {
		m.db.SaveGroup(&storage.Group{ID: groupID, CreatedAt: time.Now()})
	}

	active, err := m.activeGroupInstances(groupID)
	if err != nil {
		return nil, err
	}
	result.Current = len(active)

	// --- Reduzir ---
	if len(active) > target {
		// Ordena por custo desc: remove as mais caras primeiro
		sort.Slice(active, func(i, j int) bool {
			return active[i].CostPerHour > active[j].CostPerHour
		})
		excess := len(active) - target
		for i := 0; i < excess; i++ {
			victim := active[i]
			if err := m.StopInstance(cfg, victim.ID); err != nil {
				log.Printf("[scale] warning: failed to stop %s: %v", victim.ID, err)
				continue
			}
			result.Removed = append(result.Removed, victim.ID)
		}
		result.Current = target
		return result, nil
	}

	// --- Aumentar ---
	toCreate := target - len(active)
	for i := 0; i < toCreate; i++ {
		m.AddLog("scale:"+groupID, fmt.Sprintf("Provisioning instance %d/%d for %s...", i+1, toCreate, groupID))

		machine, err := findBestOffer(cfg, engine, model, maxCostPerHour)
		if err != nil {
			return result, fmt.Errorf("failed finding offer %d/%d: %w", i+1, toCreate, err)
		}
		log.Printf("[scale] renting %s ($%.3f/h) for %s", machine.GPU, machine.CostPerHour, groupID)

		payload := DeployRequestPayload{
			MachineID: machine.ID,
			Model:     model,
			Engine:    engine,
		}
		instanceID, err := m.DeployInstance(cfg, payload)
		if err != nil {
			return result, fmt.Errorf("failed to deploy instance %d/%d: %w", i+1, toCreate, err)
		}
		result.Created = append(result.Created, instanceID)
	}

	result.Current = len(active) + len(result.Created)
	return result, nil
}

// activeGroupInstances lista instâncias ativas do grupo direto do banco.
func (m *InstanceManager) activeGroupInstances(groupID string) ([]storage.Instance, error) {
	insts, err := m.db.ListInstancesByGroup(groupID)
	if err != nil {
		return nil, err
	}
	var active []storage.Instance
	for _, inst := range insts {
		if inst.Status == "running" || inst.Status == "deploying" {
			active = append(active, inst)
		}
	}
	return active, nil
}

// ============================================================
// Stacks multimodais (Fase 3)
// ============================================================

// StackResult resume o deploy de um stack.
type StackResult struct {
	Name     string               `json:"name"`
	Services []StackServiceStatus `json:"services"`
}

type StackServiceStatus struct {
	Name      string   `json:"name"`
	GroupID   string   `json:"group_id"`
	Engine    string   `json:"engine"`
	Model     string   `json:"model"`
	Instances int      `json:"instances"`
	Routes    []string `json:"routes,omitempty"`
	Created   []string `json:"created,omitempty"`
}

// DeployStack sobe todos os serviços de um stack: registra os grupos (com
// rotas no proxy) e escala cada um para o número de réplicas desejado.
func (m *InstanceManager) DeployStack(cfg *config.Config, st *stack.Stack) (*StackResult, error) {
	result := &StackResult{Name: st.Name}

	for _, svc := range st.Services {
		engine := svc.Engine
		if engine == "" {
			// Resolve engine pelo catálogo (mesma lógica do CLI deploy)
			if entry, ok := models.ResolveCatalog(svc.Model); ok {
				engine = entry.Engine
			} else {
				engine = "ollama"
			}
		}
		instances := svc.Instances
		if instances <= 0 {
			instances = 1
		}

		groupID := GroupID(engine, svc.Model)
		m.db.SaveGroup(&storage.Group{
			ID:        groupID,
			Stack:     st.Name,
			Routes:    svc.Routes,
			CreatedAt: time.Now(),
		})

		svcStatus := StackServiceStatus{
			Name:      svc.Name,
			GroupID:   groupID,
			Engine:    engine,
			Model:     svc.Model,
			Instances: instances,
			Routes:    svc.Routes,
		}

		res, err := m.ScaleGroup(cfg, engine, svc.Model, instances, 0)
		if err != nil {
			return result, fmt.Errorf("service %q (%s): %w", svc.Name, groupID, err)
		}
		svcStatus.Created = res.Created
		result.Services = append(result.Services, svcStatus)
	}

	m.updateProxyTargets()
	return result, nil
}

// StopStack derruba todas as instâncias dos grupos pertencentes ao stack.
func (m *InstanceManager) StopStack(cfg *config.Config, name string) ([]string, error) {
	groups, err := m.db.ListGroups()
	if err != nil {
		return nil, err
	}

	var stopped []string
	for _, g := range groups {
		if g.Stack != name {
			continue
		}
		insts, err := m.db.ListInstancesByGroup(g.ID)
		if err != nil {
			continue
		}
		for _, inst := range insts {
			if inst.Status == "running" || inst.Status == "deploying" {
				if err := m.StopInstance(cfg, inst.ID); err == nil {
					stopped = append(stopped, inst.ID)
				}
			}
		}
		m.db.DeleteGroup(g.ID)
	}
	m.updateProxyTargets()
	return stopped, nil
}

// StackStatus lista os stacks com seus grupos e instâncias.
func (m *InstanceManager) StackStatus() ([]StackResult, error) {
	groups, err := m.db.ListGroups()
	if err != nil {
		return nil, err
	}

	stackMap := map[string]*StackResult{}
	var order []string
	for _, g := range groups {
		name := g.Stack
		if name == "" {
			continue // grupos sem stack aparecem apenas no status normal
		}
		if _, ok := stackMap[name]; !ok {
			stackMap[name] = &StackResult{Name: name}
			order = append(order, name)
		}
		insts, _ := m.db.ListInstancesByGroup(g.ID)
		active := 0
		cost := 0.0
		for _, inst := range insts {
			if inst.Status == "running" || inst.Status == "deploying" {
				active++
				cost += inst.CostPerHour
			}
		}
		parts := strings.SplitN(g.ID, "/", 2)
		stackMap[name].Services = append(stackMap[name].Services, StackServiceStatus{
			Name:      g.ID,
			GroupID:   g.ID,
			Engine:    parts[0],
			Model:     strings.Join(parts[1:], "/"),
			Instances: active,
			Routes:    g.Routes,
		})
	}

	var results []StackResult
	for _, name := range order {
		results = append(results, *stackMap[name])
	}
	return results, nil
}

// ============================================================
// Proxy targets
// ============================================================

func (m *InstanceManager) updateProxyTargets() {
	m.activesMu.RLock()
	defer m.activesMu.RUnlock()

	var targets []proxy.Target
	for _, act := range m.actives {
		if act.Status != "running" {
			continue
		}

		def := resolveEngineDef(act.Instance)
		target := proxy.Target{
			InstanceID: act.ID,
			Type:       proxy.TargetTypeLocal,
			LocalPort:  act.LocalPort,
			Model:      act.Model,
			Engine:     act.Engine,
			GroupID:    act.GroupID,
			HealthPath: def.HealthPath,
		}

		// Herda rotas registradas no grupo (stacks multimodais)
		if act.GroupID != "" {
			if g, err := m.db.GetGroup(act.GroupID); err == nil && g != nil {
				target.Routes = g.Routes
			}
		}

		targets = append(targets, target)
		for _, extra := range act.ExtraModels {
			// Extra só é roteado depois de confirmado no host.
			if act.ModelStates[extra] != "ready" {
				continue
			}
			t := target
			t.Model = extra
			targets = append(targets, t)
		}
	}
	// ReplaceLocalTargets preserva backends de inferência (OpenRouter etc.)
	m.proxyServer.ReplaceLocalTargets(targets)
}

func (m *InstanceManager) GetActiveInstances() []ActiveInstance {
	m.activesMu.RLock()
	defer m.activesMu.RUnlock()

	var list []ActiveInstance
	for _, act := range m.actives {
		c := *act
		c.ModelStates = act.ModelStatesCopy() // os mapas mudam em outras goroutines
		c.ModelProgress = copyMap(act.ModelProgress)
		c.ExtraModels = append([]string(nil), act.ExtraModels...)
		c.ModelDetails = copyMap(act.ModelDetails)
		list = append(list, c)
	}
	return list
}

func findWatchdogBinary() ([]byte, error) {
	// Procura o binário openllm-watchdog na pasta atual ou onde o daemon está
	exePath, err := os.Executable()
	if err != nil {
		exePath = "."
	}
	dir := filepath.Dir(exePath)

	paths := []string{
		filepath.Join(dir, "openllm-watchdog"),
		"./openllm-watchdog",
		"../openllm-watchdog",
		"./cmd/openllm-watchdog/openllm-watchdog",
	}

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return ioutil.ReadFile(p)
		}
	}

	return nil, fmt.Errorf("openllm-watchdog binary not found. Please build it first")
}

// ============================================================
// Swap de modelo em instância rodando (otimização: não re-renta a máquina)
// ============================================================

// renderEnv substitui {{.Model}} nos valores do Env da engine (ex: OLLAMA_MODEL).
func renderEnv(env map[string]string, model string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = engines.Render(v, model)
	}
	// Token do HuggingFace do ambiente do daemon: downloads autenticados são
	// mais rápidos e liberam modelos gated.
	if tok := os.Getenv("HF_TOKEN"); tok != "" {
		out["HF_TOKEN"] = tok
	}
	return out
}

func removeString(list []string, v string) []string {
	out := list[:0:0]
	for _, s := range list {
		if s != v {
			out = append(out, s)
		}
	}
	return out
}

// watchdogScript é o equivalente shell do cmd/openllm-watchdog: pinga o
// daemon pelo túnel reverso e destrói a instância na Vast se ficar sem
// resposta por mais que o timeout.
const watchdogScript = `#!/bin/sh
ID="%s"; KEY="%s"; URL="http://localhost:%d/ping?instance_id=%s"; TIMEOUT=%d
last=$(date +%%s)
while true; do
  sleep 30
  if curl -sf -m 10 -A openllm-watchdog/sh "$URL" >/dev/null; then
    last=$(date +%%s); echo "$(date -u +%%FT%%TZ) ping ok"
  else
    idle=$(( $(date +%%s) - last )); echo "$(date -u +%%FT%%TZ) ping failed (${idle}s/${TIMEOUT}s)"
    if [ "$idle" -ge "$TIMEOUT" ]; then
      echo "$(date -u +%%FT%%TZ) timeout: self-destruct $ID"
      curl -s -m 15 -X DELETE -H "Authorization: Bearer $KEY" "https://console.vast.ai/api/v0/instances/$ID/"
      exit 0
    fi
  fi
done
`

// installWatchdog instala o watchdog e devolve o comando que o inicia.
func (m *InstanceManager) installWatchdog(sshClient *ssh.SSHClient, inst storage.Instance, cfg *config.Config) (string, error) {
	const logRedirect = " > /var/log/openllm-watchdog.log 2>&1 &"
	if _, err := sshClient.RunCommand("command -v curl >/dev/null"); err == nil {
		script := fmt.Sprintf(watchdogScript, inst.ID, cfg.ActiveAPIKey(), cfg.HeartbeatPort, inst.ID, cfg.WatchdogTimeoutMinutes*60)
		encoded := base64.StdEncoding.EncodeToString([]byte(script))
		cmd := fmt.Sprintf("echo %s | base64 -d > /usr/local/bin/openllm-watchdog.sh && chmod 700 /usr/local/bin/openllm-watchdog.sh", encoded)
		if _, err := sshClient.RunCommand(cmd); err == nil {
			m.AddLog(inst.ID, "Watchdog installed (shell script)")
			return "nohup sh /usr/local/bin/openllm-watchdog.sh" + logRedirect, nil
		}
	}

	log.Printf("[%s] Host sem curl: transferindo binário openllm-watchdog...", inst.ID)
	watchdogBytes, err := findWatchdogBinary()
	if err != nil {
		return "", err
	}
	_, _ = sshClient.RunCommand("rm -f /usr/local/bin/openllm-watchdog || true")
	if err := sshClient.CopyFile("/usr/local/bin/openllm-watchdog", watchdogBytes, "0755"); err != nil {
		return "", err
	}
	return fmt.Sprintf("nohup /usr/local/bin/openllm-watchdog --instance-id %s --api-key %s --ping-url \"http://localhost:%d/ping?instance_id=%s\" --interval 30s --timeout %dm",
		inst.ID, cfg.ActiveAPIKey(), cfg.HeartbeatPort, inst.ID, cfg.WatchdogTimeoutMinutes) + logRedirect, nil
}
