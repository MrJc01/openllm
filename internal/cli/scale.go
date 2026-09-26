package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	"github.com/briandowns/spinner"
	"github.com/crom-org/openllm/internal/config"
	"github.com/crom-org/openllm/internal/models"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	scaleModel     string
	scaleEngine    string
	scaleInstances int
	scaleMaxCost   float64
)

var scaleCmd = &cobra.Command{
	Use:   "scale",
	Short: "Scale instances running the same model (horizontal scaling)",
	Long: `Rents or destroys GPU machines so that N instances run the same model behind the
local proxy. Requests are load-balanced across all of them, increasing throughput.`,
	Example: `  openllm scale --model deepseek-r1:7b --instances 3
  openllm scale --model deepseek-r1:7b --instances 1   # reduz, mantendo a mais barata
  openllm scale --model sdxl --instances 2 --max-cost 0.5`,
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := config.LoadConfig()
		if err != nil {
			color.Red("Error: no configuration found. Run 'openllm init' first.")
			os.Exit(1)
		}

		modelName := scaleModel
		if modelName == "" {
			modelName = cfg.Model
		}

		engine := scaleEngine
		if engine == "" {
			if entry, ok := models.ResolveCatalog(modelName); ok {
				engine = entry.Engine
			} else {
				engine = "ollama"
			}
		}

		if scaleInstances < 1 {
			color.Red("--instances must be >= 1 (got %d).", scaleInstances)
			os.Exit(1)
		}

		daemonURL := fmt.Sprintf("http://127.0.0.1:%d/scale", cfg.LocalDaemonPort)
		payload := map[string]interface{}{
			"model":     modelName,
			"engine":    engine,
			"instances": scaleInstances,
		}
		if scaleMaxCost > 0 {
			payload["max_cost_hour"] = scaleMaxCost
		}
		jsonBytes, _ := json.Marshal(payload)

		color.Cyan("Scaling %s/%s to %d instances...", engine, modelName, scaleInstances)

		resp, err := http.Post(daemonURL, "application/json", bytes.NewBuffer(jsonBytes))
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
			GroupID string   `json:"group_id"`
			Current int      `json:"current"`
			Created []string `json:"created"`
			Removed []string `json:"removed"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			color.Red("Error parsing scale response: %v", err)
			os.Exit(1)
		}

		if len(result.Created) > 0 {
			color.Green("✓ %d new instance(s) provisioning: %v", len(result.Created), result.Created)
		}
		if len(result.Removed) > 0 {
			color.Yellow("✓ %d instance(s) removed: %v", len(result.Removed), result.Removed)
		}
		if len(result.Created) == 0 && len(result.Removed) == 0 {
			color.Green("✓ Group already at desired size.")
		}

		// Polling enquanto houver instâncias provisionando
		if len(result.Created) > 0 {
			color.Cyan("\nWaiting for new instances to become ready...")
			waitForGroup(cfg.LocalDaemonPort, result.Created)
		}

		color.Cyan("Proxy is load-balancing %d instance(s) of %q on http://localhost:%d",
			result.Current, modelName, cfg.LocalProxyPort)
	},
}

func waitForGroup(daemonPort int, instanceIDs []string) {
	statusURL := fmt.Sprintf("http://127.0.0.1:%d/status", daemonPort)
	remaining := map[string]bool{}
	for _, id := range instanceIDs {
		remaining[id] = true
	}

	s := newStatusSpinner()
	for {
		time.Sleep(5 * time.Second)
		resp, err := http.Get(statusURL)
		if err != nil {
			s.Stop()
			color.Red("Lost connection to daemon.")
			os.Exit(1)
		}
		body, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()

		var instances []map[string]interface{}
		if json.Unmarshal(body, &instances) != nil {
			continue
		}

		for _, inst := range instances {
			id := fmt.Sprintf("%v", inst["id"])
			if !remaining[id] {
				continue
			}
			status := fmt.Sprintf("%v", inst["status"])
			switch status {
			case "running":
				delete(remaining, id)
				color.Green("  ✓ %s is running", id)
			case "failed", "stopped":
				delete(remaining, id)
				color.Red("  ✗ %s failed (check 'openllm status' and daemon logs)", id)
			}
		}

		if len(remaining) == 0 {
			s.Stop()
			return
		}
	}
}

func newStatusSpinner() *spinner.Spinner {
	s := spinner.New(spinner.CharSets[9], 200*time.Millisecond)
	s.Color("cyan")
	s.Suffix = " Provisioning..."
	s.Start()
	return s
}

func init() {
	scaleCmd.Flags().StringVar(&scaleModel, "model", "", "Model to scale (defaults to openllm.json model)")
	scaleCmd.Flags().StringVar(&scaleEngine, "engine", "", "Engine (auto-resolved from catalog when omitted)")
	scaleCmd.Flags().IntVarP(&scaleInstances, "instances", "n", 0, "Desired number of instances running this model")
	scaleCmd.Flags().Float64Var(&scaleMaxCost, "max-cost", 0, "Max cost per hour (USD) for new rentals")
	_ = scaleCmd.MarkFlagRequired("instances")
}