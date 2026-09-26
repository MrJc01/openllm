package daemon

import (
	"fmt"
	"log"
	"regexp"
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
		background := backgroundCmd(full)
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
	m.setModelState(inst.ID, targetModel, "loading")

	deadline := time.Now().Add(20 * time.Minute)
	attempt := 0
	for time.Now().Before(deadline) {
		if _, err := sshClient.RunCommand(full); err == nil {
			m.AddLog(inst.ID, fmt.Sprintf("Model %s is ready and available", targetModel))
			m.warmupModel(sshClient, inst, def, targetModel)
			m.setModelState(inst.ID, targetModel, "ready")
			return nil
		}
		if attempt%5 == 0 { // ~15s: progresso real lido do log do pull no host
			raw, _ := sshClient.RunCommand(pullProgressCmd)
			msg := summarizePullProgress(raw)
			if msg == "" {
				msg = "aguardando início do download"
			}
			m.setModelProgress(inst.ID, targetModel, msg)
			m.AddLog(inst.ID, fmt.Sprintf("Baixando %s: %s", targetModel, msg))
		}
		attempt++
		time.Sleep(3 * time.Second)
	}
	m.setModelState(inst.ID, targetModel, "failed")
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
// warmupModel carrega o modelo na GPU antes de declarar a instância pronta.
// Falha não é fatal: o modelo carrega na 1ª requisição, só mais devagar.
func (m *InstanceManager) warmupModel(sshClient *ssh.SSHClient, inst storage.Instance, def engines.Definition, model string) {
	cmd := strings.TrimSpace(def.WarmupCmd)
	if cmd == "" {
		return
	}
	start := time.Now()
	if _, err := sshClient.RunCommand(engines.Render(cmd, model)); err != nil {
		m.AddLog(inst.ID, fmt.Sprintf("Warmup of %s failed (first request will be slower): %v", model, err))
		return
	}
	m.AddLog(inst.ID, fmt.Sprintf("Model %s warmed up on GPU in %s", model, time.Since(start).Round(100*time.Millisecond)))
}

// backgroundCmd roda cmd em background no host remoto, com log em arquivo.
// Usa aspas simples: com %q (aspas duplas) o shell externo expandia $(...)
// e $VAR antes da hora e quebrava comandos como `for i in $(seq 1 400)`.
func backgroundCmd(cmd string) string {
	return "nohup sh -c " + shellQuote(cmd) + " > /var/log/openllm-model-pull.log 2>&1 &"
}

// shellQuote envolve s em aspas simples, escapando as aspas simples internas.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// pullProgressCmd lê o fim do log do pull (curl/ollama reescrevem a linha com
// \r) e o tamanho dos arquivos .part em andamento.
const pullProgressCmd = `tail -c 4000 /var/log/openllm-model-pull.log 2>/dev/null | tr '\r' '\n' | grep -v '^[[:space:]]*$' | tail -3; ` +
	`echo '--parts--'; find / -xdev -name '*.part' -size +1M -mmin -2 2>/dev/null | head -5 | xargs -r du -m --apparent-size 2>/dev/null`

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	// Linha de progresso do curl: % Total % Received % Xferd Avg-Dl Avg-Up Total Spent Left Speed
	curlRe = regexp.MustCompile(`^\s*(\d{1,3})\s+(\S+)\s+\d{1,3}\s+(\S+)\s+\S+\s+\S+\s+(\S+)\s+\S+\s+\S+\s+\S+\s+(\S+)\s+(\S+)\s*$`)
	pctRe  = regexp.MustCompile(`(\d{1,3})%`)
)

// summarizePullProgress transforma a saída de pullProgressCmd numa linha legível:
// "45% · 2763M de 6046M · 17.9M/s · falta 0:03:16" (curl) ou a última linha do
// log (ollama/outros), mais os arquivos .part em andamento.
func summarizePullProgress(raw string) string {
	logPart, parts, _ := strings.Cut(ansiRe.ReplaceAllString(raw, ""), "--parts--")
	lines := strings.Split(strings.TrimSpace(logPart), "\n")
	var out string
	for i := len(lines) - 1; i >= 0 && out == ""; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.HasPrefix(l, "% Total") || strings.HasPrefix(l, "Dload") {
			continue
		}
		if m := curlRe.FindStringSubmatch(l); m != nil {
			out = fmt.Sprintf("%s%% · %s de %s · %s/s · falta %s", m[1], m[3], m[2], m[6], m[5])
			if m[1] == "100" {
				out = fmt.Sprintf("100%% · %s concluído", m[2])
			}
		} else if pctRe.MatchString(l) || strings.Contains(l, "pulling") || strings.Contains(l, "success") {
			if len(l) > 160 {
				l = l[:160]
			}
			out = l
		}
	}
	var files []string
	for _, f := range strings.Split(strings.TrimSpace(parts), "\n") {
		if mb, path, ok := strings.Cut(strings.TrimSpace(f), "\t"); ok {
			name := path[strings.LastIndex(path, "/")+1:]
			files = append(files, fmt.Sprintf("%s (%s MB)", strings.TrimSuffix(name, ".part"), mb))
		}
	}
	if len(files) > 0 {
		if out != "" {
			out += " · "
		}
		out += "arquivos: " + strings.Join(files, ", ")
	}
	return out
}
