package main

import (
	"github.com/crom-org/openllm/internal/cli"
	"github.com/crom-org/openllm/internal/engines"
	_ "github.com/crom-org/openllm/internal/providers/mock"
	_ "github.com/crom-org/openllm/internal/providers/openrouter"
	_ "github.com/crom-org/openllm/internal/providers/vastai"
)

func main() {
	// Engines externas (mesmos diretórios do daemon) para o CLI conhecer as
	// engines customizadas na hora do deploy/search.
	engines.LoadExternal("engines", ".openllm/engines")
	cli.Execute()
}
