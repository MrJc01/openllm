package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type DeployResp struct {
	InstanceID string `json:"instance_id"`
	Status     string `json:"status"`
}

type InstanceStatus struct {
	ID         string  `json:"id"`
	Status     string  `json:"status"`
	SSHHost    string  `json:"ssh_host"`
	LocalPort  int     `json:"local_port"`
	Model      string  `json:"model"`
	Engine     string  `json:"engine"`
}

type RunResult struct {
	Run                int     `json:"run"`
	Engine             string  `json:"engine"`
	Model              string  `json:"model"`
	CustomImage        string  `json:"custom_image,omitempty"`
	InstanceID         string  `json:"instance_id"`
	StartTS            float64 `json:"start_ts"`
	SSHReadyTS         float64 `json:"ssh_ready_ts,omitempty"`
	EngineHealthyTS    float64 `json:"engine_healthy_ts,omitempty"`
	ModelReadyTS       float64 `json:"model_ready_ts,omitempty"`
	RunningTS          float64 `json:"running_ts,omitempty"`
	// Swap metrics
	SwapStartTS        float64 `json:"swap_start_ts,omitempty"`
	SwapModelReadyTS   float64 `json:"swap_model_ready_ts,omitempty"`
	SwapTotalTS        float64 `json:"swap_total_ts,omitempty"`
	// Shutdown metrics
	ShutdownStartTS    float64 `json:"shutdown_start_ts,omitempty"`
	ShutdownDoneTS     float64 `json:"shutdown_done_ts,omitempty"`
	// First inference
	FirstInferenceTS   float64 `json:"first_inference_ts,omitempty"`
	FirstInferenceOK   bool    `json:"first_inference_ok,omitempty"`
}

func nowTS() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <engine> <model> [custom_image] [custom_cmd] [runs]\n", os.Args[0])
		os.Exit(1)
	}

	engine := os.Args[1]
	model := os.Args[2]
	customImage := ""
	customCmd := ""
	runs := 3

	if len(os.Args) > 3 {
		customImage = os.Args[3]
	}
	if len(os.Args) > 4 {
		customCmd = os.Args[4]
	}
	if len(os.Args) > 5 {
		runs, _ = strconv.Atoi(os.Args[5])
	}

	daemonPort := 17290
	daemonURL := fmt.Sprintf("http://127.0.0.1:%d", daemonPort)

	// Check daemon
	resp, err := http.Get(daemonURL + "/status")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Daemon not running on port %d: %v\n", daemonPort, err)
		os.Exit(1)
	}
	resp.Body.Close()

	// Log dir
	logDir := filepath.Join("cold_boot_logs", time.Now().Format("20060102_150405"))
	os.MkdirAll(logDir, 0755)

	fmt.Printf("=== Cold Boot Test ===\n")
	fmt.Printf("Engine: %s\n", engine)
	fmt.Printf("Model: %s\n", model)
	if customImage != "" {
		fmt.Printf("Custom Image: %s\n", customImage)
	}
	fmt.Printf("Runs: %d\n", runs)
	fmt.Printf("Log dir: %s\n\n", logDir)

	results := []RunResult{}

	for run := 1; run <= runs; run++ {
		fmt.Printf("--- Run %d ---\n", run)

		// Build payload
		payload := map[string]string{
			"model":  model,
			"engine": engine,
		}
		if customImage != "" {
			payload["custom_image"] = customImage
		}
		if customCmd != "" {
			payload["custom_cmd"] = customCmd
		}

		jsonPayload, _ := json.Marshal(payload)
		fmt.Printf("Payload: %s\n", string(jsonPayload))

		startTS := nowTS()

		// Deploy
		httpReq, _ := http.NewRequest("POST", daemonURL+"/deploy", strings.NewReader(string(jsonPayload)))
		httpReq.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(httpReq)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Deploy error: %v\n", err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var deployResp DeployResp
		if err := json.Unmarshal(body, &deployResp); err != nil || deployResp.InstanceID == "" {
			fmt.Fprintf(os.Stderr, "Deploy failed: %s\n", string(body))
			continue
		}

		instanceID := deployResp.InstanceID
		fmt.Printf("Instance ID: %s\n", instanceID)

		result := RunResult{
			Run:          run,
			Engine:       engine,
			Model:        model,
			CustomImage:  customImage,
			InstanceID:   instanceID,
			StartTS:      startTS,
		}

		// Poll status
		sshReady := false
		engineHealthy := false
		modelReady := false

		for {
			time.Sleep(3 * time.Second)
			now := nowTS()

			resp, err := http.Get(daemonURL + "/status")
			if err != nil {
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			var instances []InstanceStatus
			if err := json.Unmarshal(body, &instances); err != nil {
				continue
			}

			var inst *InstanceStatus
			for i := range instances {
				if instances[i].ID == instanceID {
					inst = &instances[i]
					break
				}
			}
			if inst == nil {
				continue
			}

			// SSH ready
			if !sshReady && inst.SSHHost != "" {
				result.SSHReadyTS = now
				sshReady = true
				fmt.Printf("[%.1fs] SSH ready: %s\n", now-startTS, inst.SSHHost)
			}

			// Engine healthy (status running)
			if !engineHealthy && inst.Status == "running" {
				result.EngineHealthyTS = now
				engineHealthy = true
				fmt.Printf("[%.1fs] Engine healthy (status running)\n", now-startTS)
			}

			// Model ready - check health endpoint
			if engineHealthy && !modelReady && inst.LocalPort > 0 {
				healthy := checkModelHealth(engine, inst.LocalPort, model)
				if healthy {
					result.ModelReadyTS = now
					modelReady = true
					fmt.Printf("[%.1fs] Model ready (health check OK)\n", now-startTS)
				}
			}

			// Fully running
			if modelReady {
				result.RunningTS = now
				fmt.Printf("[%.1fs] FULLY RUNNING\n", now-startTS)
				break
			}

			if inst.Status == "failed" {
				fmt.Printf("Deploy failed\n")
				break
			}
		}

		// Stop instance
		stopPayload := fmt.Sprintf(`{"instance_id":"%s"}`, instanceID)
		stopReq, _ := http.NewRequest("POST", daemonURL+"/stop", strings.NewReader(stopPayload))
		stopReq.Header.Set("Content-Type", "application/json")
		client.Do(stopReq)

		results = append(results, result)

		// Save individual result
		saveResult(logDir, run, result)

		fmt.Printf("Run %d complete. Waiting 10s before next...\n\n", run)
		time.Sleep(10 * time.Second)
	}

	// Summary
	printSummary(results, logDir)
}

