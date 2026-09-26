package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/briandowns/spinner"
	"github.com/crom-org/openllm/internal/config"
	"github.com/crom-org/openllm/internal/models"
	"github.com/crom-org/openllm/internal/providers"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
)

var (
	searchModel    string
	searchTps      float64
	searchVram     float64
	searchGpuCount int
	searchProvider string
)

var searchCmd = &cobra.Command{
	Use:   "search",
	Short: "Search available GPU machines or remote API models in real-time",
	Long:  `Queries active providers (e.g. Vast.ai or OpenRouter) for available GPU offers or API models.`,
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := config.LoadConfig()
		if err != nil {
			cfg = config.DefaultConfig()
		}

		provName := searchProvider
		if provName == "" {
			provName = cfg.Provider
		}

		provType, err := providers.ProviderType(provName)
		if err != nil {
			color.Red("Error: %v\n", err)
			os.Exit(1)
		}

		modelName := searchModel
		if modelName == "" {
			modelName = cfg.Model
		}

		ctx := context.Background()

		if provType == "inference" {
			// ============================================================
			// FLUXO: Provedor de Inferência (API externa ex: OpenRouter)
			// ============================================================
			client, ok := providers.GetInference(provName)
			if !ok {
				color.Red("Error: inference provider %s not found\n", provName)
				os.Exit(1)
			}

			color.Cyan("\n🔍 Searching models for %q on Inference Provider (%s)", modelName, provName)

			s := spinner.New(spinner.CharSets[14], 100*time.Millisecond)
			s.Color("cyan")
			s.Suffix = " Fetching models from OpenRouter API..."
			s.Start()

			results, err := client.SearchModels(ctx, providers.ModelSearchRequest{
				Query: modelName,
			})
			s.Stop()

			if err != nil {
				color.Red("Error querying provider: %v\n", err)
				os.Exit(1)
			}

			if len(results) == 0 {
				color.Red("No matching models found for query %q.\n", modelName)
				return
			}

			fmt.Println()
			table := tablewriter.NewWriter(os.Stdout)
			table.SetHeader([]string{"Model ID", "Model Name", "Context Limit", "Input / 1M", "Output / 1M", "Type"})
			table.SetHeaderColor(
				tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
				tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
				tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
				tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
				tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
				tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
			)
			table.SetBorder(true)
			table.SetAutoWrapText(false)

			for _, m := range results {
				inputStr := color.YellowString("$%.2f", m.PricePerMInput)
				outputStr := color.YellowString("$%.2f", m.PricePerMOutput)
				typeStr := "Paid"

				if m.IsFree {
					inputStr = color.GreenString("FREE")
					outputStr = color.GreenString("FREE")
					typeStr = color.GreenString("Free (limits)")
				}

				ctxStr := fmt.Sprintf("%d tokens", m.ContextLength)
				if m.ContextLength >= 1000 {
					ctxStr = fmt.Sprintf("%dk", m.ContextLength/1000)
				}

				table.Append([]string{
					m.ID,
					m.Name,
					ctxStr,
					inputStr,
					outputStr,
					typeStr,
				})
			}
			table.Render()

			color.Cyan("\nSummary: Found %d matching models on OpenRouter", len(results))
			fmt.Printf("To use a model, update your config or run: ")
			color.Yellow("openllm deploy --model <Model ID>\n\n")

		} else {
			// ============================================================
			// FLUXO: Provedor de Compute (Vast.ai, RunPod, etc.)
			// ============================================================
			client, ok := providers.GetCompute(provName)
			if !ok {
				color.Red("Error: compute provider %s not found\n", provName)
				os.Exit(1)
			}

			estVram, estTps := models.GetRequirements(modelName)

			targetVram := searchVram
			if targetVram == 0 {
				targetVram = estVram
			}

			targetTps := searchTps
			if targetTps == 0 {
				targetTps = cfg.TpsTarget
			}
			if targetTps == 0 {
				targetTps = estTps
			}

			gpuCount := searchGpuCount
			if gpuCount == 0 {
				gpuCount = cfg.InstancesCount
			}
			if gpuCount == 0 {
				gpuCount = 1
			}

			color.Cyan("\n🔍 Searching GPUs for %s (VRAM target: %.1f GB, TPS target: %.0f)", modelName, targetVram, targetTps)
			color.Yellow("   Min specifications required per instance: VRAM: %.1f GB | GPUs: %d", targetVram, gpuCount)

			s := spinner.New(spinner.CharSets[14], 100*time.Millisecond)
			s.Color("cyan")
			s.Suffix = " Querying " + provName + " API bundles..."
			s.Start()

			results, err := client.Search(ctx, providers.SearchRequest{
				MinVRAM:  targetVram,
				MinTPS:   targetTps,
				GPUCount: gpuCount,
				Model:    modelName,
			})
			s.Stop()

			if err != nil {
				color.Red("Error querying provider: %v\n", err)
				os.Exit(1)
			}

			if len(results) == 0 {
				color.Red("No matching GPU offers found that satisfy the filters.\n")
				return
			}

			fmt.Println()
			table := tablewriter.NewWriter(os.Stdout)
			table.SetHeader([]string{"ID", "GPU Model", "Count", "VRAM (GPU)", "Est. TPS", "Cost/Hour", "Location"})
			table.SetHeaderColor(
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

			var minCost, maxCost float64
			minCost = results[0].CostPerHour
			maxCost = results[0].CostPerHour

			for _, m := range results {
				if m.CostPerHour < minCost {
					minCost = m.CostPerHour
				}
				if m.CostPerHour > maxCost {
					maxCost = m.CostPerHour
				}

				tpsStr := fmt.Sprintf("%.1f t/s", m.EstimatedTPS)
				if m.EstimatedTPS >= targetTps {
					tpsStr = color.GreenString("✓ " + tpsStr)
				}

				table.Append([]string{
					m.ID,
					m.GPU,
					strconv.Itoa(m.GPUCount),
					fmt.Sprintf("%.1f GB", m.VRAM),
					tpsStr,
					color.YellowString("$%.3f/h", m.CostPerHour),
					m.Location,
				})
			}
			table.Render()

			color.Cyan("\nSummary: Found %d matching offers", len(results))
			fmt.Printf("Price Range: ")
			color.Green("$%.3f/h (min) — $%.3f/h (max)\n", minCost, maxCost)
			fmt.Printf("To rent a machine, run: ")
			color.Yellow("openllm deploy --machine-id <ID> --model %s\n\n", modelName)
		}
	},
}

func init() {
	searchCmd.Flags().StringVar(&searchModel, "model", "", "Model name (ex: deepseek-r1:70b or openai/gpt-4o)")
	searchCmd.Flags().Float64Var(&searchTps, "tps", 0, "Target tokens per second")
	searchCmd.Flags().Float64Var(&searchVram, "vram", 0, "Override VRAM requirement in GB")
	searchCmd.Flags().IntVar(&searchGpuCount, "gpu-count", 0, "Filter for exact number of GPUs on the machine")
	searchCmd.Flags().StringVar(&searchProvider, "provider", "", "Override provider (ex: vastai, openrouter)")
}
