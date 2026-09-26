package daemon

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/crom-org/openllm/internal/config"
	"github.com/crom-org/openllm/internal/engines"
	"github.com/crom-org/openllm/internal/ssh"
	"github.com/crom-org/openllm/internal/storage"
)

// O provisioner agora é totalmente genérico: em vez de uma função Go por
// engine, o fluxo de instalação/start/readiness é dirigido pelos campos da
// engines.Definition (InstallCmd, ModelPullCmd, HealthPath, ReadyTimeout).
// Adicionar uma engine nova exige apenas uma Definition (builtin ou JSON em
// ./engines/), sem tocar neste arquivo.

// setupEngine executa o comando de instalação da engine na máquina remota
// (via SSH), se a Definition tiver InstallCmd.
func (m *InstanceManager) setupEngine(sshClient *ssh.SSHClient, inst storage.Instance, def engines.Definition) error {
	if strings.TrimSpace(def.InstallCmd) == "" {
		return nil
	}
	cmd := engines.Render(def.InstallCmd, inst.Model)
	m.AddLog(inst.ID, fmt.Sprintf("Running engine install: %s", cmd))
	output, err := sshClient.RunCommand(cmd)
	if err != nil {
		log.Printf("[%s] Engine install warning (proceeding): %v. Output: %s", inst.ID, err, output)
	}
	return nil
}

// startEngine inicia o serviço (já iniciado pelo onstart no boot do container)
// e dispara o download do modelo em background, sem travar o deploy.
//
// O pull roda fire-and-forget no host remoto (nohup + log em arquivo):
// canais SSH interativos morrem em downloads longos e não há como reanexá-los.
func (m *InstanceManager) startEngine(sshClient *ssh.SSHClient, inst storage.Instance, def engines.Definition) error {
	pullCmd := def.ModelPullCmd
	if strings.TrimSpace(pullCmd) == "" {
		return nil
	}

	go func() {
		full := engines.Render(pullCmd, inst.Model)
		background := fmt.Sprintf("nohup sh -c %q > /var/log/openllm-model-pull.log 2>&1 &", full)
		if _, err := sshClient.RunCommand(background); err != nil {
			log.Printf("[%s] Failed to launch background model pull: %v", inst.ID, err)
			m.AddLog(inst.ID, fmt.Sprintf("Model pull launch error: %v", err))
			return
		}
		log.Printf("[%s] Model pull launched in background: %s", inst.ID, full)
		m.AddLog(inst.ID, fmt.Sprintf("Model pull launched in background (log: /var/log/openllm-model-pull.log)"))
	}()

	return nil
}

// waitForModelReady faz polling (via SSH) de um comando de disponibilidade do
// modelo até retornar exit 0, ou expirar o timeout. Falha de timeout não
// aborta o deploy — só avisa (o modelo pode ser grande, mas o túnel já está vivo).
// waitForModelReady espera o MODELO INFORMADO ficar disponível (não inst.Model —
// durante um swap o modelo alvo é diferente do atual da instância).
func (m *InstanceManager) waitForModelReady(sshClient *ssh.SSHClient, inst storage.Instance, def engines.Definition, targetModel string) error {
	readyCmd := strings.TrimSpace(def.ModelReadyCmd)
	if readyCmd == "" {
		return nil
	}
	full := engines.Render(readyCmd, targetModel)

	deadline := time.Now().Add(20 * time.Minute)
	attempt := 0
	for time.Now().Before(deadline) {
		if _, err := sshClient.RunCommand(full); err == nil {
			m.AddLog(inst.ID, fmt.Sprintf("Model %s is ready and available", targetModel))
			return nil
		}
		if attempt%6 == 0 { // loga a cada ~1 min (interval 10s)
			m.AddLog(inst.ID, fmt.Sprintf("Waiting for model %s to download...", targetModel))
		}
		attempt++
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("model %s not ready within 20 minutes (check /var/log/openllm-model-pull.log on the host)", targetModel)
}

// waitForEngineReady faz polling do endpoint de saúde da engine dentro da
// máquina remota (via SSH + curl) até responder 2xx, ou expirar o timeout.
// Retorna erro apenas se o timeout estourar — o chamador decide se falha ou
// segue com aviso.
func (m *InstanceManager) waitForEngineReady(sshClient *ssh.SSHClient, inst storage.Instance, def engines.Definition, port int) error {
	healthPath := def.HealthPath
	if healthPath == "" {
		healthPath = "/"
	}
	timeoutMin := def.ReadyTimeout
	if timeoutMin <= 0 {
		timeoutMin = 15
	}

	healthURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, healthPath)
	checkCmd := fmt.Sprintf(
		`curl -s -o /dev/null -w "%%{http_code}" --max-time 5 %s 2>/dev/null || echo 000`,
		healthURL,
	)

	deadline := time.Now().Add(time.Duration(timeoutMin) * time.Minute)
	attempt := 0
	for time.Now().Before(deadline) {
		output, err := sshClient.RunCommand(checkCmd)
		code := strings.TrimSpace(output)
		if err == nil && strings.HasPrefix(code, "2") {
			m.AddLog(inst.ID, fmt.Sprintf("Engine %s is healthy (health check %s OK)", def.Name, healthURL))
			return nil
		}
		if attempt%6 == 0 { // loga a cada ~30s (interval 5s)
			m.AddLog(inst.ID, fmt.Sprintf("Waiting for engine %s to become ready (%s)...", def.Name, healthURL))
		}
		attempt++
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("engine %s did not report ready within %d minutes (health: %s)", def.Name, timeoutMin, healthURL)
}

// remotePortForEngine retorna a porta em que a engine escuta na máquina remota.
func remotePortForEngine(cfg *config.Config, engine string) int {
	return engines.RemotePort(engine, cfg.RemotePort)
}

// effectiveRemotePort prioriza a porta da Definition (persistida no deploy).
// Definition de engine builtin sempre tem RemotePort; o fallback cobre defs
// antigas sem porta, resolvendo pelo registry/nome como antes.
func effectiveRemotePort(def engines.Definition, cfg *config.Config, engine string) int {
	if def.RemotePort > 0 {
		return def.RemotePort
	}
	return remotePortForEngine(cfg, engine)
}