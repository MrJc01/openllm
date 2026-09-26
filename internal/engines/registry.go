// Package engines is the single source of truth for what generation engines
// openllm knows how to rent a GPU for: their name, the port they listen on
// remotely, and how much VRAM each of their models needs.
//
// It has no dependency on SSH/daemon internals on purpose — both the CLI
// (which only needs port/VRAM to search for a matching GPU offer) and the
// daemon (which additionally needs to know *how* to install/start each
// engine, see internal/daemon/engines.go) import this package.
//
// To add a new engine: add one Definition below, drop a JSON manifest in
// ./engines/ (or .openllm/engines/), or call Register() from an init().
package engines

import (
	"strings"
)

// Definition describes one generation engine (Ollama, LocalAI, ComfyUI...).
type Definition struct {
	Name string `json:"name"`

	// Modality lists what this engine serves: "text", "image", "audio",
	// "video", "tts", "asr", "embedding".
	Modality []string `json:"modality,omitempty"`

	// RemotePort is the port the engine listens on inside the rented machine.
	RemotePort int `json:"remote_port"`

	// DefaultVRAM is used when a specific model isn't listed in ModelVRAM.
	DefaultVRAM float64 `json:"default_vram"`

	// ModelVRAM maps a model/checkpoint name to its minimum recommended VRAM (GB).
	ModelVRAM map[string]float64 `json:"model_vram,omitempty"`

	// DockerImage is the native pre-built image used when renting GPU instances.
	DockerImage string `json:"docker_image"`

	// OnStartCmd is the initial command executed inside the container on boot.
	// Supports the {{.Model}} placeholder, substituted at deploy time.
	OnStartCmd string `json:"on_start_cmd"`

	// HealthPath is the HTTP path polled (from inside the remote host) to
	// decide the engine is ready to serve. Empty defaults to "/".
	HealthPath string `json:"health_path"`

	// ReadyTimeout is how many minutes to wait for the engine health endpoint
	// before proceeding anyway (with a warning). 0 = 15 minutes.
	ReadyTimeout int `json:"ready_timeout"`

	// InstallCmd is an optional command executed over SSH right after the
	// container boots (before OnStartCmd/health waits). Supports {{.Model}}.
	InstallCmd string `json:"install_cmd"`

	// ModelPullCmd is an optional command executed over SSH to download the
	// model in background (e.g. "ollama pull {{.Model}}"). Supports {{.Model}}.
	// Executado fire-and-forget (nohup) — canais SSH não sobrevivem a
	// downloads longos.
	ModelPullCmd string `json:"model_pull_cmd"`

	// ModelReadyCmd é o comando SSH que retorna exit 0 quando o modelo está
	// disponível (ex: "ollama list | grep -q {{.Model}}"). Suporta {{.Model}}.
	ModelReadyCmd string `json:"model_ready_cmd"`

	// WarmupCmd (opcional) roda via SSH logo após o modelo ficar pronto e
	// carrega os pesos na GPU. Sem ele, a 1ª requisição do usuário paga
	// ~20-40s de inicialização do runner. Suporta {{.Model}}.
	WarmupCmd string `json:"warmup_cmd,omitempty"`

	// Env são variáveis de ambiente extras definidas no container (ex: HF_TOKEN,
	// PROVISIONING_SCRIPT). Repassadas ao provedor via DeployRequest.Env.
	Env map[string]string `json:"env,omitempty"`

	// DiskGB é o tamanho de disco (GB) solicitado ao provedor para a instância.
	// 0 = usa o default do daemon (35GB).
	DiskGB float64 `json:"disk_gb,omitempty"`

	// MinCUDA é a versão mínima de CUDA que o driver do host precisa suportar
	// para a DockerImage (ex: 13.0 para tags -cuda-13.0). 0 = sem restrição.
	MinCUDA float64 `json:"min_cuda,omitempty"`
}

