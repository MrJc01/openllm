# Plano de Melhorias — openllm (crom-openllm-vastai)

> Objetivos: **(1)** subir qualquer IA facilmente (texto, imagem, áudio, vídeo, multimodal);
> **(2)** escalar horizontalmente máquinas rodando o mesmo modelo para ganhar velocidade;
> **(3)** suportar diferentes tipos de sistemas/engines sem alterar código-fonte;
> **(4)** facilitar tudo (um comando, zero configuração manual).

---

## 1. Análise do estado atual

### 1.1 O que já existe (pontos fortes)

| Componente | Arquivo | Situação |
|---|---|---|
| Registry de engines | `internal/engines/registry.go` | 4 engines hardcoded (ollama, localai, localai-image, vllm) + `GetCustom()` para imagem/cmd arbitrários |
| Provisioners | `internal/daemon/engines.go` | Mapa extensível `provisioners` (só ollama e localai implementados) |
| Provider abstraction | `internal/providers/provider.go` | Interfaces `ComputeProvider` / `InferenceProvider` — pronto para RunPod, Lambda, etc. |
| Proxy com LB | `internal/proxy/server.go` | Round-robin **por modelo**, suporta backends locais (túnel SSH) e remotos (OpenAI-compat) |
| Watchdog anti-custo | `cmd/openllm-watchdog` | Por instância, via túnel reverso — funciona para N instâncias (cada uma tem o próprio túnel) |
| Estado | `internal/storage/db.go` | SQLite com `instances` e `profiles`; migração best-effort de colunas |
| Daemon API | `internal/daemon/api.go` | `/deploy`, `/status`, `/stop`, `/pause`, `/resume`, `/logs`, `/ping` |

### 1.2 Gaps e bugs encontrados (impedem os objetivos)

| # | Problema | Local | Impacto |
|---|---|---|---|
| G1 | `GetCustom()` retorna `Name: "custom"`, mas `provisioners` não tem entry "custom" → cai no fallback **ollama**, que roda `ollama pull` em qualquer engine custom | `registry.go:97` + `engines.go:30-38` | Deploy de engine custom quebra/instala coisa errada |
| G2 | `updateProxyTargets()` só manda pro LB instâncias com engine `ollama` ou vazia | `instances.go:459` | vLLM/LocalAI/comfy nunca entram no balanceamento |
| G3 | `selectTarget()`: se o modelo não bate, faz fallback para `targets[0]` → requisição vai para **modelo errado** | `proxy/server.go:100-105` | Respostas erradas silenciosas |
| G4 | `UpdateTargets()` **substitui** a lista inteira → o deploy de um provider de inferência apaga os targets locais existentes | `proxy/server.go:60` + `api.go:240` | Deploy misto (local+remoto) perde backends |
| G5 | `cfg.InstancesCount` é passado como `GPUCount` no Search (confusão nº de instâncias × nº de GPUs) | `cli/deploy.go:113` | Busca filtra errado; não existe deploy múltiplo |
| G6 | CLI `deploy` não envia `capabilities/custom_image/custom_cmd` embora o daemon aceite | `cli/deploy.go:155-160` | Features inacessíveis via CLI |
| G7 | `Definition` não tem health-check nem comando de install/pull por engine — waits são `time.Sleep(5s)` fixo | `registry.go` + `instances.go:284` | "Running" ≠ pronto; proxy responde 502 nos primeiros minutos |
| G8 | Catálogo de modelos (`internal/models/models.json`) só cobre LLMs de texto (params/TPS) | `models/database.go` | Sem VRAM/throughput para imagem, áudio, vídeo |
| G9 | API key do Vast.ai commitada no `openllm.json` | `openllm.json` | Segredo exposto no histórico |
| G10 | Sem conceito de "grupo/deployment" no banco (só instâncias soltas) | `storage/db.go` | Impossível escalar "o mesmo modelo" como unidade |

---

## 2. Visão alvo (UX desejada)

