package engines

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
)

// LoadExternal carrega manifests de engine em JSON de um ou mais diretórios
// e os registra via Register(). Isso permite adicionar engines novas (imagem
// docker, portas, comandos de instalação, VRAM) sem recompilar o openllm.
//
// Ordem de resolução do nome do arquivo: <basename>.json → nome da engine.
// Arquivos com erro de parse são reportados em log mas não interrompem o boot.
func LoadExternal(dirs ...string) []string {
	loaded := []string{}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // diretório inexistente é silencioso
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			def, err := LoadFile(path)
			if err != nil {
				fmt.Printf("[engines] warning: skipping manifest %s: %v\n", path, err)
				continue
			}
			if def.Name == "" {
				def.Name = strings.TrimSuffix(filepath.Base(e.Name()), ".json")
			}
			Register(def)
			loaded = append(loaded, def.Name)
		}
	}
	return loaded
}

// LoadFile lê e valida um único manifest de engine.
func LoadFile(path string) (Definition, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return Definition{}, err
	}
	var def Definition
	if err := json.Unmarshal(data, &def); err != nil {
		return Definition{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if def.RemotePort <= 0 {
		return Definition{}, fmt.Errorf("remote_port is required (> 0)")
	}
	if def.DockerImage == "" {
		return Definition{}, fmt.Errorf("docker_image is required")
	}
	if def.HealthPath == "" {
		def.HealthPath = "/"
	}
	if def.ReadyTimeout <= 0 {
		def.ReadyTimeout = 15
	}
	return def, nil
}