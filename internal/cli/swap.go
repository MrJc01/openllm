package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	"github.com/crom-org/openllm/internal/config"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	swapInstanceID string
	swapModel      string
)

var swapCmd = &cobra.Command{
	Use:   "swap",
	Short: "Swap the model on a running instance (no re-rent needed)",
	Long: `Downloads the new model on the running GPU machine and switches the load balancer
to it when ready. The old model keeps serving during the download. Much faster than
destroying and re-deploying a machine.`,
	Example: `  openllm swap --instance 48967110 --model llama3.2:3b`,
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := config.LoadConfig()
		if err != nil {
			color.Red("Error: no configuration found. Run 'openllm init' first.")
			os.Exit(1)
		}

		if swapInstanceID == "" || swapModel == "" {
			color.Red("--instance and --model are required.")
			os.Exit(1)
		}

		payload, _ := json.Marshal(map[string]string{
			"instance_id": swapInstanceID,
			"model":       swapModel,
		})

		color.Cyan("Swapping model on instance %s to %q...", swapInstanceID, swapModel)
		resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/swap", cfg.LocalDaemonPort),
			"application/json", bytes.NewBuffer(payload))
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

		color.Green("✓ Swap started! The new model downloads in background.")
		color.Cyan("Watch progress with: openllm status --watch")

		// Polling até o swap completar (model muda no status)
		statusURL := fmt.Sprintf("http://127.0.0.1:%d/status", cfg.LocalDaemonPort)
		for i := 0; i < 240; i++ {
			time.Sleep(5 * time.Second)
			resp, err := http.Get(statusURL)
			if err != nil {
				continue
			}
			raw, _ := ioutil.ReadAll(resp.Body)
			var instances []map[string]interface{}
			if json.Unmarshal(raw, &instances) != nil {
				continue
			}
			for _, inst := range instances {
				if fmt.Sprintf("%v", inst["id"]) == swapInstanceID && fmt.Sprintf("%v", inst["model"]) == swapModel {
					color.Green("\n🚀 Instance %s is now serving %q!", swapInstanceID, swapModel)
					return
				}
			}
		}
	},
}

func init() {
	swapCmd.Flags().StringVar(&swapInstanceID, "instance", "", "Instance ID to swap the model on")
	swapCmd.Flags().StringVar(&swapModel, "model", "", "New model to download and run")
	rootCmd.AddCommand(swapCmd)
}