# Engines externas (manifests JSON)

O openllm carrega automaticamente manifests de engine em:

- `./engines/*.json` (do diretório onde o CLI/daemon roda)
- `./.openllm/engines/*.json`

Isso permite subir **qualquer sistema** em uma máquina GPU sem recompilar o
binário: basta descrever a imagem docker, a porta, o comando de start e o
endpoint de health.

## Formato

```json
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
    "VAR_EXTRA": "valor"
  }
}
```

## Campos

| Campo | Obrigatório | Descrição |
|---|---|---|
| `name` | sim* | Nome da engine (default: nome do arquivo) |
| `modality` | não | Modalidades: `text`, `image`, `audio`, `video`, `tts`, `asr`, `embedding` |
| `remote_port` | sim | Porta que a engine escuta dentro da máquina remota |
| `default_vram` | sim | VRAM mínima padrão em GB |
| `docker_image` | sim | Imagem docker usada no aluguel do Vast.ai |
| `on_start_cmd` | sim | Comando inicial no boot do container (suporta `{{.Model}}`) |
| `health_path` | não | Path de health-check (default `/`) |
| `ready_timeout` | não | Minutos esperando health antes de seguir com aviso (default 15) |
| `install_cmd` | não | Comando SSH executado após o boot (suporta `{{.Model}}`) |
| `model_pull_cmd` | não | Comando de download do modelo em background (suporta `{{.Model}}`) |
| `model_vram` | não | Mapa modelo → VRAM (GB) |
| `env` | não | Variáveis de ambiente extras |

\* Se omitido, usa o nome do arquivo sem extensão.

## Uso

```bash
cp engines/f5-tts.json.example engines/f5-tts.json   # edite a imagem
openllm engines list                                  # aparece na lista
openllm deploy --model f5-tts-base --engine f5-tts
```
