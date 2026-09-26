package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"

	"github.com/crom-org/openllm/internal/config"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	stopInstanceID string
	stopAll        bool
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop active LLM instances",
	Long:  `Terminates running GPU contracts on providers (e.g. Vast.ai) and tears down local SSH forwarding tunnels.`,
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := config.LoadConfig()
		if err != nil {
			color.Red("Error: no configuration found. Run 'openllm init' first.")
			os.Exit(1)
		}

		if stopInstanceID == "" && !stopAll {
			color.Red("Error: either --instance or --all is required.")
			cmd.Usage()
			os.Exit(1)
		}

		statusURL := fmt.Sprintf("http://127.0.0.1:%d/status", cfg.LocalDaemonPort)
		stopURL := fmt.Sprintf("http://127.0.0.1:%d/stop", cfg.LocalDaemonPort)

		var idsToStop []string

		if stopAll {
			resp, err := http.Get(statusURL)
			if err != nil {
				color.Red("Connection failed: could not connect to daemon to query active instances.")
				os.Exit(1)
			}

			body, _ := ioutil.ReadAll(resp.Body)
			resp.Body.Close()

			var instances []map[string]interface{}
			if err := json.Unmarshal(body, &instances); err != nil {
				color.Red("Failed to parse status response.")
				os.Exit(1)
			}

			for _, inst := range instances {
				status := inst["status"].(string)
				if status == "running" || status == "deploying" {
					idsToStop = append(idsToStop, inst["id"].(string))
				}
			}

			if len(idsToStop) == 0 {
				color.Cyan("No active or deploying instances found to stop.")
				return
			}
		} else {
			idsToStop = append(idsToStop, stopInstanceID)
		}

		for _, id := range idsToStop {
			color.Cyan("Stopping instance %s...", id)
			
			payload := map[string]string{
				"instance_id": id,
			}
			jsonBytes, _ := json.Marshal(payload)

			resp, err := http.Post(stopURL, "application/json", bytes.NewBuffer(jsonBytes))
			if err != nil {
				color.Red("Failed to send stop command to daemon for instance %s: %v", id, err)
				continue
			}

			body, _ := ioutil.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				color.Green("✓ Instance %s successfully stopped and destroyed on provider.", id)
			} else {
				color.Red("Error stopping instance %s: %s", id, string(body))
			}
		}
	},
}

func init() {
	stopCmd.Flags().StringVar(&stopInstanceID, "instance", "", "Vast.ai Instance/Contract ID to stop")
	stopCmd.Flags().BoolVar(&stopAll, "all", false, "Stop all running and deploying instances")
}