```bash
# Texto — igual a hoje
openllm deploy --model llama3.3:70b -y

# Qualquer modalidade — o engine é escolhido automaticamente pelo catálogo
openllm deploy --model sdxl                    # imagem  → comfyui/localai
openllm deploy --model whisper-large-v3        # áudio   → faster-whisper
openllm deploy --model f5-tts                  # TTS     → f5-tts-server
openllm deploy --model ltx-video-2b            # vídeo   → comfyui
openllm deploy --model minha-coisa --image foo/bar:latest --port 8080  # arbitrário

# ESCALA: N máquinas rodando o MESMO modelo atrás do mesmo proxy
openllm scale --model deepseek-r1:7b --instances 3     # aluga 2 novas + mantém a atual
openllm scale --model deepseek-r1:7b --instances 1     # reduz (mata a mais cara/recente)
openllm status                                         # agrupa por modelo, custo total

# STACK multimodal (texto + imagem + vídeo em um endpoint só)
openllm stack up examples/multimodal.yaml
#   → aluga máquinas para cada serviço, roteia por path:
#   :11434/v1/chat/completions      → LLM
#   :11434/v1/images/generations    → SDXL
#   :11434/v1/audio/transcriptions  → Whisper
```

---

## 3. Fases de implementação

### Fase 0 — Correções de fundação (pré-requisito, ~1 sessão)

Antes de construir em cima, estabilizar o que existe:

- [ ] **F0.1** Corrigir **G1**: provisioner genérico default que usa `Definition.InstallCmd`/`ModelPullCmd` (ver F1.2) em vez de cair no ollama.
- [ ] **F0.2** Corrigir **G2**: mandar todas as instâncias `running` para o proxy (o roteamento por path da F2 resolve os protocolos diferentes).
- [ ] **F0.3** Corrigir **G3**: sem match de modelo → 404 com lista de modelos disponíveis (nunca `targets[0]`).
- [ ] **F0.4** Corrigir **G4**: `UpdateTargets` vira merge por `InstanceID` (adicionar/remover), não replace global.
- [ ] **F0.5** Corrigir **G6**: CLI envia `capabilities/custom_image/custom_cmd` no payload.
- [ ] **F0.6** **Segurança (G9)**: remover API key do `openllm.json` versionado; suportar env var `OPENLLM_VASTAI_KEY` (fallback: key no arquivo); adicionar `openllm.json`/`.openllm/` ao `.gitignore`; revogar a chave atual no painel do Vast.ai.
- [ ] **F0.7** Comitar a mudança pendente em `internal/engines/registry.go`.
- [ ] **F0.8** Health-check pós-start (base da F1.2): após `startEngine`, poll do endpoint de saúde até ready ou timeout (30 min, já é o timeout de startup da instância).

**Aceite:** deploy ollama funciona igual a hoje; deploy vllm entra no proxy; modelo desconhecido não cai em backend errado; `git status` limpo; nenhuma chave no repositório.

---

### Fase 1 — Subir qualquer IA: catálogo + engines declarativas (núcleo do objetivo 1)

**Ideia central:** hoje adicionar uma engine exige Go em 2 lugares (`registry.go` + `engines.go`). Passa a ser **só dados**.

- [ ] **F1.1 — `Definition` v2** (`internal/engines/registry.go`):

  ```go
  type Definition struct {
      Name          string
      Modality      []string            // "text","image","audio","video","tts","asr","embedding"
      RemotePort    int
      DefaultVRAM   float64
      ModelVRAM     map[string]float64
      DockerImage   string
      OnStartCmd    string
      // novos campos:
      HealthPath    string   // ex: "/api/tags" (ollama), "/health" (localai), "/" (comfy)
      ReadyTimeout  int      // minutos
      InstallCmd    string   // template executado via SSH antes do start
      ModelPullCmd  string   // template com {{.Model}} — download do modelo
      Env           map[string]string
  }
  ```

- [ ] **F1.2 — Provisioner genérico** (`internal/daemon/engines.go`): um único provisioner movido pelos campos da `Definition` (InstallCmd → ModelPullCmd → poll HealthPath até ReadyTimeout). Remove os provisioners hardcoded (setup/start ollama/localai) — comportamento preservado via campos.
- [ ] **F1.3 — Catálogo de modelos v2** (`internal/models/models.json` → novo `catalog.json`):

  ```json
  { "sdxl":            { "engine": "comfyui",  "modality": "image", "vram_gb": 12,
                         "pull": "comfy model download sdxl", "metric": "it/s", "metric_target": 1.0 } }
  ```

  Função nova `catalog.Resolve(modelName) → (engine, vram, pullCmd, modality)`; `GetCustom` mantido como fallback. `openllm deploy --model X` sem `--engine` resolve automaticamente pelo catálogo (heurística atual de capabilities fica como fallback).