var registry = map[string]Definition{
	"ollama": {
		Name:        "ollama",
		Modality:    []string{"text", "embedding"},
		RemotePort:  11434,
		DefaultVRAM: 8,
		// Imagem/tag do template oficial Vast "Ollama" (maior chance de cache
		// no host). O entrypoint sobe o ollama em 127.0.0.1:11434 e já inicia
		// o pull de OLLAMA_MODEL no boot, antes do SSH ficar disponível.
		DockerImage: "vastai/ollama:0.34.0",
		OnStartCmd:  "entrypoint.sh",
		Env: map[string]string{
			"OLLAMA_MODEL":   "{{.Model}}",
			"DATA_DIRECTORY": "/workspace/",
			// Imagens vastai/* só sobem os serviços listados em PORTAL_CONFIG
			// (/etc/portal.yaml); sem ele o supervisor pula o serviço.
			"PORTAL_CONFIG": "localhost:21434:11434:/:Ollama API",
		},
		HealthPath:    "/api/tags",
		ReadyTimeout:  15,
		ModelPullCmd:  "ollama pull {{.Model}}",
		ModelReadyCmd: "ollama list | grep -q {{.Model}}",
		// Gera 1 token (não só carrega): o 1º forward pass também paga a
		// inicialização de kernels CUDA. Modelos de embedding não aceitam
		// generate, então cai para /api/embed.
		WarmupCmd: `curl -sf -m 300 http://127.0.0.1:11434/api/generate -d '{"model":"{{.Model}}","prompt":"hi","stream":false,"keep_alive":"30m","options":{"num_predict":1}}' >/dev/null || ` +
			`curl -sf -m 300 http://127.0.0.1:11434/api/embed -d '{"model":"{{.Model}}","input":"hi","keep_alive":"30m"}' >/dev/null`,
	},
	"localai": {
		Name:         "localai",
		Modality:     []string{"text", "image", "audio"},
		RemotePort:   8080,
		DefaultVRAM:  16,
		DockerImage:  "localai/localai:latest-gpu-nvidia-cuda-12",
		OnStartCmd:   "mkdir -p /root/backends /root/models; /local-ai backends install stablediffusion-ggml 2>&1 || true; LOCALAI_ADDRESS=0.0.0.0:8080 /local-ai --models-path /root/models &",
		HealthPath:   "/readyz", // LocalAI expõe /healthz e /readyz; /health não existe
		ReadyTimeout: 20,
		// /health responde sem nenhum modelo: sem estes dois comandos o LocalAI
		// "subia" vazio e toda requisição dava `model "X" not found`. O nome
		// do modelo é o da galeria (ex: sd-1.5-ggml), instalado no servidor
		// em execução via API (POST /models/apply, id "<galeria>@<modelo>").
		// O pull é disparado antes do servidor terminar de subir: espera o
		// /readyz (até ~20 min) antes de pedir o modelo, senão o curl falha
		// em silêncio e o modelo nunca é instalado.
		ModelPullCmd: `for i in $(seq 1 400); do curl -sf http://127.0.0.1:8080/readyz >/dev/null && break; sleep 3; done; ` +
			`curl -sf -X POST http://127.0.0.1:8080/models/apply -H 'Content-Type: application/json' -d '{"id":"localai@{{.Model}}"}'`,
		ModelReadyCmd: `curl -sf http://127.0.0.1:8080/v1/models | grep -q '"{{.Model}}"'`,
		ModelVRAM: map[string]float64{
			"z-image-turbo-diffusers": 16,
			"sd-1.5-ggml":             8,
			"ace-step-turbo":          8,
			"ltx-2":                   24,
		},
	},
	"localai-image": {
		Name:         "localai-image",
		Modality:     []string{"image"},
		RemotePort:   8080,
		DefaultVRAM:  8,
		DockerImage:  "localai/localai:latest-gpu-nvidia-cuda-12",
		OnStartCmd:   "mkdir -p /root/backends /root/models; /local-ai backends install stablediffusion-ggml 2>&1 || true; LOCALAI_ADDRESS=0.0.0.0:8080 /local-ai --models-path /root/models &",
		HealthPath:   "/readyz", // LocalAI expõe /healthz e /readyz; /health não existe
		ReadyTimeout: 20,
		ModelVRAM: map[string]float64{
			"sd-1.5": 8,
		},
	},
	"vllm": {
		Name:        "vllm",
		Modality:    []string{"text", "embedding"},
		RemotePort:  18000,
		DefaultVRAM: 16,
		// Tag pinada (nunca :latest flutuante): deploys reproduzíveis e
		// cache de layers reaproveitável entre deploys no mesmo host.
		// Template oficial Vast "vLLM": o entrypoint sobe `vllm serve` com
		// VLLM_MODEL/VLLM_ARGS (porta interna 18000; a 8000 é o proxy com auth).
		DockerImage: "vastai/vllm:v0.30.0-cuda-13.0",
		MinCUDA:     13.0,
		OnStartCmd:  "entrypoint.sh",
		Env: map[string]string{
			"VLLM_MODEL":     "{{.Model}}",
			"VLLM_ARGS":      "--max-model-len 8192 --download-dir /workspace/models --host 127.0.0.1 --port 18000",
			"AUTO_PARALLEL":  "true",
			"RAY_ADDRESS":    "127.0.0.1",
			"RAY_ARGS":       "--head --port 6379 --dashboard-host 127.0.0.1 --dashboard-port 28265",
			"DATA_DIRECTORY": "/workspace/",
			// Imagens vastai/* só sobem os serviços listados em PORTAL_CONFIG
			// (/etc/portal.yaml); sem ele o supervisor pula o serviço.
			"PORTAL_CONFIG": "localhost:8000:18000:/docs:vLLM API",
		},
		HealthPath:   "/v1/models",
		ReadyTimeout: 30, // vLLM compila o grafo na primeira subida
		ModelVRAM: map[string]float64{
			"Qwen/Qwen2.5-Coder-7B-Instruct":    16,
			"meta-llama/Llama-3.3-70B-Instruct": 40,
		},
		DiskGB: 50, // 70B models: ~40GB VRAM + model weights on disk
	},
	"comfyui": {
		Name:        "comfyui",
		Modality:    []string{"image", "video"},
		RemotePort:  18188,
		DefaultVRAM: 12,
		// cu126-megapak: a variante FULL (Debian com apt — as cu130/slim são
		// apt-less e o entrypoint do vast.ai não consegue instalar sshd nelas,
		// o SSH nunca sobe). Compatível com Ada/Ampere (sm_86/sm_89).
		// Template oficial Vast "ComfyUI": porta interna 18188 (8188 é o proxy
		// com auth); o provisioning baixa o checkpoint do workflow SDXL Turbo.
		DockerImage: "vastai/comfy:v0.37.0-cuda-13.2-py312",
		MinCUDA:     13.2,
		OnStartCmd:  "entrypoint.sh",
		Env: map[string]string{
			"COMFYUI_ARGS":     "--disable-auto-launch --disable-xformers --port 18188 --enable-cors-header",
			"COMFYUI_API_BASE": "http://localhost:18188",
			// Sem PROVISIONING_*: o provisioning baixava ~7GB do HF antes de
			// subir o ComfyUI (2h num host com HF lento). A imagem já traz o
			// SD 1.5 em /opt/model_store, então a 1ª imagem não baixa nada.
			"DATA_DIRECTORY": "/workspace/",
			// Imagens vastai/* só sobem os serviços listados em PORTAL_CONFIG
			// (/etc/portal.yaml); sem ele o supervisor pula o serviço.
			"PORTAL_CONFIG": "localhost:8188:18188:/:ComfyUI",
		},
		HealthPath: "/",
		// Modelos com "files" no catálogo têm download e "pronto" gerados pelo
		// daemon (daemon/modelfiles.go). Aqui fica só o SD 1.5, que já vem na
		// imagem. "true" mantém swap/+modelo habilitados para o ComfyUI.
		ModelPullCmd: "true",
		ModelReadyCmd: `case "{{.Model}}" in ` +
			`sd-1.5|sd15|stable-diffusion-1.5) curl -sf http://127.0.0.1:18188/models/checkpoints | grep -q v1-5-pruned-emaonly;; ` +
			// Modelo sem regra nem files: nunca "pronto".
			`*) exit 1;; esac`,
		ReadyTimeout: 20,
		ModelVRAM: map[string]float64{
			"sd-1.5":        4,
			"sdxl":          12,
			"flux1-dev":     24,
			"ltx-video-2b":  12,
			"ltx-video-13b": 24,
			"wan2.1-1.3b":   8,
		},
		DiskGB: 120, // vários modelos de 10–30 GB na mesma máquina
	},
	// Kokoro-82M (TTS leve, Apache 2.0, voz PT pf_dora) com API OpenAI
	// (/v1/audio/speech). Na Vast o CMD da imagem não roda: o onstart chama o
	// entrypoint.sh com o ambiente da imagem explícito.
	"kokoro": {
		Name:        "kokoro",
		Modality:    []string{"tts", "audio"},
		RemotePort:  8880,
		DefaultVRAM: 4,
		DockerImage: "ghcr.io/remsky/kokoro-fastapi-gpu:latest",
		OnStartCmd: "cd /app && PATH=/app/.venv/bin:$PATH PYTHONPATH=/app:/app/api USE_GPU=true DEVICE=gpu " +
			"PHONEMIZER_ESPEAK_PATH=/usr/bin PHONEMIZER_ESPEAK_DATA=/usr/share/espeak-ng-data ESPEAK_DATA_PATH=/usr/share/espeak-ng-data " +
			"nohup ./entrypoint.sh > /var/log/kokoro.log 2>&1 &",
		HealthPath:   "/docs",
		ReadyTimeout: 15,
		// Pronto = sintetiza de verdade (o modelo carrega na 1ª chamada).
		ModelReadyCmd: `curl -sf -m 120 -X POST http://127.0.0.1:8880/v1/audio/speech -H 'Content-Type: application/json' ` +
			`-d '{"model":"kokoro","input":"ok","voice":"af_heart"}' -o /dev/null`,
		DiskGB: 30,
	},
	// Speaches (ex faster-whisper-server): transcrição com API OpenAI
	// (/v1/audio/transcriptions). Os modelos são baixados por POST /v1/models/<id HF>.
	"speaches": {
		Name:        "speaches",
		Modality:    []string{"asr", "audio"},
		RemotePort:  8000,
		DefaultVRAM: 6,
		DockerImage: "ghcr.io/speaches-ai/speaches:latest-cuda",
		OnStartCmd: "cd /home/ubuntu/speaches && HOME=/home/ubuntu PATH=/home/ubuntu/speaches/.venv/bin:$PATH " +
			"nohup uvicorn --factory speaches.main:create_app --host 0.0.0.0 --port 8000 > /var/log/speaches.log 2>&1 &",
		HealthPath:   "/health",
		ReadyTimeout: 15,
		ModelPullCmd: `for i in $(seq 1 200); do curl -sf http://127.0.0.1:8000/health >/dev/null && break; sleep 3; done; ` +
			`curl -sf -X POST "http://127.0.0.1:8000/v1/models/{{.ServeAs}}"`,
		// Pronto = transcreve 1s de silêncio (o speaches só baixa/carrega o
		// modelo na 1ª transcrição; listar em /v1/models não basta).
		ModelReadyCmd: `[ -f /tmp/openllm-silence.wav ] || /home/ubuntu/speaches/.venv/bin/python -c "import wave;w=wave.open('/tmp/openllm-silence.wav','wb');w.setnchannels(1);w.setsampwidth(2);w.setframerate(16000);w.writeframes(bytes(32000))"; ` +
			`curl -sf -m 900 http://127.0.0.1:8000/v1/audio/transcriptions -F file=@/tmp/openllm-silence.wav -F 'model={{.ServeAs}}' >/dev/null`,
		DiskGB:        30,
	},
	"faster-whisper": {
		Name:        "faster-whisper",
		Modality:    []string{"asr", "audio"},
		RemotePort:  8000,
		DefaultVRAM: 6,
		DockerImage: "fedirz/faster-whisper-server:latest-cuda",
		// O app é uv-based e usa factory (create_app): "uvicorn ...:app" puro
		// falha ("Attribute app not found"). O modelo HF segue o padrão
		// Systran/faster-<model> (whisper-small -> Systran/faster-whisper-small).
		OnStartCmd:   "cd /root/faster-whisper-server && FASTER_WHISPER_MODEL=Systran/faster-{{.Model}} uv run python -c \"from faster_whisper_server.main import create_app; import uvicorn; uvicorn.run(create_app(), host='0.0.0.0', port=8000)\" &",
		HealthPath:   "/health",
		ReadyTimeout: 15,
		ModelVRAM: map[string]float64{
			"whisper-large-v3": 10,
			"whisper-medium":   6,
			"whisper-small":    3,
		},
	},
	"xtts": {
		Name:        "xtts",
		Modality:    []string{"tts", "audio"},
		RemotePort:  8000,
		DefaultVRAM: 6,
		// A tag `latest-cuda` não existe (manifest unknown); as válidas são
		// latest-cuda118 / latest-cuda121. O código do servidor fica em /app.
		DockerImage:  "ghcr.io/coqui-ai/xtts-streaming-server:latest-cuda121",
		OnStartCmd:   "cd /app && COQUI_TOS_AGREED=1 python -m uvicorn main:app --host 0.0.0.0 --port 8000 &",
		HealthPath:   "/",
		ReadyTimeout: 15,
	},
}

