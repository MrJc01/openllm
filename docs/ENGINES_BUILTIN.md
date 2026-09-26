# Engines Builtin — Referência Rápida

| Engine | Modalidades | Imagem Docker | Porta | Health Path | VRAM Default | DiskGB | InstallCmd | ModelPullCmd | ModelReadyCmd |
|---|---|---|---|---|---|---|---|---|---|
| **ollama** | text, embedding | `ollama/ollama:latest` | 11434 | `/api/tags` | 8 | 35 | — | `ollama pull {{.Model}}` | `ollama list \| grep -q {{.Model}}` |
| **vllm** | text, embedding | `vllm/vllm-openai:v0.30.0` | 8000 | `/v1/models` | 16 | 50 | — | — (modelo no start) | — |
| **localai** | text, image, audio | `localai/localai:latest-gpu-nvidia-cuda-12` | 8080 | `/health` | 16 | 35 | instala backends | — | — |
| **localai-image** | image | `localai/localai:latest-gpu-nvidia-cuda-12` | 8080 | `/health` | 8 | 35 | instala backends | — | — |
| **comfyui** | image, video | `yanwk/comfyui-boot:cu126-megapak` | 8188 | `/` | 12 | 50 | — | — | — |
| **faster-whisper** | asr, audio | `fedirz/faster-whisper-server:latest-cuda` | 8000 | `/health` | 6 | 35 | — | HF auto-download | — |
| **xtts** | tts, audio | `ghcr.io/coqui-ai/xtts-streaming-server:latest-cuda` | 8000 | `/` | 6 | 35 | — | — | — |

## Notas por Engine

### ollama
- Modelo passado via `ollama pull {{.Model}}` em background (nohup)
- Health check polling `/api/tags` até 15 min
- Suporta swap de modelo via `ollama pull` + `ollama list`

### vllm
- **Imagem pinada em `v0.30.0`** (não `:latest`) para deploys reproduzíveis
- Modelo passado no `--model` do `on_start_cmd` — vLLM baixa no primeiro start
- `ReadyTimeout: 30` (compilação de grafo na primeira subida)
- DiskGB: 50 (modelos 70B pesam ~40GB em disco)

### comfyui
- **Preso a `cu126-megapak`**: variantes `cu130/slim` são apt-less e o entrypoint vast.ai não instala sshd nelas → SSH nunca sobe
- Compatível Ada/Ampere (sm_86/sm_89)
- Modelos baixados via workflow/ComfyUI Manager, não via CLI
- DiskGB: 50 (flux1-dev/ltx-video-13b são grandes)

### faster-whisper
- App uv-based com factory `create_app` — `uvicorn ...:app` puro falha
- Modelo HF segue padrão `Systran/faster-<model>` (ex.: whisper-small → `Systran/faster-whisper-small`)
- Auto-download no start (sem ModelPullCmd)

### xtts
- Streaming TTS server
- Modelo xtts-v2 ~6GB VRAM

## Adicionar Engine Nova (sem recompilar)

```bash
# 1. Crie JSON em ./engines/ ou ~/.openllm/engines/
cat > engines/minha-engine.json <<'EOF'
{
  "name": "minha-engine",
  "modality": ["text", "image"],
  "remote_port": 8080,
  "default_vram": 12,
  "docker_image": "minha-imagem:latest",
  "on_start_cmd": "minha-engine --model {{.Model}} &",
  "health_path": "/health",
  "ready_timeout": 15,
  "install_cmd": "apt-get update && apt-get install -y algo",
  "model_pull_cmd": "minha-engine pull {{.Model}}",
  "model_vram": {
    "modelo-grande": 24
  },
  "env": {
    "HF_TOKEN": ""
  },
  "disk_gb": 30
}
EOF

# 2. Aparece no CLI automaticamente
openllm engines list
# minha-engine  text,image  8080  12GB  minha-imagem:latest

# 3. Deploy
openllm deploy --model meu-modelo --engine minha-engine
```

## Campos do Manifest (JSON)

| Campo | Tipo | Obrigatório | Descrição |
|---|---|---|---|
| `name` | string | sim* | Nome da engine (default: nome do arquivo) |
| `modality` | string[] | não | `text`, `image`, `audio`, `video`, `tts`, `asr`, `embedding` |
| `remote_port` | int | sim | Porta dentro da máquina remota |
| `default_vram` | float | sim | VRAM mínima padrão (GB) |
| `docker_image` | string | sim | Imagem docker no Vast.ai |
| `on_start_cmd` | string | sim | Comando de boot (suporta `{{.Model}}`) |
| `health_path` | string | não | Path de health (default `/`) |
| `ready_timeout` | int | não | Minutos para health (default 15) |
| `install_cmd` | string | não | Comando SSH pós-boot (suporta `{{.Model}}`) |
| `model_pull_cmd` | string | não | Download modelo em background (suporta `{{.Model}}`) |
| `model_ready_cmd` | string | não | SSH que retorna 0 quando modelo pronto (suporta `{{.Model}}`) |
| `model_vram` | object | não | Mapa `modelo → VRAM GB` |
| `env` | object | não | Variáveis de ambiente extras |
| `disk_gb` | float | não | Disco GB pedido ao Vast.ai (0 = 35 default) |

*Se omitido, usa nome do arquivo sem extensão.

## Resolução Automática de Engine (Catálogo)

`openllm deploy --model X` sem `--engine` resolve automaticamente:

| Modelo | Engine Resolvida | Fonte |
|---|---|---|
| `deepseek-r1:7b` | `ollama` | `catalog.json` |
| `sdxl` | `comfyui` | `catalog.json` |
| `whisper-large-v3` | `faster-whisper` | `catalog.json` |
| `xtts-v2` | `xtts` | `catalog.json` |
| `Qwen/Qwen2.5-Coder-7B-Instruct` | `vllm` | `catalog.json` |
| Modelo desconhecido + `--capabilities text` | `ollama` | Heurística `GetCustom` |
| Modelo desconhecido + `--capabilities image` | `localai-image` | Heurística `GetCustom` |
| Modelo desconhecido + `--capabilities audio` | `faster-whisper` | Heurística `GetCustom` |

## Variáveis de Ambiente Úteis

- `HF_TOKEN` — token Hugging Face para modelos privados/gated (passado via `env` no manifest ou builtin)
- `PROVISIONING_SCRIPT` — script de provisionamento no Vast.ai (quando usado via template, não via PUT /asks)