- [ ] **F1.4 — Engines manifest externos**: daemon (e CLI) carregam `./engines/*.json` (e `.openllm/engines/*.json`) chamando `engines.Register()` — usuário adiciona engine **sem recompilar**. Formato = o JSON da `Definition`.
- [ ] **F1.5 — Novas engines no registry builtin** (uma Definition cada, zero código extra):

  | Engine | Modality | Imagem docker | Porta |
  |---|---|---|---|
  | `comfyui` | image, video | `comfy:latest` (ou `yanwk/comfyui-boot`) | 8188 |
  | `faster-whisper` | asr | `fedirz/faster-whisper-server` | 8000 |
  | `f5-tts` | tts | imagem própria crom ou pip install | 8080 |
  | `vllm` (ajuste) | text | corrigir start cmd p/ receber `{{.Model}}` do HF hub | 8000 |

- [ ] **F1.6 — CLI**: `openllm models list` (tabela: modelo, engine, modalidade, VRAM); `deploy --model X` resolve engine do catálogo; `--vram` override.
- [ ] **F1.7 — Search por modalidade**: `SearchRequest` ganha campo `Metric` ("tps"/"itps"/"rtf"); estimativa de TPS só se aplica a texto; imagem/áudio/vídeo filtram só por VRAM (comportamento já existe para localai — generalizar).

**Aceite:** `openllm deploy --model sdxl -y` aluga máquina, sobe ComfyUI, e a UI fica acessível no proxy; adicionar uma engine nova = escrever 1 JSON.

---

### Fase 2 — Escala horizontal: várias máquinas, mesmo modelo (objetivo 2)

**Ideia central:** LB já existe (`selectTarget` faz round-robin por modelo). Falta orquestrar N instâncias do mesmo modelo e um LB mais inteligente.

- [ ] **F2.1 — Schema**: tabela `instances` ganha `group_id TEXT` (migração best-effort igual à da coluna `engine`). `group_id = engine + "/" + model`.
- [ ] **F2.2 — Daemon `POST /scale`**: payload `{model, engine, instances, vram?, max_cost_hour?}`:
  - conta instâncias `running|deploying` do grupo;
  - se faltam: para cada uma → busca oferta (reusa lógica de busca do CLI, movida para o daemon) → `DeployInstance()` em loop paralelo (limit 3 concurrent);
  - se sobram: destrói as mais caras mais recentes (nunca a última);
  - retorna plano de ação + instâncias criadas.
- [ ] **F2.3 — CLI `openllm scale`**: chama `/scale`, mostra progresso por instância (poll `/status` + `/logs`), imprime custo/hora total do grupo.
- [ ] **F2.4 — LB v2** (`internal/proxy/server.go`):
  - health-check em background por target (poll `HealthPath` a cada 15s; target sick sai do pool, volta quando responde 2× seguidas);
  - estratégia: round-robin (default) + `least-connections` (contador atômico de requisições em voo por target);
  - `handleTags` em paralelo (goroutines) com timeout de 2s por target.
- [ ] **F2.5 — `openllm status`**: agrupa por modelo mostrando `N instâncias · $X/h total · req/s no proxy (opcional)`; `--watch` já existe.
- [ ] **F2.6 — Watchdog**: já funciona por instância (túnel reverso próprio) — validar com 3+ instâncias.
- [ ] **F2.7 — (Opcional, fase 2b) Autoscaler**: loop no daemon que observa concorrência no proxy e chama `/scale` entre `min_instances` e `max_instances` do grupo, com orçamento teto.

**Aceite:** `openllm scale --model deepseek-r1:7b --instances 3` deixa 3 máquinas rodando; `curl :11434/api/chat` distribui entre elas; matar o daemon mata todas via watchdog (testar).

---

### Fase 3 — Stacks multimodais: texto + imagem + vídeo num endpoint (objetivos 1+3)

**Ideia central:** o proxy vira um **roteador por path**, cada rota aponta para um grupo (modelo). Uma "stack" é um YAML que descreve vários serviços.

