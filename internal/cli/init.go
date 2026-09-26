package cli

import (
	"fmt"
	"os"

	"github.com/crom-org/openllm/internal/config"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize data directory and default configuration file",
	Long:  `Creates the local .openllm/ state folder and generates a default openllm.json template in the current directory if it does not already exist.`,
	Run: func(cmd *cobra.Command, args []string) {
		cfg, created, err := config.AutoInitialize()
		if err != nil {
			fmt.Printf("Error: failed to initialize: %v\n", err)
			os.Exit(1)
		}

		if created {
			fmt.Println("----------------------------------------------------------------------")
			fmt.Printf("SUCCESS: Initialized state folder .openllm/ and template '%s'\n", config.ConfigFileName)
			fmt.Println("Please configure your API Key in 'openllm.json' and run:")
			fmt.Println("  openllm deploy")
			fmt.Println("----------------------------------------------------------------------")
		} else {
			fmt.Printf("Project already initialized. Current model: %s, provider: %s\n", cfg.Model, cfg.Provider)
		}
	},
}
