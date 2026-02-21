package solana

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJitoClient_SendBundle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "bundle-id-12345",
		})
	}))
	defer server.Close()

	config := DefaultJitoConfig()
	config.Enabled = true
	config.BlockEngineURL = server.URL
	client := NewJitoClient(config)

	status, err := client.SendBundle(context.Background(), []string{"tx1-base64", "tx2-base64"})
	require.NoError(t, err)
	assert.Equal(t, "bundle-id-12345", status.BundleID)
	assert.Equal(t, "pending", status.Status)

	stats := client.Stats()
	assert.Equal(t, int64(1), stats.BundlesSent)
}

func TestJitoClient_SendBundle_Disabled(t *testing.T) {
	config := DefaultJitoConfig()
	config.Enabled = false
	client := NewJitoClient(config)

	_, err := client.SendBundle(context.Background(), []string{"tx1"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not enabled")
}

func TestJitoClient_SendBundle_EmptyBundle(t *testing.T) {
	config := DefaultJitoConfig()
	config.Enabled = true
	client := NewJitoClient(config)

	_, err := client.SendBundle(context.Background(), []string{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty bundle")
}

func TestJitoClient_GetBundleStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"value": []map[string]any{
					{
						"bundle_id":           "bundle-123",
						"confirmation_status": "confirmed",
						"slot":                uint64(12345),
					},
				},
			},
		})
	}))
	defer server.Close()

	config := DefaultJitoConfig()
	config.BlockEngineURL = server.URL
	client := NewJitoClient(config)

	status, err := client.GetBundleStatus(context.Background(), "bundle-123")
	require.NoError(t, err)
	assert.Equal(t, "landed", status.Status)
	assert.Equal(t, uint64(12345), status.Slot)
}

func TestJitoClient_NextTipAccount_RoundRobin(t *testing.T) {
	config := DefaultJitoConfig()
	client := NewJitoClient(config)

	first := client.NextTipAccount()
	second := client.NextTipAccount()
	assert.NotEqual(t, first, second, "Should rotate tip accounts")

	// Cycle through all 8 accounts.
	seen := make(map[Pubkey]bool)
	seen[first] = true
	seen[second] = true
	for i := 0; i < 6; i++ {
		seen[client.NextTipAccount()] = true
	}
	assert.Len(t, seen, 8, "Should cycle through all 8 tip accounts")
}

func TestJitoClient_Stats(t *testing.T) {
	config := DefaultJitoConfig()
	config.TipSOL = decimal.NewFromFloat(0.001)
	client := NewJitoClient(config)

	stats := client.Stats()
	assert.False(t, stats.Enabled)
	assert.Equal(t, int64(0), stats.BundlesSent)
	assert.Equal(t, "0", stats.TotalTipSOL)
}

func TestJitoClient_SendBundle_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"error": map[string]any{
				"code":    -32000,
				"message": "Bundle simulation failed",
			},
		})
	}))
	defer server.Close()

	config := DefaultJitoConfig()
	config.Enabled = true
	config.BlockEngineURL = server.URL
	client := NewJitoClient(config)

	_, err := client.SendBundle(context.Background(), []string{"tx1"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "simulation failed")

	stats := client.Stats()
	assert.Equal(t, int64(1), stats.BundlesFailed)
}