- [ ] **F3.1 — Config de stack** (`openllm.yaml` — novo comando, não mexe no `openllm.json`):

  ```yaml
  name: multimodal
  services:
    - name: llm
      engine: ollama
      model: deepseek-r1:7b
      instances: 1
      routes: ["/v1/chat/completions", "/api/chat", "/api/generate"]
    - name: image
      engine: comfyui
      model: sdxl
      instances: 1
      routes: ["/v1/images/generations"]
    - name: video
      engine: comfyui
      model: ltx-video-2b
      instances: 1
      routes: ["/v1/videos/generations"]
    - name: speech
      engine: faster-whisper
      model: whisper-large-v3
      instances: 1
      routes: ["/v1/audio/transcriptions", "/v1/audio/speech"]
  ```

- [ ] **F3.2 — Proxy rotas**: `Target` ganha `Routes []string`; matching longest-prefix antes do match por model; sem rota definida → comportamento atual (por model). Corrigir **G4** aqui (merge).
- [ ] **F3.3 — Daemon `POST /stack`**: deploy em sequência por dependência (nenhuma, na prática — todas paralelas, cap 3), registra rotas; `DELETE /stack/{name}` derruba tudo do grupo.
- [ ] **F3.4 — CLI**: `openllm stack up <yaml>`, `openllm stack down`, `openllm stack status`; `examples/multimodal.yaml` pronto.
- [ ] **F3.5 — `openllm stacks list` no catálogo**: stacks prontas embutidas (texto+imagem, texto+imagem+vídeo, RAG+áudio) que só geram o YAML no disco para o usuário editar.

**Aceite:** `openllm stack up examples/multimodal.yaml` sobe 4 serviços em máquinas separadas; um único `http://localhost:11434` atende chat, imagem e áudio.

---

### Fase 4 — Testes e confiabilidade (contínuo, começa junto com a Fase 0)

- [ ] **F4.1 — Unit tests**: `proxy.selectTarget` (por modelo, fallback, LB strategies), `config` migração, `catalog.Resolve`, `engines.GetCustom` corrigido, `FindFreePort`. `make test` já existe no Makefile.
- [ ] **F4.2 — Mock de ComputeProvider** (`internal/providers/mock/`): `Search` retorna ofertas fake; `Deploy` retorna instância fake — permite testar o fluxo completo do daemon (deploy → tunnels → status) **sem gastar dinheiro no Vast.ai**.
- [ ] **F4.3 — Teste de integração**: `go test ./internal/daemon -tags=integration` subindo proxy + 3 backends HTTP fake + balanceamento/health-check.
- [ ] **F4.4 — Testes manuais de campo** (com dinheiro real, 1 instância por vez): ollama texto, sdxl imagem, whisper áudio, ltx vídeo; derrubar daemon e confirmar auto-destruição; scale 3× e medir throughput agregado.
- [ ] **F4.5 — CI simples**: GitHub Actions rodando `go vet ./... && go test ./...` (Go 1.25) em cada push.

**Aceite:** `make test` cobre seleção de backend e catálogo; teste E2E com mock provider passa sem rede.

---

## 4. Ordem e dependências

```
F0 (correções) ──► F1 (catálogo/engines) ──► F3 (stacks)
       │                    │
       └──► F4 (testes, contínuo)      F2 (scale) — depende de F0, paralelizável com F1
```

| Fase | Esforço estimado | Risco |
|---|---|---|
| F0 | 1 sessão | baixo |
| F1 | 2–3 sessões | médio (catalogar VRAM de modelos de imagem/vídeo) |
| F2 | 2 sessões | médio (LB + health checks concorrentes) |
| F3 | 1–2 sessões | baixo (é composição das fases 1–2) |
| F4 | contínuo | — |

## 5. Riscos e mitigações

1. **VRAM estimada errada para modelos difusão/TTS** → campos `vram_gb` no catálogo conservadores + override `--vram` + logs do Vast.ai no `/logs`.
2. **Muitas instâncias = custo alto** → `max_cost_hour` no scale + watchdog já auto-destrói; alerta no `status` quando custo/hora > orçamento.
3. **Protocolos diferentes (ComfyUI ≠ Ollama ≠ OpenAI)** → proxy roteia por path e cada grupo expõe o protocolo nativo; apps que só falam OpenAI podem usar LocalAI/vLLM como fachada (padrão já usado hoje).
4. **Túneis com múltiplas instâncias** → cada instância já tem conexão SSH e porta local própria (`FindFreePort`); nada muda, só validar volume.
5. **Registros de instância duplicados após restart do daemon** → `SaveInstance` usa `INSERT OR REPLACE` por ID; group_id sobrevive a restarts.