func checkModelHealth(engine string, port int, model string) bool {
	url := ""
	switch engine {
	case "ollama":
		url = fmt.Sprintf("http://127.0.0.1:%d/api/tags", port)
	case "vllm":
		url = fmt.Sprintf("http://127.0.0.1:%d/v1/models", port)
	case "comfyui":
		url = fmt.Sprintf("http://127.0.0.1:%d/", port)
	default:
		url = fmt.Sprintf("http://127.0.0.1:%d/", port)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return false
	}

	switch engine {
	case "ollama":
		var data map[string]interface{}
		if err := json.Unmarshal(body, &data); err != nil {
			return false
		}
		models, ok := data["models"].([]interface{})
		if !ok {
			return false
		}
		for _, m := range models {
			if mm, ok := m.(map[string]interface{}); ok {
				if name, ok := mm["name"].(string); ok && strings.HasPrefix(name, model) {
					return true
				}
			}
		}
	case "vllm":
		var data map[string]interface{}
		if err := json.Unmarshal(body, &data); err != nil {
			return false
		}
		models, ok := data["data"].([]interface{})
		if !ok {
			return false
		}
		for _, m := range models {
			if mm, ok := m.(map[string]interface{}); ok {
				if id, ok := mm["id"].(string); ok && strings.HasPrefix(id, model) {
					return true
				}
			}
		}
	case "comfyui":
		// ComfyUI just needs to respond
		return true
	}
	return false
}

func saveResult(logDir string, run int, result RunResult) {
	filename := filepath.Join(logDir, fmt.Sprintf("run_%d.json", run))
	f, _ := os.Create(filename)
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.Encode(result)
}

