package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/briandowns/spinner"
	"github.com/crom-org/openllm/internal/config"
	"github.com/crom-org/openllm/internal/daemon"
	"github.com/crom-org/openllm/internal/engines"
	"github.com/crom-org/openllm/internal/models"
	"github.com/crom-org/openllm/internal/providers"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	deployMachineID   string
	deployModel       string
	deployEngine      string
	deployYes         bool
	deployVRAM        float64
	deployGPUCount    int
	deployCaps        string
	deployCustomImage string
	deployCustomCmd   string
	deployCustomPort  int
	deployMaxCost     float64
)

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy an LLM instance on a GPU machine",
	Long:  `Deploys the chosen model on the specified machine ID, or automatically searches and finds the cheapest best option matching openllm.json.`,
	Run: func(cmd *cobra.Command, args []string) {
		// 1. Auto-inicializa o projeto na pasta se não houver openllm.json
		cfg, created, err := config.AutoInitialize()
		if err != nil {
			color.Red("Initialization error: %v\n", err)
			os.Exit(1)
		}

		if created {
			fmt.Println("----------------------------------------------------------------------")
			fmt.Printf("SUCCESS: Created '%s' template and state directory .openllm/\n", config.ConfigFileName)
			fmt.Println("Please configure your 'vastai_api_key' in openllm.json and run again:")
			fmt.Println("  openllm deploy")
			fmt.Println("----------------------------------------------------------------------")
			os.Exit(0)
		}

		if err := cfg.Validate(); err != nil {
			color.Red("Configuration validation error: %v\nPlease edit 'openllm.json' with valid values.\n", err)
			os.Exit(1)
		}

		// Determina o modelo
		modelName := deployModel
		if modelName == "" {
			modelName = cfg.Model
		}

		// Resolve a engine: flag → catálogo de modelos → "ollama"
		engine := deployEngine
		if engine == "" {
			if entry, ok := models.ResolveCatalog(modelName); ok {
				engine = entry.Engine
				color.Cyan("Auto-resolved engine %q for model %q (modality: %s)", engine, modelName, entry.Modality)
			} else {
				engine = "ollama"
			}
		}
		if !slices.Contains(engines.List(), engine) {
			color.Red("Unknown engine %q. Available engines: %v\n", engine, engines.List())
			os.Exit(1)
		}

		// Custom engine via imagem docker arbitrária (sobe qualquer sistema)
		var customCaps []string
		if deployCaps != "" {
			for _, c := range strings.Split(deployCaps, ",") {
				c = strings.TrimSpace(c)
				if c != "" {
					customCaps = append(customCaps, c)
				}
			}
		}

		machineID := deployMachineID

		// Verifica o tipo do provider
		provType, err := providers.ProviderType(cfg.Provider)
		if err != nil {
			color.Red("Invalid provider: %v\n", err)
			os.Exit(1)
		}

		// 2. Se for compute e não passou machine ID, faz busca automática pela melhor/mais barata opção
		if provType == "compute" && machineID == "" {
			color.Cyan("No --machine-id provided. Finding the best option for %s on %s...", modelName, cfg.Provider)

			var targetVram, targetTps float64
			if engine == "ollama" {
				estVram, estTps := models.GetRequirements(modelName)
				targetVram = estVram
				targetTps = cfg.TpsTarget
				if targetTps == 0 {
					targetTps = estTps
				}
			} else {
				// Modelos de imagem/áudio/vídeo não têm um conceito de "tokens/s" —
				// só filtramos por VRAM mínima.
				targetVram = engines.VRAMFor(engine, modelName)
			}

			// Catálogo tem prioridade sobre estimativa genérica da engine
			if entry, ok := models.ResolveCatalog(modelName); ok && entry.VRAMGB > 0 {
				targetVram = entry.VRAMGB
			}
			// Override manual sempre vence
			if deployVRAM > 0 {
				targetVram = deployVRAM
			}
			// Filtro de nº de GPUs: apenas via --gpu-count explícito.
			// cfg.InstancesCount é nº de RÉPLICAS (escala), não nº de GPUs
			// por máquina — usá-lo aqui escondia ofertas válidas (G5).
			gpuCountFilter := 0
			if deployGPUCount > 0 {
				gpuCountFilter = deployGPUCount
			}

			s := spinner.New(spinner.CharSets[14], 100*time.Millisecond)
			s.Color("cyan")
			s.Suffix = " Searching cheapest option satisfying requirements..."
			s.Start()

			client, ok := providers.GetCompute(cfg.Provider)
			if !ok {
				s.Stop()
				color.Red("Provider %s is not registered or not a compute provider\n", cfg.Provider)
				os.Exit(1)
			}
			results, err := client.Search(context.Background(), providers.SearchRequest{
				MinVRAM:  targetVram,
				MinTPS:   targetTps,
				GPUCount: gpuCountFilter,
				Model:    modelName,
			})
			s.Stop()

			if err != nil {
				color.Red("Search failed: %v\n", err)
				os.Exit(1)
			}

			if len(results) == 0 {
				color.Red("No available GPU machines satisfy the target requirements.\n")
				os.Exit(1)
			}

			// Seleção inteligente (mesma regra do daemon): dentro de +20% do
			// preço mínimo, vence a maior banda de rede — o pull da imagem é
			// o gargalo real do deploy.
			bestOption := daemon.PickBestMachine(results)

			color.Green("✓ Found best candidate: %s x%d (VRAM %.1fGB) - $%.3f/h in %s, %0.f Mbps (Offer ID: %s)",
				bestOption.GPU, bestOption.GPUCount, bestOption.VRAM, bestOption.CostPerHour, bestOption.Location, bestOption.NetMbps, bestOption.ID)
			
			if !deployYes {
				fmt.Print("Deploy this candidate? [y/N]: ")
				var input string
				fmt.Scanln(&input)
				input = strings.TrimSpace(strings.ToLower(input))
				if input != "y" && input != "yes" {
					color.Yellow("Deploy cancelled.")
					return
				}
			}
			machineID = bestOption.ID
		}

		// 3. Comunica com o Daemon local para executar o deploy
		daemonURL := fmt.Sprintf("http://127.0.0.1:%d/deploy", cfg.LocalDaemonPort)
		payload := map[string]interface{}{
			"machine_id": machineID,
			"model":      modelName,
			"engine":     engine,
		}
		if len(customCaps) > 0 {
			payload["capabilities"] = customCaps
		}
		if deployCustomImage != "" {
			payload["custom_image"] = deployCustomImage
		}
		if deployCustomCmd != "" {
			payload["custom_cmd"] = deployCustomCmd
		}
		if deployCustomPort > 0 {
			payload["custom_port"] = deployCustomPort
		}
		if deployMaxCost > 0 {
			payload["max_cost_per_hour"] = deployMaxCost // daemon reconfere antes de alugar
		}
		jsonBytes, _ := json.Marshal(payload)

		color.Cyan("Contacting openllmd daemon at http://127.0.0.1:%d...", cfg.LocalDaemonPort)

		resp, err := http.Post(daemonURL, "application/json", bytes.NewBuffer(jsonBytes))
		if err != nil {
			color.Red("Connection failed: could not connect to daemon. Is 'openllmd' running?")
			color.Yellow("Try starting the daemon in the background first, or run 'openllm daemon start'.")
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := ioutil.ReadAll(resp.Body)
			color.Red("Daemon returned error during deploy: %s", string(body))
			os.Exit(1)
		}

		var respData map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&respData)
		instanceID := respData["instance_id"].(string)

		color.Green("✓ Deployment started in background! Instance ID: %s", instanceID)
		
		// 4. Polling de progresso
		color.Cyan("\nMonitoring deployment status...")
		statusURL := fmt.Sprintf("http://127.0.0.1:%d/status", cfg.LocalDaemonPort)

		s := spinner.New(spinner.CharSets[9], 200*time.Millisecond)
		s.Color("cyan")
		s.Suffix = " Initializing contract and provisioning GPU container..."
		s.Start()

		for {
			time.Sleep(5 * time.Second)
			
			statusResp, err := http.Get(statusURL)
			if err != nil {
				s.Stop()
				color.Red("Error checking status: daemon connection lost.")
				os.Exit(1)
			}

			body, err := ioutil.ReadAll(statusResp.Body)
			statusResp.Body.Close()
			if err != nil {
				continue
			}

			var instances []map[string]interface{}
			if err := json.Unmarshal(body, &instances); err != nil {
				continue
			}

			// Procura nossa instância na lista
			var foundInst map[string]interface{}
			for _, inst := range instances {
				if inst["id"] == instanceID {
					foundInst = inst
					break
				}
			}

			if foundInst == nil {
				s.Stop()
				color.Red("Instance lost or cleared from status.")
				os.Exit(1)
			}

			status := foundInst["status"].(string)

			if status == "running" {
				s.Stop()
				color.Green("\n🚀 SUCCESS: Instance is fully deployed and active!")
				fmt.Printf("Ollama is available locally on: ")
				color.Cyan("http://localhost:%d\n", cfg.LocalProxyPort)
				fmt.Printf("Instance SSH target: %s:%d\n", foundInst["ssh_host"], int(foundInst["ssh_port"].(float64)))
				color.Yellow("Watchdog heartbeat is active. Keep this daemon running to maintain the tunnels.")
				return
			}

			if status == "failed" {
				s.Stop()
				color.Red("\n❌ ERROR: Deployment failed. Check daemon logs for details.")
				os.Exit(1)
			}

			if status == "stopped" {
				s.Stop()
				color.Yellow("\nInstance was stopped or terminated.")
				os.Exit(0)
			}
		}
	},
}

