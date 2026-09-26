package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/crom-org/openllm/internal/config"
	"github.com/crom-org/openllm/internal/storage"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
)

var profileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Manage reusable configuration profiles",
	Long:  `Saves and loads settings to/from SQLite so you can easily switch between model size presets.`,
}

var profileSaveCmd = &cobra.Command{
	Use:   "save <name>",
	Short: "Save current configuration in openllm.json as a profile",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]
		
		cfg, err := config.LoadConfig()
		if err != nil {
			color.Red("Error loading config: make sure you have openllm.json. Run 'openllm init'.")
			os.Exit(1)
		}

		db, err := storage.OpenDB()
		if err != nil {
			color.Red("Database connection error: %v", err)
			os.Exit(1)
		}
		defer db.Close()

		p := &storage.Profile{
			ID:             fmt.Sprintf("prof-%d", time.Now().Unix()),
			Name:           name,
			Provider:       cfg.Provider,
			Model:          cfg.Model,
			TpsTarget:      cfg.TpsTarget,
			InstancesCount: cfg.InstancesCount,
			CreatedAt:      time.Now(),
		}

		if err := db.SaveProfile(p); err != nil {
			color.Red("Failed to save profile: %v", err)
			os.Exit(1)
		}

		color.Green("✓ Profile '%s' successfully saved in SQLite.", name)
	},
}

var profileLoadCmd = &cobra.Command{
	Use:   "load <name>",
	Short: "Load a profile and overwrite local openllm.json",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]

		db, err := storage.OpenDB()
		if err != nil {
			color.Red("Database connection error: %v", err)
			os.Exit(1)
		}
		defer db.Close()

		p, err := db.GetProfile(name)
		if err != nil {
			color.Red("Failed to query profile: %v", err)
			os.Exit(1)
		}

		if p == nil {
			color.Red("Profile '%s' not found.", name)
			os.Exit(1)
		}

		// Carrega a configuração atual para preservar a chave de API
		cfg, err := config.LoadConfig()
		if err != nil {
			cfg = config.DefaultConfig()
		}

		cfg.Provider = p.Provider
		cfg.Model = p.Model
		cfg.TpsTarget = p.TpsTarget
		cfg.InstancesCount = p.InstancesCount

		if err := config.SaveConfig(cfg); err != nil {
			color.Red("Failed to write openllm.json: %v", err)
			os.Exit(1)
		}

		color.Green("✓ Loaded profile '%s' into openllm.json. Current model is now %s.", name, p.Model)
	},
}

var profileListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all saved profiles",
	Run: func(cmd *cobra.Command, args []string) {
		db, err := storage.OpenDB()
		if err != nil {
			color.Red("Database connection error: %v", err)
			os.Exit(1)
		}
		defer db.Close()

		profiles, err := db.ListProfiles()
		if err != nil {
			color.Red("Failed to query profiles: %v", err)
			os.Exit(1)
		}

		if len(profiles) == 0 {
			fmt.Println("No saved profiles found. Use 'openllm profile save <name>' to save current settings.")
			return
		}

		table := tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{"Profile Name", "Provider", "Model", "TPS Target", "Instances", "Created At"})
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

		for _, p := range profiles {
			table.Append([]string{
				p.Name,
				p.Provider,
				p.Model,
				fmt.Sprintf("%.0f", p.TpsTarget),
				fmt.Sprintf("%d", p.InstancesCount),
				p.CreatedAt.Format("2006-01-02 15:04:05"),
			})
		}
		table.Render()
	},
}

var profileDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a saved profile",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]

		db, err := storage.OpenDB()
		if err != nil {
			color.Red("Database connection error: %v", err)
			os.Exit(1)
		}
		defer db.Close()

		p, err := db.GetProfile(name)
		if err != nil {
			color.Red("Failed to query profile: %v", err)
			os.Exit(1)
		}

		if p == nil {
			color.Red("Profile '%s' not found.", name)
			os.Exit(1)
		}

		if err := db.DeleteProfile(name); err != nil {
			color.Red("Failed to delete profile: %v", err)
			os.Exit(1)
		}

		color.Green("✓ Profile '%s' deleted from SQLite database.", name)
	},
}

func init() {
	profileCmd.AddCommand(profileSaveCmd)
	profileCmd.AddCommand(profileLoadCmd)
	profileCmd.AddCommand(profileListCmd)
	profileCmd.AddCommand(profileDeleteCmd)
}