func printSummary(results []RunResult, logDir string) {
	fmt.Printf("\n=== SUMMARY ===\n")

	if len(results) == 0 {
		fmt.Println("No successful runs")
		return
	}

	// Print table - Deploy metrics
	fmt.Printf("%-4s %-10s %-10s %-10s %-10s %-10s\n", "Run", "Engine", "SSH(s)", "Engine(s)", "Model(s)", "Total(s)")
	for _, r := range results {
		ssh := fmt.Sprintf("%.1f", r.SSHReadyTS-r.StartTS)
		eng := fmt.Sprintf("%.1f", r.EngineHealthyTS-r.StartTS)
		mod := fmt.Sprintf("%.1f", r.ModelReadyTS-r.StartTS)
		tot := fmt.Sprintf("%.1f", r.RunningTS-r.StartTS)
		if r.SSHReadyTS == 0 { ssh = "N/A" }
		if r.EngineHealthyTS == 0 { eng = "N/A" }
		if r.ModelReadyTS == 0 { mod = "N/A" }
		if r.RunningTS == 0 { tot = "N/A" }
		fmt.Printf("%-4d %-10s %-10s %-10s %-10s %-10s\n", r.Run, r.Engine, ssh, eng, mod, tot)
	}

	// Print table - Swap metrics (if any)
	hasSwap := false
	for _, r := range results {
		if r.SwapTotalTS > 0 {
			hasSwap = true
			break
		}
	}
	if hasSwap {
		fmt.Printf("\n%-4s %-10s %-10s %-10s %-10s\n", "Run", "Engine", "SwapStart(s)", "SwapReady(s)", "SwapTotal(s)")
		for _, r := range results {
			if r.SwapTotalTS > 0 {
				ss := fmt.Sprintf("%.1f", r.SwapStartTS-r.StartTS)
				sr := fmt.Sprintf("%.1f", r.SwapModelReadyTS-r.StartTS)
				st := fmt.Sprintf("%.1f", r.SwapTotalTS-r.StartTS)
				fmt.Printf("%-4d %-10s %-10s %-10s %-10s\n", r.Run, r.Engine, ss, sr, st)
			}
		}
	}

	// Print table - Shutdown metrics (if any)
	hasShutdown := false
	for _, r := range results {
		if r.ShutdownDoneTS > 0 {
			hasShutdown = true
			break
		}
	}
	if hasShutdown {
		fmt.Printf("\n%-4s %-10s %-10s\n", "Run", "Engine", "Shutdown(s)")
		for _, r := range results {
			if r.ShutdownDoneTS > 0 {
				sd := fmt.Sprintf("%.1f", r.ShutdownDoneTS-r.ShutdownStartTS)
				fmt.Printf("%-4d %-10s %-10s\n", r.Run, r.Engine, sd)
			}
		}
	}

	// Print table - First Inference (if any)
	hasInference := false
	for _, r := range results {
		if r.FirstInferenceTS > 0 {
			hasInference = true
			break
		}
	}
	if hasInference {
		fmt.Printf("\n%-4s %-10s %-10s %-10s\n", "Run", "Engine", "Inference(s)", "OK")
		for _, r := range results {
			if r.FirstInferenceTS > 0 {
				inf := fmt.Sprintf("%.1f", r.FirstInferenceTS-r.StartTS)
				ok := "no"
				if r.FirstInferenceOK { ok = "yes" }
				fmt.Printf("%-4d %-10s %-10s %-10s\n", r.Run, r.Engine, inf, ok)
			}
		}
	}

	// Averages
	metrics := []string{"ssh_ready", "engine_healthy", "model_ready", "total"}
	for _, m := range metrics {
		var vals []float64
		for _, r := range results {
			var v float64
			switch m {
			case "ssh_ready":
				if r.SSHReadyTS > 0 { v = r.SSHReadyTS - r.StartTS }
			case "engine_healthy":
				if r.EngineHealthyTS > 0 { v = r.EngineHealthyTS - r.StartTS }
			case "model_ready":
				if r.ModelReadyTS > 0 { v = r.ModelReadyTS - r.StartTS }
			case "total":
				if r.RunningTS > 0 { v = r.RunningTS - r.StartTS }
			}
			if v > 0 {
				vals = append(vals, v)
			}
		}
		if len(vals) > 0 {
			mean := avg(vals)
			std := stddev(vals)
			minV := min(vals)
			maxV := max(vals)
			fmt.Printf("%-12s: mean=%.1fs stdev=%.1fs min=%.1fs max=%.1fs (n=%d)\n", m, mean, std, minV, maxV, len(vals))
		}
	}

	// Swap averages
	swapMetrics := []string{"swap_start", "swap_ready", "swap_total"}
	for _, m := range swapMetrics {
		var vals []float64
		for _, r := range results {
			var v float64
			switch m {
			case "swap_start":
				if r.SwapStartTS > 0 { v = r.SwapStartTS - r.StartTS }
			case "swap_ready":
				if r.SwapModelReadyTS > 0 { v = r.SwapModelReadyTS - r.StartTS }
			case "swap_total":
				if r.SwapTotalTS > 0 { v = r.SwapTotalTS - r.StartTS }
			}
			if v > 0 {
				vals = append(vals, v)
			}
		}
		if len(vals) > 0 {
			mean := avg(vals)
			std := stddev(vals)
			minV := min(vals)
			maxV := max(vals)
			fmt.Printf("%-12s: mean=%.1fs stdev=%.1fs min=%.1fs max=%.1fs (n=%d)\n", m, mean, std, minV, maxV, len(vals))
		}
	}

	// Shutdown average
	var shutdownVals []float64
	for _, r := range results {
		if r.ShutdownDoneTS > 0 && r.ShutdownStartTS > 0 {
			shutdownVals = append(shutdownVals, r.ShutdownDoneTS-r.ShutdownStartTS)
		}
	}
	if len(shutdownVals) > 0 {
		mean := avg(shutdownVals)
		std := stddev(shutdownVals)
		minV := min(shutdownVals)
		maxV := max(shutdownVals)
		fmt.Printf("%-12s: mean=%.1fs stdev=%.1fs min=%.1fs max=%.1fs (n=%d)\n", "shutdown", mean, std, minV, maxV, len(shutdownVals))
	}

	// First inference average
	var infVals []float64
	for _, r := range results {
		if r.FirstInferenceTS > 0 && r.FirstInferenceOK {
			infVals = append(infVals, r.FirstInferenceTS-r.StartTS)
		}
	}
	if len(infVals) > 0 {
		mean := avg(infVals)
		std := stddev(infVals)
		minV := min(infVals)
		maxV := max(infVals)
		fmt.Printf("%-12s: mean=%.1fs stdev=%.1fs min=%.1fs max=%.1fs (n=%d)\n", "1st_infer", mean, std, minV, maxV, len(infVals))
	}

	// Save summary JSON
	summary := map[string]interface{}{
		"timestamp":    time.Now().Format(time.RFC3339),
		"engine":       results[0].Engine,
		"model":        results[0].Model,
		"custom_image": results[0].CustomImage,
		"runs":         len(results),
		"results":      results,
	}
	f, _ := os.Create(filepath.Join(logDir, "summary.json"))
	json.NewEncoder(f).Encode(summary)
	f.Close()

	fmt.Printf("\nLogs salvos em: %s\n", logDir)
}

