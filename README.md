# openllm — GPUs alugadas por centavos, qualquer IA, um endpoint

[![CI](https://github.com/MrJc01/crom-openllm-vastai/actions/workflows/ci.yml/badge.svg)](https://github.com/MrJc01/crom-openllm-vastai/actions/workflows/ci.yml)

CLI + daemon em **Go** que orquestra GPUs alugadas (Vast.ai e provedores similares) para rodar **qualquer modelo de IA** — texto, imagem, áudio (transcrição/TTS), vídeo e música — expondo tudo num **endpoint local com load balancer**, com **watchdog de auto-destruição** para nunca queimar dinheiro.

> **Testado em produção real**: chat (Llama/Qwen), swap de modelo sem re-aluguel, transcrição de áudio, geração de imagem (ComfyUI + SD1.5), escala horizontal com LB e watchdog auto-destroy — tudo contra contas e GPUs reais do Vast.ai.

---

## 🎯 Por quê?

| | Nuvem de API (OpenRouter/Runway/Suno) | **openllm (GPU alugada)** |
|---|---|---|
| Privacidade | prompts/imagens/áudio vão para a nuvem | **só sua máquina** (túnel SSH, zero portas públicas) |
| Custo em volume | $0,10–1,25 por clipe/música/imagem | **$0,0003–0,10 por geração** |
| Throttle | filas, caps de download | = número de GPUs que você alugar |
| Modelos | só o catálogo deles | **qualquer coisa** (engines em JSON, sem recompilar) |
| Controle | nenhum | seeds, paralelismo, contextos, fine-tune |

Referências verificadas (ago/2026): Replicate Wan2.1-14B 720p **$1,25/clipe 5s** vs nosso ~$0,09; Suno **$0,012/música** (cap de 20 downloads/mês) vs nosso ACE-Step **~$0,002**; OpenRouter **não oferece vídeo**.

---

## 🛠️ Arquitetura

```
┌────────────────────────────────────────────────────────────┐
│                      openllm CLI                           │
│   search · deploy · scale · swap · stack · stop · status   │
└────────────────────┬───────────────────────────────────────┘
                     │ HTTP API (127.0.0.1:17290)
┌────────────────────▼───────────────────────────────────────┐
│                    openllmd Daemon                         │
│                                                            │
│  ┌──────────────────────────────────────────────────────┐  │
│  │  Proxy + Load Balancer (:21434)                      │  │
│  │  round-robin / least-connections · health-check      │  │
│  │  roteamento por path (texto/img/áudio/vídeo)         │  │
│  └──────────────────────────────────────────────────────┘  │
│  ┌────────────┐ ┌───────────┐ ┌──────────┐ ┌────────────┐  │
│  │ SSH Manager│ │ Vast.ai   │ │ SQLite   │ │ Catálogo   │  │
│  │ (ed25519)  │ │ Provider  │ │ State    │ │ + Engines  │  │
│  └────────────┘ └───────────┘ └──────────┘ └────────────┘  │
└────────────────────────────────────────────────────────────┘
        │ SSH: túnel direto (engine) + túnel reverso (heartbeat)
┌───────▼────────────────────────────────────────────────────┐
│                 Máquina alugada (Vast.ai)                  │
│  ┌──────────────────────────┐ ┌─────────────────────────┐  │
│  │ Engine (ollama, vLLM,    │ │ openllm-watchdog        │  │
│  │ ComfyUI, whisper, você)  │ │ auto-destroy sem daemon │  │
│  └──────────────────────────┘ └─────────────────────────┘  │
└────────────────────────────────────────────────────────────┘
```

**Ciclo de vida com segurança financeira:**
1. **Pesquisa ao vivo** — `openllm search` estima VRAM/TPS por modelo e lista ofertas; a seleção desempata **por banda de rede** dentro de +20% do preço (pull de imagem é o gargalo real).
2. **Deploy com chave ed25519** auto-gerada e registrada no provedor (RSA foi aposentado — hosts novos recusam).
3. **Watchdog**: pings via túnel reverso a cada 30s; sem heartbeat por `watchdog_timeout_minutes`, a máquina **se auto-destrói** (validado 3× em produção).
4. **Anti-vazamento**: deploy novo que falha (SSH, pull, timeout) destrói a instância automaticamente — inclusive após crash/restart do daemon (`deploy_timeout_minutes` configurável, padrão 30).
5. **Guarda de label**: instâncias só são destruídas se o label começa com `openllm-` — nunca toca em contratos de terceiros na mesma conta.

---

## ⚡ Quick Start

```bash
git clone https://github.com/MrJc01/crom-openllm-vastai.git
cd crom-openllm-vastai && make build

mkdir meu-workspace && cd meu-workspace
cp ../openllm ./ ../openllmd .   # ou os binários de release
./openllm deploy                  # cria openllm.json na 1ª execução
# edite openllm.json → providers.vastai.api_key  (ou export OPENLLM_VASTAI_API_KEY)

(nohup setsid ./openllmd > daemon.log 2>&1 &)
./openllm deploy --model llama3.2:3b -y

# consuma via OpenAI-compatible proxy:
curl http://localhost:21434/api/chat -d '{"model":"llama3.2:3b","messages":[{"role":"user","content":"oi"}]}'
```

## 🧩 Um modelo de cada tipo

| Tipo | Deploy | Endpoint |
|---|---|---|
| **Texto (chat)** | `openllm deploy --model qwen3:8b -y` | `POST /api/chat` (estilo Ollama) |
| **Imagem** | `openllm deploy --model sdxl -y` (ComfyUI/LocalAI) | `POST /prompt` (estilo ComfyUI) |
| **Áudio ASR** | `openllm deploy --engine faster-whisper --model whisper-small -y` | `POST /v1/audio/transcriptions` (OpenAI-style) |
| **TTS** | `openllm deploy --engine xtts -y` | endpoint TTS da engine |
| **Vídeo** | `openllm deploy --engine comfyui --model ltx-video-2b -y` | `POST /prompt` (workflow LTX) |
| **Música** | engine externa (`engines/*.json`) — ex. ACE-Step | HTTP da engine |
| **Tudo junto** | `openllm stack up examples/multimodal.yaml` | um endpoint, rotas por path |

O catálogo (`internal/models/catalog.json`) resolve **engine e VRAM automaticamente** pelo nome do modelo; qualquer modelo não listado cai no Ollama.

## 🔧 Tudo é modificável (sem recompilar)

1. **Catálogo** (`internal/models/catalog.json`): modelo → engine, modalidade, VRAM, aliases.
2. **Engines externas**: solte um JSON em `./engines/` ou `.openllm/engines/` com docker image, comando, porta e health-check:

```bash
cp engines/f5-tts.json.example engines/f5-tts.json   # edite
openllm engines list
openllm deploy --model f5-tts-base --engine f5-tts
```

3. **Engine custom ad-hoc** (qualquer imagem docker, testado com ComfyUI + SD1.5):

```bash
openllm deploy --custom-image pytorch/pytorch:2.3.1-cuda12.1-cudnn8-runtime \
  --custom-port 8188 \
  --custom-cmd "instale/suba o que quiser; python main.py --listen 0.0.0.0 --port 8188" -y
```

4. **`openllm.json`**: porta do proxy, timeout de deploy, watchdog, alvo de TPS, nº de instâncias.

## 🚀 VRAM que sobra = mais velocidade

Um modelo de 3B usa ~4GB de uma GPU de 12GB. O resto **não fica parado**:

- **Paralelismo**: aumente `OLLAMA_NUM_PARALLEL` (via `--custom-cmd` ou env da engine) — mais requisições simultâneas no mesmo container = mais throughput no LB.
- **Contextos maiores**: mais VRAM = janelas de contexto/KV cache maiores.
- **Workers sidecar**: o proxy aceita N targets — réplicas do mesmo modelo ou serviços distintos (ex.: ollama + TTS) convivendo na mesma máquina, cada um na sua porta/rota.

## 📖 Comandos

```bash
openllm search --vram 8                     # ofertas ao vivo com banda de rede
openllm deploy --model llama3.3:70b -y      # texto
openllm scale --model qwen2.5:0.5b --instances 2   # escala horizontal + LB
openllm scale --model qwen2.5:0.5b --instances 1   # reduz (destroi a mais cara)
openllm swap --instance <ID> --model qwen2.5:0.5b  # troca modelo SEM re-alugar
openllm stack up examples/multimodal.yaml   # texto+imagem+áudio+vídeo num endpoint
openllm stack status / stack down <nome>
openllm models list / openllm engines list  # catálogo
openllm status --watch                      # instâncias + heartbeats ao vivo
openllm stop --instance <ID> / stop --all   # libera recursos (para já!)
openllm profile save/load/list              # perfis reutilizáveis
```

## 💡 Como funciona o swap (economia real)

`openllm swap` baixa o novo modelo na máquina **que já está alugada**, mantendo o antigo servindo durante o download, e troca o LB quando o novo fica pronto. Sem destruir/re-alugar/re-puxar imagem: validamos troca `llama3.2:3b → qwen2.5:0.5b` em ~80s contra deploy completo de ~15 min.

## 🔐 Segurança

- Túneis SSH only — **nenhuma porta pública** exposta na GPU.
- Chave **ed25519** auto-gerada por workspace, re-registro idempotente no provedor (self-healing).
- API key: `export OPENLLM_VASTAI_API_KEY=...` (env > `openllm.json`); `prod-test/` e `openllm.json` estão no `.gitignore`.
- Guarda de label impede destruir instâncias de outros projetos na mesma conta.

## 🧪 Testes & CI

```bash
go test -race ./...   # 60+ testes unitários
```

Cobrindo: ciclo de deploy/destroy com provider mock, scale up/down, LB e concorrência do proxy, catálogo/engines, stacks, anti-vazamento (resume pós-crash destrói `deploying`), keygen ed25519, seleção por banda, portas de engine custom. CI roda lint + testes em cada push (`.github/workflows/ci.yml`).

## 📄 Licença

Fair-Code: uso pessoal e não comercial gratuito. Uso comercial/corporativo: contate a organização **crom-**.
