package vastai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crom-org/openllm/internal/models"
	"github.com/crom-org/openllm/internal/providers"
)

type Client struct {
	httpClient *http.Client
}

func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) Name() string {
	return "vastai"
}

// VastOffer representa um item do array "offers" retornado pela API do Vast.ai
type VastOffer struct {
	ID           interface{} `json:"id"`
	MachineID    int         `json:"machine_id"`
	GpuName      string      `json:"gpu_name"`
	NumGpus      int         `json:"num_gpus"`
	GpuRam       float64     `json:"gpu_ram"` // Geralmente em MB (ex: 24576)
	DphTotal     float64     `json:"dph_total"`
	Reliability2 float64     `json:"reliability2"`
	Geolocation  string      `json:"geolocation"`
	Country      string      `json:"country"`
	Rentable     bool        `json:"rentable"`
	Rented       bool        `json:"rented"`
	// Banda medida pelo host — determina a velocidade do pull de imagem/modelo,
	// que na prática domina o tempo de deploy (imagens de 8-12GB).
	InetDown float64 `json:"inet_down"` // Mbps
	DiskBW   float64 `json:"disk_bw"`   // MB/s leitura+escrita
}

type BundlesResponse struct {
	Offers []VastOffer `json:"offers"`
}

func (c *Client) Search(ctx context.Context, req providers.SearchRequest) ([]providers.Machine, error) {
	url := "https://console.vast.ai/api/v0/bundles/"
	
	// Filtro de pesquisa padrão
	// Buscamos apenas on-demand, verificados, não alugados no momento
	payload := map[string]interface{}{
		"limit":    100,
		"type":     "on-demand",
		"verified": map[string]interface{}{"eq": true},
		"rentable": map[string]interface{}{"eq": true},
		"rented":   map[string]interface{}{"eq": false},
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("vast.ai search returned status %d: %s", resp.StatusCode, string(body))
	}

	var bundlesResp BundlesResponse
	if err := json.NewDecoder(resp.Body).Decode(&bundlesResp); err != nil {
		return nil, err
	}

	var results []providers.Machine
	for _, o := range bundlesResp.Offers {
		// Convert ID to string
		var idStr string
		switch v := o.ID.(type) {
		case string:
			idStr = v
		case float64:
			idStr = strconv.FormatFloat(v, 'f', -1, 64)
		case int:
			idStr = strconv.Itoa(v)
		default:
			idStr = fmt.Sprintf("%v", o.ID)
		}

		// Vast.ai gpu_ram é tipicamente em MB. Convertemos para GB.
		vramGB := o.GpuRam
		if vramGB > 100 {
			vramGB = o.GpuRam / 1024.0
		}

		// Calcula a VRAM total da máquina
		totalVram := vramGB * float64(o.NumGpus)

		// Filtra por VRAM se especificado
		if req.MinVRAM > 0 && totalVram < req.MinVRAM {
			continue
		}

		// Filtra por número de GPUs se especificado
		if req.GPUCount > 0 && o.NumGpus != req.GPUCount {
			continue
		}

		// Estima a métrica do modelo nessa GPU
		estimatedMetric := models.EstimateMetric(req.Model, o.GpuName, o.NumGpus, req.Metric)

		// Filtra por métrica se especificado
		if req.MinTPS > 0 && estimatedMetric > 0 {
			if req.Metric == "rtf" {
				// RTF: menor é melhor (real-time factor)
				if estimatedMetric > req.MinTPS {
					continue
				}
			} else {
				// TPS, itps, fps: maior é melhor
				if estimatedMetric < req.MinTPS {
					continue
				}
			}
		}

		loc := o.Geolocation
		if loc == "" {
			loc = o.Country
		}
		if loc == "" {
			loc = "Unknown"
		}

		results = append(results, providers.Machine{
			ID:             idStr,
			Provider:       c.Name(),
			GPU:            o.GpuName,
			VRAM:           vramGB,
			GPUCount:       o.NumGpus,
			CostPerHour:    o.DphTotal,
			EstimatedTPS:   estimatedMetric, // compat: usado p/ tps
			EstimatedMetric: estimatedMetric,
			Metric:         req.Metric,
			Location:       loc,
			NetMbps:        o.InetDown,
		})
	}

	return results, nil
}

type DeployResponse struct {
	Success     bool        `json:"success"`
	NewContract interface{} `json:"new_contract"`
	Error       string      `json:"error,omitempty"`
}