// Register adds or overrides an engine definition. Used by external JSON
// manifests (internal/engines/external.go) and future engine packages that
// register themselves from an init().
func Register(def Definition) {
	if def.Name == "" {
		return
	}
	registry[def.Name] = def
}

// Get returns the definition for a known engine, or a fallback.
func Get(name string) Definition {
	if def, ok := registry[name]; ok {
		return def
	}
	return registry["ollama"]
}

// TryGet retorna a Definition se a engine existir (sem fallback).
func TryGet(name string) (Definition, bool) {
	def, ok := registry[name]
	return def, ok
}

// Exists reports whether an engine with the given name is registered.
func Exists(name string) bool {
	_, ok := registry[name]
	return ok
}

// GetCustom returns an engine definition dynamically computed from capabilities or custom inputs.
func GetCustom(name string, capabilities []string, customImage string, customCmd string, customPort int) Definition {
	if customImage != "" {
		port := customPort
		if port <= 0 {
			port = 8080
		}
		return Definition{
			Name:         "custom",
			RemotePort:   port,
			DefaultVRAM:  12,
			DockerImage:  customImage,
			OnStartCmd:   customCmd,
			HealthPath:   "/",
			ReadyTimeout: 20,
		}
	}

	// Modular capability resolution
	hasImage := false
	hasText := false
	hasAudio := false
	hasVideo := false
	for _, cap := range capabilities {
		switch cap {
		case "image":
			hasImage = true
		case "text":
			hasText = true
		case "audio":
			hasAudio = true
		case "video":
			hasVideo = true
		}
	}

	// Multimodal (texto + mídia) → LocalAI full
	if hasText && (hasImage || hasAudio || hasVideo) {
		return Get("localai")
	}

	// Só imagem/vídeo → imagem light
	if hasImage || hasVideo {
		return Get("localai-image")
	}

	// Só áudio → faster-whisper por padrão
	if hasAudio {
		return Get("faster-whisper")
	}

	// Só texto → ollama
	if hasText {
		return Get("ollama")
	}

	return Get(name)
}

