package cli

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/crom-org/openllm/internal/config"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
)

var statusWatch bool

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show status of current LLM instances",
	Long:  `Queries the running openllmd daemon for a list of active deployments, connection details, and heartbeat statuses.`,
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := config.LoadConfig()
		if err != nil {
			color.Red("Error: no configuration found. Run 'openllm init' first.")
			os.Exit(1)
		}

		statusURL := fmt.Sprintf("http://127.0.0.1:%d/status", cfg.LocalDaemonPort)

		for {
			resp, err := http.Get(statusURL)
			if err != nil {
				color.Red("Connection failed: could not connect to openllmd daemon at :%d.", cfg.LocalDaemonPort)
				os.Exit(1)
			}

			body, err := ioutil.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				color.Red("Error reading response: %v", err)
				os.Exit(1)
			}

			var instances []map[string]interface{}
			if err := json.Unmarshal(body, &instances); err != nil {
				color.Red("Error parsing status response: %v", err)
				os.Exit(1)
			}

			if statusWatch {
				// Limpa terminal
				fmt.Print("\033[H\033[2J")
			}

			color.Cyan("openllm - Deployment Status at %s", time.Now().Format("15:04:05"))
			fmt.Println("------------------------------------------------------------")

			if len(instances) == 0 {
				fmt.Println("No active or historic instances registered.")
			} else {
				table := tablewriter.NewWriter(os.Stdout)
				table.SetHeader([]string{"Instance ID", "Model", "GPU Info", "Cost/h", "SSH Target", "Local Port", "Last Ping", "Active Time", "Status"})
				table.SetHeaderColor(
					tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
					tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
					tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
					tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
					tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
					tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
					tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
					tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
					tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
				)
				table.SetBorder(true)
				table.SetAutoWrapText(false)

				for _, inst := range instances {
					id := fmt.Sprintf("%v", inst["id"])
					model := fmt.Sprintf("%v", inst["model"])
					
					gpuCount := int(inst["gpu_count"].(float64))
					gpuStr := fmt.Sprintf("%s x%d", inst["gpu"], gpuCount)
					
					cost := color.YellowString("$%.3f/h", inst["cost_per_hour"])
					
					sshTarget := "-"
					if inst["ssh_host"] != "" {
						sshTarget = fmt.Sprintf("%s:%v", inst["ssh_host"], inst["ssh_port"])
					}

					localPortStr := "-"
					localPortVal := inst["local_port"]
					if localPortVal != nil && localPortVal.(float64) > 0 {
						localPortStr = strconv.Itoa(int(localPortVal.(float64)))
					}

					lastPing := "-"
					if inst["last_ping"] != nil {
						lastPing = fmt.Sprintf("%v", inst["last_ping"])
					}

					timeActive := fmt.Sprintf("%v", inst["time_active"])
					status := fmt.Sprintf("%v", inst["status"])

					statusStr := status
					if status == "running" {
						statusStr = color.GreenString("running")
					} else if status == "deploying" {
						statusStr = color.YellowString("deploying")
					} else if status == "stopped" {
						statusStr = color.HiBlackString("stopped")
					} else if status == "failed" {
						statusStr = color.RedString("failed")
					}

					table.Append([]string{
						id, model, gpuStr, cost, sshTarget, localPortStr, lastPing, timeActive, statusStr,
					})
				}
				table.Render()
			}

			if !statusWatch {
				break
			}
			time.Sleep(3 * time.Second)
		}
	},
}

func init() {
	statusCmd.Flags().BoolVarP(&statusWatch, "watch", "w", false, "Continuously watch status updates")
}
