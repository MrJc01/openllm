package daemon

import (
	"fmt"
	"strings"

	"github.com/crom-org/openllm/internal/engines"
	"github.com/crom-org/openllm/internal/models"
)

// modelCmds devolve os comandos de download e de "pronto" de um modelo.
// ComfyUI com Files no catálogo: gerados a partir da lista (qualquer modelo
// novo é só uma entrada no catálogo). Senão: os comandos da engine.
func modelCmds(def engines.Definition, model string) (pull, ready string) {
	if e, ok := models.ResolveCatalog(model); ok && len(e.Files) > 0 && def.Name == "comfyui" {
		return comfyFilesPull(e.Files), comfyFilesReady(e.Files)
	}
	// {{.ServeAs}}: id do modelo no servidor (ex: speaches usa o id do HF).
	serveAs := model
	if e, ok := models.ResolveCatalog(model); ok && e.ServeAs != "" {
		serveAs = e.ServeAs
	}
	r := strings.NewReplacer("{{.ServeAs}}", serveAs)
	return r.Replace(engines.Render(def.ModelPullCmd, model)), r.Replace(engines.Render(def.ModelReadyCmd, model))
}

// comfyFilesPull baixa todos os arquivos em paralelo na pasta do ComfyUI em
// execução (.part → mv, para o "pronto" nunca ver arquivo pela metade).
func comfyFilesPull(files []models.ModelFile) string {
	var b strings.Builder
	b.WriteString(". /etc/environment 2>/dev/null; ")
	b.WriteString(engines.ComfyModelsDir)
	b.WriteString(`get() { [ -f "$M/$1" ] && return; mkdir -p "$(dirname "$M/$1")"; ` +
		`curl -fL --retry 5 ${HF_TOKEN:+-H "Authorization: Bearer $HF_TOKEN"} -o "$M/$1.part" "$2" && mv "$M/$1.part" "$M/$1"; }; `)
	for _, f := range files {
		fmt.Fprintf(&b, "get %s %s & ", shellQuote(f.Dir+"/"+f.Name()), shellQuote(f.URL))
	}
	b.WriteString("wait")
	return b.String()
}

// comfyFilesReady: pronto quando o ComfyUI lista todos os arquivos.
func comfyFilesReady(files []models.ModelFile) string {
	parts := make([]string, 0, len(files))
	for _, f := range files {
		parts = append(parts, fmt.Sprintf("curl -sf http://127.0.0.1:18188/models/%s | grep -qF %s", f.Dir, shellQuote(f.Name())))
	}
	return strings.Join(parts, " && ")
}

// modelFileNames: arquivos do modelo no catálogo (filtra o progresso dos .part).
func modelFileNames(model string) []string {
	e, ok := models.ResolveCatalog(model)
	if !ok {
		return nil
	}
	names := make([]string, len(e.Files))
	for i, f := range e.Files {
		names[i] = f.Name()
	}
	return names
}