// ============================================================
// Extended test functions: Swap, Shutdown, First Inference
// ============================================================

// RunSwapTest tests model swap on an existing instance
func RunSwapTest(daemonURL, instanceID, newModel, engine string) (RunResult, error) {
	result := RunResult{
		Run:         0, // swap runs are not numbered like deploy runs
		Engine:      engine,
		Model:       newModel,
		InstanceID:  instanceID,
		StartTS:     nowTS(),
	}

	fmt.Printf("\n=== SWAP TEST: %s -> %s ===\n", result.Model, newModel)

	// Initiate swap
	swapPayload := fmt.Sprintf(`{"instance_id":"%s","model":"%s"}`, instanceID, newModel)
	httpReq, _ := http.NewRequest("POST", daemonURL+"/swap", strings.NewReader(swapPayload))
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return result, fmt.Errorf("swap request failed: %v", err)
	}
	resp.Body.Close()

	result.SwapStartTS = nowTS()
	fmt.Printf("[%.1fs] Swap initiated\n", result.SwapStartTS-result.StartTS)

	// Poll for model ready (check health endpoint for new model)
	modelReady := false
	for {
		time.Sleep(3 * time.Second)
		now := nowTS()

		// Check daemon status for this instance
		resp, err := http.Get(daemonURL + "/status")
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var instances []InstanceStatus
		if err := json.Unmarshal(body, &instances); err != nil {
			continue
		}

		var inst *InstanceStatus
		for i := range instances {
			if instances[i].ID == instanceID {
				inst = &instances[i]
				break
			}
		}
		if inst == nil {
			continue
		}

		// Model ready - check health endpoint for NEW model
		if inst.Status == "running" && inst.LocalPort > 0 && !modelReady {
			healthy := checkModelHealth(engine, inst.LocalPort, newModel)
			if healthy {
				result.SwapModelReadyTS = now
				modelReady = true
				fmt.Printf("[%.1fs] Swap: Model ready (health check OK)\n", now-result.StartTS)
			}
		}

		if modelReady {
			result.SwapTotalTS = now
			fmt.Printf("[%.1fs] Swap COMPLETE\n", now-result.StartTS)
			break
		}

		if inst.Status == "failed" {
			return result, fmt.Errorf("instance failed during swap")
		}
	}

	return result, nil
}

