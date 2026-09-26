package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/crom-org/openllm/internal/config"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var stackCmd = &cobra.Command{
	Use:   "stack",
	Short: "Deploy multimodal stacks (text + image + video + audio in one endpoint)",
	Long: `A stack is a YAML describing multiple AI services (each an engine+model) that are
rented, provisioned and load-balanced together. The local proxy routes requests to the
right service by path (routes), exposing a single unified API.`,
	Example: `  openllm stack up examples/multimodal.yaml
  openllm stack status
  openllm stack down multimodal`,
}

var stackUpCmd = &cobra.Command{
	Use:   "up <file.yaml>",
	Short: "Deploy all services from a stack YAML",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := config.LoadConfig()
		if err != nil {
			color.Red("Error: no configuration found. Run 'openllm init' first.")
			os.Exit(1)
		}

		filePath := args[0]
		data, err := ioutil.ReadFile(filePath)
		if err != nil {
			color.Red("Failed to read stack file: %v", err)
			os.Exit(1)
		}

		name := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
		payload := map[string]string{
			"name": name,
			"yaml": string(data),
		}
		jsonBytes, _ := json.Marshal(payload)

		color.Cyan("Deploying stack %q from %s...", name, filePath)
		resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/stack", cfg.LocalDaemonPort),
			"application/json", bytes.NewBuffer(jsonBytes))
		if err != nil {
			color.Red("Connection failed: could not connect to daemon. Is 'openllmd' running?")
			os.Exit(1)
		}
		defer resp.Body.Close()

		body, _ := ioutil.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			color.Red("Daemon returned error: %s", string(body))
			os.Exit(1)
		}

		var result struct {
			Stack struct {
				Name     string `json:"name"`
				Services []struct {
					Name      string   `json:"name"`
					GroupID   string   `json:"group_id"`
					Engine    string   `json:"engine"`
					Model     string   `json:"model"`
					Instances int      `json:"instances"`
					Routes    []string `json:"routes"`
					Created   []string `json:"created"`
				} `json:"services"`
			} `json:"stack"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			color.Red("Error parsing stack response: %v", err)
			os.Exit(1)
		}

		color.Green("\n✓ Stack %q deploy started!", result.Stack.Name)
		var allCreated []string
		for _, svc := range result.Stack.Services {
			fmt.Printf("\n  %-12s engine=%-14s model=%s  (%d instance(s))\n", svc.Name, svc.Engine, svc.Model, svc.Instances)
			if len(svc.Routes) > 0 {
				color.Cyan("  %-12s routes: %s", "", strings.Join(svc.Routes, "  "))
			}
			allCreated = append(allCreated, svc.Created...)
		}

		if len(allCreated) > 0 {
			color.Cyan("\nWaiting for all services to become ready...")
			waitForGroup(cfg.LocalDaemonPort, allCreated)
		}

		color.Cyan("\nUnified endpoint: http://localhost:%d (routed by path)", cfg.LocalProxyPort)
		color.Yellow("Keep the daemon running to maintain tunnels and the watchdog.")
	},
}

var stackDownCmd = &cobra.Command{
	Use:   "down <name>",
	Short: "Stop all instances of a stack",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := config.LoadConfig()
		if err != nil {
			color.Red("Error: no configuration found. Run 'openllm init' first.")
			os.Exit(1)
		}

		name := args[0]
		payload, _ := json.Marshal(map[string]string{"name": name})
		resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/stack/down", cfg.LocalDaemonPort),
			"application/json", bytes.NewBuffer(payload))
		if err != nil {
			color.Red("Connection failed: could not connect to daemon.")
			os.Exit(1)
		}
		defer resp.Body.Close()

		body, _ := ioutil.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			color.Red("Daemon returned error: %s", string(body))
			os.Exit(1)
		}

		var result struct {
			StoppedCount int `json:"stopped_count"`
		}
		json.Unmarshal(body, &result)
		color.Green("✓ Stack %q stopped (%d instance(s) destroyed).", name, result.StoppedCount)
	},
}

var stackStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "List deployed stacks and their services",
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := config.LoadConfig()
		if err != nil {
			color.Red("Error: no configuration found. Run 'openllm init' first.")
			os.Exit(1)
		}

		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/stacks", cfg.LocalDaemonPort))
		if err != nil {
			color.Red("Connection failed: could not connect to daemon.")
			os.Exit(1)
		}
		defer resp.Body.Close()

		body, _ := ioutil.ReadAll(resp.Body)
		var stacks []struct {
			Name     string `json:"name"`
			Services []struct {
				GroupID   string   `json:"group_id"`
				Engine    string   `json:"engine"`
				Model     string   `json:"model"`
				Instances int      `json:"instances"`
				Routes    []string `json:"routes"`
			} `json:"services"`
		}
		if err := json.Unmarshal(body, &stacks); err != nil {
			color.Red("Error parsing stacks response: %v", err)
			os.Exit(1)
		}

		if len(stacks) == 0 {
			fmt.Println("No stacks deployed. Try: openllm stack up examples/multimodal.yaml")
			return
		}

		for _, st := range stacks {
			color.Cyan("\nStack: %s", st.Name)
			for _, svc := range st.Services {
				fmt.Printf("  %-28s engine=%-14s model=%-24s running=%d", svc.GroupID, svc.Engine, svc.Model, svc.Instances)
				if len(svc.Routes) > 0 {
					fmt.Printf("  routes=%s", strings.Join(svc.Routes, ","))
				}
				fmt.Println()
			}
		}
	},
}

func init() {
	stackCmd.AddCommand(stackUpCmd)
	stackCmd.AddCommand(stackDownCmd)
	stackCmd.AddCommand(stackStatusCmd)
}