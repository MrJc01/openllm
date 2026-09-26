package config

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
)

// ProviderConfig contém as configurações específicas de um provider.
type ProviderConfig struct {
	APIKey  string            `json:"api_key"`
	Options map[string]string `json:"options,omitempty"` // Configs livres: region, http_referer, etc.
}

// Config representa o arquivo openllm.json
type Config struct {
	// Provider ativo: "vastai", "openrouter", etc.
	Provider string `json:"provider"`

	// Configurações de cada provider pelo nome (novo formato)
	Providers map[string]ProviderConfig `json:"providers,omitempty"`

	// --- Legado: mantido para migração automática de configs antigos ---
	LegacyVastAiApiKey string `json:"vastai_api_key,omitempty"`

	// Configurações gerais (independem do provider)
	Model                  string  `json:"model"`
	TpsTarget              float64 `json:"tps_target"`
	InstancesCount         int     `json:"instances_count"`
	SSHPrivateKeyPath      string  `json:"ssh_private_key_path,omitempty"`
	WatchdogTimeoutMinutes int     `json:"watchdog_timeout_minutes"`
	// DeployTimeoutMinutes limita a fase "aguardando container ativar" (pull
	// de imagem docker incluso). Imagens grandes em hosts lentos podem passar
	// de 30 min; 0 usa o default.
	DeployTimeoutMinutes int `json:"deploy_timeout_minutes,omitempty"`
	RemotePort             int     `json:"remote_port"`
	LocalProxyPort         int     `json:"local_proxy_port"`
	LocalDaemonPort        int     `json:"local_daemon_port"`
	HeartbeatPort          int     `json:"heartbeat_port"`

// LBStrategy define o balanceamento do proxy: "round-robin" (default)
	// ou "least-connections".
	LBStrategy string `json:"lb_strategy,omitempty"`

	// Autoscaler config (opcional)
	AutoscalerEnabled  bool    `json:"autoscaler_enabled,omitempty"`
	AutoscalerMin      int     `json:"autoscaler_min,omitempty"`      // default 1
	AutoscalerMax      int     `json:"autoscaler_max,omitempty"`      // default 3
	AutoscalerTarget   int     `json:"autoscaler_target,omitempty"`   // reqs em voo por instância, default 2
	AutoscalerMaxCost  float64 `json:"autoscaler_max_cost,omitempty"` // teto $/h total, 0 = sem limite
}

const (
	ConfigFileName = "openllm.json"
	StateDirName   = ".openllm"
	DbFileName     = "openllm.db"
)

// ActiveAPIKey retorna a API key do provider ativo.
// Ordem de resolução: variável de ambiente OPENLLM_<PROVIDER>_API_KEY →
// openllm.json (providers.<provider>.api_key) → campo legado.
// A recomendação é manter a chave fora do arquivo (que pode vazar em backup).
func (c *Config) ActiveAPIKey() string {
	// 1. Env var (ex: OPENLLM_VASTAI_API_KEY)
	envKey := "OPENLLM_" + strings.ToUpper(strings.ReplaceAll(c.Provider, "-", "_")) + "_API_KEY"
	if v := os.Getenv(envKey); v != "" {
		return v
	}

	// 2. Arquivo de configuração
	if c.Providers != nil {
		if pc, ok := c.Providers[c.Provider]; ok && pc.APIKey != "" {
			return pc.APIKey
		}
	}

	// Fallback para legado
	if c.Provider == "vastai" && c.LegacyVastAiApiKey != "" {
		return c.LegacyVastAiApiKey
	}
	return ""
}

// ProviderOption retorna uma opção extra do provider ativo (ex: "http_referer").
func (c *Config) ProviderOption(key string) string {
	if c.Providers != nil {
		if pc, ok := c.Providers[c.Provider]; ok {
			return pc.Options[key]
		}
	}
	return ""
}

// DefaultConfig retorna a configuração padrão do sistema
func DefaultConfig() *Config {
	return &Config{
		Provider: "vastai",
		Providers: map[string]ProviderConfig{
			"vastai": {
				APIKey: "",
			},
			"openrouter": {
				APIKey: "",
				Options: map[string]string{
					"http_referer": "https://openllm.crom.dev",
					"x_title":      "openllm",
				},
			},
		},
		Model:                  "deepseek-r1:7b",
		TpsTarget:              40,
		InstancesCount:         1,
		SSHPrivateKeyPath:      "",
		WatchdogTimeoutMinutes: 5,
		RemotePort:             11434,
		LocalProxyPort:         11434,
		LocalDaemonPort:        17290,
		HeartbeatPort:          17291,
		AutoscalerEnabled:      false,
		AutoscalerMin:          1,
		AutoscalerMax:          3,
		AutoscalerTarget:       2,
		AutoscalerMaxCost:      0,
	}
}

// GetStateDir retorna o caminho absoluto da pasta .openllm no diretório de trabalho
func GetStateDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(wd, StateDirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// GetDbPath retorna o caminho absoluto do banco SQLite local
func GetDbPath() (string, error) {
	stateDir, err := GetStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, DbFileName), nil
}

// LoadConfig carrega e migra automaticamente o openllm.json da pasta atual.
func LoadConfig() (*Config, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	configPath := filepath.Join(wd, ConfigFileName)

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil, err
	}

	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	// Migração automática: campo legado vastai_api_key → providers.vastai.api_key
	if cfg.LegacyVastAiApiKey != "" {
		if cfg.Providers == nil {
			cfg.Providers = make(map[string]ProviderConfig)
		}
		if pc, ok := cfg.Providers["vastai"]; !ok || pc.APIKey == "" {
			cfg.Providers["vastai"] = ProviderConfig{APIKey: cfg.LegacyVastAiApiKey}
		}
		cfg.LegacyVastAiApiKey = "" // Limpa o campo legado para não ser salvo de volta
		// Salva o config já migrado
		_ = SaveConfig(&cfg)
	}

	return &cfg, nil
}

// SaveConfig salva a configuração no arquivo openllm.json da pasta atual
func SaveConfig(cfg *Config) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	configPath := filepath.Join(wd, ConfigFileName)

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	return ioutil.WriteFile(configPath, data, 0644)
}

// AutoInitialize garante que .openllm/ e openllm.json existam.
// Retorna a config carregada, se foi criada agora, e qualquer erro.
func AutoInitialize() (*Config, bool, error) {
	_, err := GetStateDir()
	if err != nil {
		return nil, false, err
	}

	wd, err := os.Getwd()
	if err != nil {
		return nil, false, err
	}
	configPath := filepath.Join(wd, ConfigFileName)

	created := false
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		defaultCfg := DefaultConfig()
		if err := SaveConfig(defaultCfg); err != nil {
			return nil, false, err
		}
		created = true
	}

	cfg, err := LoadConfig()
	if err != nil {
		return nil, false, err
	}

	return cfg, created, nil
}

// Validate verifica se as configurações mínimas necessárias estão presentes.
func (c *Config) Validate() error {
	if c.Provider == "" {
		return errors.New("provider cannot be empty (use 'vastai' or 'openrouter')")
	}
	if c.ActiveAPIKey() == "" {
		return errors.New("api_key for provider \"" + c.Provider + "\" is empty in openllm.json under providers." + c.Provider + ".api_key")
	}
	if c.Model == "" {
		return errors.New("model cannot be empty")
	}
	if c.InstancesCount <= 0 {
		c.InstancesCount = 1
	}
	return nil
}
