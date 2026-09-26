package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/crom-org/openllm/internal/engines"
	"github.com/crom-org/openllm/internal/models"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
)

var modelsModality string

var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "Browse the model catalog",
	Long:  `Lists models known to the catalog with their engine, modality (text, image, audio, video, tts, asr) and VRAM requirements. Any model not listed defaults to Ollama.`,
}

var modelsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List catalog models",
	Run: func(cmd *cobra.Command, args []string) {
		filter := strings.ToLower(modelsModality)

		table := tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{"Model", "Engine", "Modality", "VRAM (GB)", "Tags"})
		table.SetBorder(true)
		table.SetAutoWrapText(false)

		catalog := models.ListCatalog()
		names := make([]string, 0, len(catalog))
		for name := range catalog {
			names = append(names, name)
		}
		sort.Strings(names)

		shown := 0
		for _, name := range names {
			entry := catalog[name]
			if filter != "" && entry.Modality != filter {
				continue
			}
			engine := entry.Engine
			if !engines.Exists(engine) {
				engine = engine + color.HiBlackString(" (external)")
			}
			table.Append([]string{
				name,
				engine,
				entry.Modality,
				fmt.Sprintf("%.0f", entry.VRAMGB),
				strings.Join(entry.Tags, ", "),
			})
			shown++
		}

		if shown == 0 {
			color.Yellow("No models found for modality %q.", filter)
			return
		}
		table.Render()
		color.Cyan("\n%d models. Any model outside this catalog defaults to ollama (text).", shown)
		color.Cyan("Use 'openllm engines list' to see available engines, or add JSON manifests in ./engines/.")
	},
}

var enginesCmd = &cobra.Command{
	Use:   "engines",
	Short: "Manage inference engines",
}

var enginesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available engines",
	Run: func(cmd *cobra.Command, args []string) {
		table := tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{"Engine", "Modality", "Port", "Default VRAM", "Docker Image"})
		table.SetBorder(true)
		table.SetAutoWrapText(false)

		names := engines.List()
		sort.Strings(names)
		for _, name := range names {
			def := engines.Get(name)
			modality := strings.Join(def.Modality, ", ")
			table.Append([]string{
				name,
				modality,
				fmt.Sprintf("%d", def.RemotePort),
				fmt.Sprintf("%.0f", def.DefaultVRAM),
				def.DockerImage,
			})
		}
		table.Render()
	},
}

func init() {
	enginesCmd.AddCommand(enginesListCmd)
	modelsCmd.AddCommand(modelsListCmd)
	modelsListCmd.Flags().StringVar(&modelsModality, "modality", "", "Filter by modality (text, image, audio, video, tts, asr, embedding)")
}