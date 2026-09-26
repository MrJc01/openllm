package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/crom-org/openllm/internal/config"
	"github.com/crom-org/openllm/internal/daemon"
	"github.com/crom-org/openllm/internal/engines"
	_ "github.com/crom-org/openllm/internal/providers/mock"
	_ "github.com/crom-org/openllm/internal/providers/openrouter"
	_ "github.com/crom-org/openllm/internal/providers/vastai"
)

func main() {
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