func (c *Client) Deploy(ctx context.Context, req providers.DeployRequest) (*providers.InstanceInfo, error) {
	// A API do Vast.ai espera um PUT para /asks/{OFFER_ID}/
	url := fmt.Sprintf("https://console.vast.ai/api/v0/asks/%s/", req.MachineID)

	onstart := req.OnstartCmd
	if onstart == "" {
		onstart = "echo 'Container starting...'"
	}

	// Definimos o template e comando inicial na máquina
	// O watchdog será copiado via SSH, a engine é iniciada pelo entrypoint/onstart
	diskGB := req.DiskGB
	if diskGB <= 0 {
		diskGB = 35.0 // default
	}
	payload := map[string]interface{}{
		"image":   req.Image,
		"disk":    diskGB,
		"runtype": "ssh",
		"label":   fmt.Sprintf("openllm-%s", strings.ReplaceAll(req.Model, ":", "-")),
		"onstart": onstart,
	}
	// Env da engine (Definition.Env) → variáveis de ambiente do container.
	// Ex: PROVISIONING_SCRIPT, HF_TOKEN. Omitido quando vazio.
	if len(req.Env) > 0 {
		payload["env"] = req.Env
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", req.APIKey))

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vast.ai deploy returned status %d: %s", resp.StatusCode, string(body))
	}

	var deployResp DeployResponse
	if err := json.Unmarshal(body, &deployResp); err != nil {
		return nil, fmt.Errorf("failed to parse deploy response: %w", err)
	}

	var contractID string
	switch v := deployResp.NewContract.(type) {
	case string:
		contractID = v
	case float64:
		contractID = strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		contractID = strconv.Itoa(v)
	default:
		contractID = fmt.Sprintf("%v", deployResp.NewContract)
	}

	if contractID == "" || contractID == "<nil>" {
		return nil, fmt.Errorf("vast.ai did not return a contract ID. Error: %s", deployResp.Error)
	}

	// Retorna info básica
	return &providers.InstanceInfo{
		ID:        contractID,
		Provider:  c.Name(),
		MachineID: req.MachineID,
		Status:    "deploying",
	}, nil
}

func (c *Client) Destroy(ctx context.Context, instanceID string, apiKey string) error {
	url := fmt.Sprintf("https://console.vast.ai/api/v0/instances/%s/", instanceID)

	httpReq, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("vast.ai destroy returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (c *Client) changeState(ctx context.Context, instanceID string, apiKey string, state string) error {
	url := fmt.Sprintf("https://console.vast.ai/api/v0/instances/%s/", instanceID)

	payload := map[string]string{"state": state}
	payloadBytes, _ := json.Marshal(payload)

	httpReq, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("vast.ai change state returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (c *Client) Pause(ctx context.Context, instanceID string, apiKey string) error {
	return c.changeState(ctx, instanceID, apiKey, "stopped")
}

func (c *Client) Resume(ctx context.Context, instanceID string, apiKey string) error {
	return c.changeState(ctx, instanceID, apiKey, "running")
}

type VastInstance struct {
	ID           int     `json:"id"`
	ActualStatus string  `json:"actual_status"` // "loading", "running", "offline", etc.
	StatusMsg    string  `json:"status_msg"`
	SshHost      string  `json:"ssh_host"`
	SshPort      int     `json:"ssh_port"`
	GpuName      string  `json:"gpu_name"`
	NumGpus      int     `json:"num_gpus"`
	GpuRam       float64 `json:"gpu_ram"`
	DphTotal     float64 `json:"dph_total"`
	MachineID    int     `json:"machine_id"`
	Label        string  `json:"label"`
}

type InstancesListResponse struct {
	Instances []VastInstance `json:"instances"`
}

func (c *Client) GetStatus(ctx context.Context, instanceID string, apiKey string) (*providers.InstanceInfo, error) {
	// Em vez de chamar o endpoint de instância única que pode variar a estrutura do JSON,
	// chamamos a lista geral de todas instâncias e procuramos o ID correto.
	url := "https://console.vast.ai/api/v0/instances/"

	httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("vast.ai get status list returned status %d: %s", resp.StatusCode, string(body))
	}

	var listResp InstancesListResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, err
	}

	targetID, err := strconv.Atoi(instanceID)
	if err != nil {
		return nil, fmt.Errorf("invalid instance ID format: %w", err)
	}

	for _, inst := range listResp.Instances {
		if inst.ID == targetID {
			status := "deploying"
			if inst.ActualStatus == "running" {
				status = "running"
			} else if inst.ActualStatus == "stopped" || inst.ActualStatus == "exited" {
				status = "stopped"
			} else if strings.Contains(strings.ToLower(inst.ActualStatus), "error") || inst.ActualStatus == "offline" {
				status = "failed"
			}

			vramGB := inst.GpuRam
			if vramGB > 100 {
				vramGB = inst.GpuRam / 1024.0
			}

			return &providers.InstanceInfo{
				ID:          instanceID,
				Provider:    c.Name(),
				MachineID:   strconv.Itoa(inst.MachineID),
				GPU:         inst.GpuName,
				GPUCount:    inst.NumGpus,
				VRAM:        vramGB,
				CostPerHour: inst.DphTotal,
				SSHHost:     inst.SshHost,
				SSHPort:     inst.SshPort,
				Status:      status,
				StatusMsg:   inst.StatusMsg,
				Label:       inst.Label,
			}, nil
		}
	}

	return nil, fmt.Errorf("instance with ID %s not found in vast.ai list", instanceID)
}

// init registra o Vast.ai como ComputeProvider no registry global.
// Basta importar este package com _ "...providers/vastai" para ativá-lo.
func init() {
	providers.RegisterCompute(NewClient())
}