func init() {
	deployCmd.Flags().StringVar(&deployMachineID, "machine-id", "", "Vast.ai Offer ID to rent")
	deployCmd.Flags().StringVar(&deployModel, "model", "", "Model to pull and run (auto-resolves engine via catalog)")
	deployCmd.Flags().StringVar(&deployEngine, "engine", "", "Engine to use (ollama, localai, comfyui, faster-whisper, xtts, vllm...). Auto-resolved from catalog when omitted")
	deployCmd.Flags().BoolVarP(&deployYes, "yes", "y", false, "Accept the cheapest candidate automatically without prompting")
	deployCmd.Flags().Float64Var(&deployVRAM, "vram", 0, "Override VRAM requirement in GB")
	deployCmd.Flags().IntVar(&deployGPUCount, "gpu-count", 0, "Filter for exact number of GPUs on the machine")
	deployCmd.Flags().StringVar(&deployCaps, "capabilities", "", "Comma-separated capabilities for engine resolution (text,image,audio,video)")
	deployCmd.Flags().StringVar(&deployCustomImage, "custom-image", "", "Arbitrary docker image to deploy (runs ANY system)")
	deployCmd.Flags().StringVar(&deployCustomCmd, "custom-cmd", "", "Start command for the custom image (supports {{.Model}})")
	deployCmd.Flags().Float64Var(&deployMaxCost, "max-cost", 0, "Abort if the offer's current price exceeds this $/h (+5% tolerance)")
	deployCmd.Flags().IntVar(&deployCustomPort, "custom-port", 0, "Port the custom image listens on (default 8080)")
}