// RunShutdownTest tests instance shutdown
func RunShutdownTest(daemonURL, instanceID string) (RunResult, error) {
	result := RunResult{
		InstanceID: instanceID,
		StartTS:    nowTS(),
	}

	fmt.Printf("\n=== SHUTDOWN TEST: %s ===\n", instanceID)

	result.ShutdownStartTS = nowTS()
	stopPayload := fmt.Sprintf(`{"instance_id":"%s"}`, instanceID)
	httpReq, _ := http.NewRequest("POST", daemonURL+"/stop", strings.NewReader(stopPayload))
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return result, fmt.Errorf("shutdown request failed: %v", err)
	}
	resp.Body.Close()

	// Poll until instance is gone from status
	for {
		time.Sleep(2 * time.Second)
		resp, err := http.Get(daemonURL + "/status")
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var instances []InstanceStatus
		if err := json.Unmarshal(body, &instances); err != nil {
			continue
		}

		found := false
		for _, inst := range instances {
			if inst.ID == instanceID {
				found = true
				break
			}
		}

		if !found {
			result.ShutdownDoneTS = nowTS()
			fmt.Printf("[%.1fs] Shutdown COMPLETE (instance destroyed)\n", result.ShutdownDoneTS-result.StartTS)
			break
		}
	}

	return result, nil
}

// RunFirstInferenceTest tests first inference latency
func RunFirstInferenceTest(daemonURL, instanceID, engine, model string) (RunResult, error) {
	result := RunResult{
		InstanceID: instanceID,
		Engine:     engine,
		Model:      model,
		StartTS:    nowTS(),
	}

	fmt.Printf("\n=== FIRST INFERENCE TEST: %s/%s ===\n", engine, model)

	// Find local port
	resp, err := http.Get(daemonURL + "/status")
	if err != nil {
		return result, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var instances []InstanceStatus
	json.Unmarshal(body, &instances)
	var localPort int
	for _, inst := range instances {
		if inst.ID == instanceID {
			localPort = inst.LocalPort
			break
		}
	}
	if localPort == 0 {
		return result, fmt.Errorf("instance not found or no local port")
	}

	// Send first inference request
	var infPayload string
	switch engine {
	case "ollama":
		infPayload = fmt.Sprintf(`{"model":"%s","prompt":"Hello","stream":false}`, model)
	case "vllm":
		infPayload = fmt.Sprintf(`{"model":"%s","messages":[{"role":"user","content":"Hello"}],"stream":false,max_tokens:10}`, model)
	default:
		infPayload = fmt.Sprintf(`{"model":"%s","prompt":"Hello"}`, model)
	}

	inferStart := nowTS()
	httpReq, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/generate", localPort), strings.NewReader(infPayload))
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err = client.Do(httpReq)
	if err != nil {
		return result, fmt.Errorf("inference request failed: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	_ = respBody

	result.FirstInferenceTS = nowTS()
	if resp.StatusCode == 200 {
		result.FirstInferenceOK = true
		fmt.Printf("[%.1fs] First inference OK (latency: %.1fs)\n", result.FirstInferenceTS-result.StartTS, result.FirstInferenceTS-inferStart)
	} else {
		result.FirstInferenceOK = false
		fmt.Printf("[%.1fs] First inference FAILED: %s\n", result.FirstInferenceTS-result.StartTS, string(body))
	}

	return result, nil
}

func avg(vals []float64) float64 {
	sum := 0.0
	for _, v := range vals { sum += v }
	return sum / float64(len(vals))
}

func stddev(vals []float64) float64 {
	if len(vals) <= 1 { return 0 }
	m := avg(vals)
	sum := 0.0
	for _, v := range vals { sum += (v - m) * (v - m) }
	return math.Sqrt(sum / float64(len(vals)-1))
}

func min(vals []float64) float64 {
	m := vals[0]
	for _, v := range vals[1:] { if v < m { m = v } }
	return m
}

func max(vals []float64) float64 {
	m := vals[0]
	for _, v := range vals[1:] { if v > m { m = v } }
	return m
}