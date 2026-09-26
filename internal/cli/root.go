package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "openllm",
	Short: "openllm is a CLI tool to orchestrate GPU instances on Vast.ai for Ollama LLMs",
	Long: `A developer-friendly CLI that automates searching, pricing estimation, 
renting, and managing GPU instances on providers like Vast.ai to host local-compatible LLM servers.
It runs a background daemon to keep SSH tunnels alive and uses a remote watchdog for safe, automatic cleanup.`,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(searchCmd)
	rootCmd.AddCommand(deployCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(profileCmd)
	rootCmd.AddCommand(scaleCmd)
	rootCmd.AddCommand(stackCmd)
	rootCmd.AddCommand(modelsCmd)
	rootCmd.AddCommand(enginesCmd)
}
