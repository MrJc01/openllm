package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	instanceID := flag.String("instance-id", "", "Vast.ai Instance/Contract ID to destroy if timeout is reached")
	apiKey := flag.String("api-key", "", "Vast.ai API Key")
	pingURL := flag.String("ping-url", "http://localhost:17291/ping", "URL to ping through the reverse tunnel")
	interval := flag.Duration("interval", 30*time.Second, "Ping interval")
	timeout := flag.Duration("timeout", 5*time.Minute, "Max duration allowed without a successful ping")
	flag.Parse()

	if *instanceID == "" || *apiKey == "" {
		log.Fatalf("Error: --instance-id and --api-key are required")
	}

	log.Printf("Starting openllm-watchdog...")
	log.Printf("Monitoring: %s", *pingURL)
	log.Printf("Interval: %v, Timeout: %v", *interval, *timeout)
	log.Printf("Target Instance ID: %s", *instanceID)

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	lastSuccess := time.Now()
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for range ticker.C {
		log.Printf("Pinging %s...", *pingURL)
		req, err := http.NewRequest("GET", *pingURL, nil)
		if err != nil {
			log.Printf("Failed to create request: %v", err)
			checkTimeout(lastSuccess, *timeout, *instanceID, *apiKey)
			continue
		}

		// Pass watchdog header just to be identifiable
		req.Header.Set("User-Agent", "openllm-watchdog/1.0")

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Ping error: %v", err)
			checkTimeout(lastSuccess, *timeout, *instanceID, *apiKey)
			continue
		}
		
		body, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			log.Printf("Ping OK: %s", string(body))
			lastSuccess = time.Now()
		} else {
			log.Printf("Ping failed with status %d: %s", resp.StatusCode, string(body))
			checkTimeout(lastSuccess, *timeout, *instanceID, *apiKey)
		}
	}
}

func checkTimeout(lastSuccess time.Time, timeout time.Duration, instanceID, apiKey string) {
	timeSinceLastSuccess := time.Since(lastSuccess)
	log.Printf("Time since last successful ping: %v / %v max", timeSinceLastSuccess, timeout)

	if timeSinceLastSuccess >= timeout {
		log.Printf("CRITICAL: Timeout reached! Initiating self-destruct for instance %s...", instanceID)
		
		url := fmt.Sprintf("https://console.vast.ai/api/v0/instances/%s/", instanceID)
		req, err := http.NewRequest("DELETE", url, nil)
		if err != nil {
			log.Printf("Failed to create self-destruct request: %v", err)
			os.Exit(1)
		}
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Failed to send self-destruct request: %v", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		body, _ := ioutil.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusOK {
			log.Printf("SUCCESS: Self-destruct request accepted! Response: %s", string(body))
			os.Exit(0)
		} else {
			log.Printf("ERROR: Self-destruct request failed with status %d: %s", resp.StatusCode, string(body))
			os.Exit(1)
		}
	}
}
