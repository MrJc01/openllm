package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/crom-org/openllm/internal/config"
	"github.com/crom-org/openllm/internal/daemon"
	"github.com/crom-org/openllm/internal/engines"
	_ "github.com/crom-org/openllm/internal/providers/mock"
	_ "github.com/crom-org/openllm/internal/providers/openrouter"
	_ "github.com/crom-org/openllm/internal/providers/vastai"
)

// loadDotEnv lê KEY=VALUE de um .env (segredos fora do git, ex:
// OPENLLM_VASTAI_API_KEY, HF_TOKEN). Não sobrescreve variáveis já definidas.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
}

func main() {
	loadDotEnv(".env")
	// Registro permanente: tudo que o daemon loga vai também para
	// .openllm/logs/openllmd-AAAAMMDD.log (terminal continua recebendo).
	if dir, err := config.GetStateDir(); err == nil {
		logDir := filepath.Join(dir, "logs")
		if err := os.MkdirAll(logDir, 0755); err == nil {
			name := filepath.Join(logDir, "openllmd-"+time.Now().Format("20060102")+".log")
			if f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
				log.SetOutput(io.MultiWriter(os.Stderr, f))
			}
		}
	}
	log.Println("Starting openllmd daemon...")

	// Engines externas: manifests JSON em ./engines/ e .openllm/engines/
	loaded := engines.LoadExternal("engines", ".openllm/engines")
	if len(loaded) > 0 {
		log.Printf("Loaded %d external engine manifest(s): %v", len(loaded), loaded)
	}

	// Inicialização automática do ambiente local
	cfg, created, err := config.AutoInitialize()
	if err != nil {
		log.Fatalf("Error during auto-initialization: %v", err)
	}

	if created {
		fmt.Println("----------------------------------------------------------------------")
		fmt.Printf("Initialized data folder .openllm/ and created template '%s'\n", config.ConfigFileName)
		fmt.Println("Please configure your API Key in 'openllm.json' and run again.")
		fmt.Println("----------------------------------------------------------------------")
		os.Exit(0)
	}

	// Validação básica do arquivo de configuração
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Configuration validation error: %v\nPlease edit 'openllm.json' with valid values.", err)
	}

	d, err := daemon.NewDaemon(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize daemon: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Captura de sinais do sistema para shutdown gracioso
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		log.Printf("Received signal %v. Initiating graceful shutdown...", sig)
		cancel()
	}()

	if err := d.Start(ctx); err != nil {
		log.Fatalf("Daemon stopped with error: %v", err)
	}

	log.Println("Daemon stopped successfully.")
}
