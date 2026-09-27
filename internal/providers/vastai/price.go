package vastai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// OfferPrice consulta o dph_total atual de uma oferta (reconferência de preço
// imediatamente antes do PUT /asks/{id}).
func (c *Client) OfferPrice(ctx context.Context, machineID, apiKey string) (float64, error) {
	id, err := strconv.ParseInt(machineID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid offer id %q", machineID)
	}
	body, _ := json.Marshal(map[string]interface{}{
		"id":    map[string]interface{}{"eq": id},
		"limit": 1,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://console.vast.ai/api/v0/bundles/", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return 0, fmt.Errorf("vast.ai offer lookup returned status %d: %s", resp.StatusCode, b)
	}
	var br BundlesResponse
	if err := json.NewDecoder(resp.Body).Decode(&br); err != nil {
		return 0, err
	}
	if len(br.Offers) == 0 {
		return 0, fmt.Errorf("offer %s is no longer available", machineID)
	}
	return br.Offers[0].DphTotal, nil
}