// Render substitui o placeholder {{.Model}} (e a variante {{model}}) por um
// ComfyModelsDir: pasta models/ do ComfyUI QUE ESTÁ RODANDO (cwd do processo).
// A vastai/comfy tem duas instalações (/opt/workspace-internal e a cópia em
// /workspace); baixar na errada deixa o modelo invisível para o ComfyUI.
const ComfyModelsDir = `for i in $(seq 1 200); do P=$(pgrep -f 'main[.]py.*--port 18188' | head -1); [ -n "$P" ] && break; sleep 3; done; ` +
	`M=$(readlink /proc/$P/cwd)/models; `

// nome de modelo em comandos (OnStartCmd, InstallCmd, ModelPullCmd).
func Render(tpl string, model string) string {
	if model == "" {
		model = "facebook/opt-125m"
	}
	return strings.NewReplacer("{{.Model}}", model, "{{model}}", model).Replace(tpl)
}

// RemotePort returns the port the given engine listens on remotely, falling
// back to `defaultOllamaPort` (the configurable value from openllm.json)
// when the engine is Ollama or unknown.
func RemotePort(name string, defaultOllamaPort int) int {
	if name == "" || name == "ollama" {
		return defaultOllamaPort
	}
	return Get(name).RemotePort
}

// VRAMFor returns the minimum recommended VRAM (GB) for a given engine+model.
func VRAMFor(engineName string, modelName string) float64 {
	def := Get(engineName)
	if v, ok := def.ModelVRAM[modelName]; ok {
		return v
	}
	return def.DefaultVRAM
}

// List returns all known engine names.
func List() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	return names
}
