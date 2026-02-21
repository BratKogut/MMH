package solana

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Jito Bundle Client — MEV protection via bundles with tips
// https://jito-labs.gitbook.io/mev/
// ---------------------------------------------------------------------------

const (
	// Jito Block Engine endpoints (mainnet).
	jitoMainnetURL    = "https://mainnet.block-engine.jito.wtf/api/v1"
	jitoBundleURL     = "/bundles"
	jitoTipAccountURL = "/bundles/tip_accounts"

	// Known Jito tip accounts (mainnet).
	jitoTipAccount1 = "96gYZGLnJYVFmbjzopPSU6QiEV5fGqZNyN9nmNhvrZU5"
	jitoTipAccount2 = "HFqU5x63VTqvQss8hp11i4bVqkfRtQ7NmXwkiY8X9W5E"
	jitoTipAccount3 = "Cw8CFyM9FkoMi7K7Crf6HNQqf4uEMzpKw6QNghXLvLkY"
	jitoTipAccount4 = "ADaUMid9yfUytqMBgopwjb2DTLSLuiv3Jhqzsg1dbE7B"
	jitoTipAccount5 = "DfXygSm4jCyNCzbzYYR18MFJkvDVwVS7s3d7rZmLhRDd"
	jitoTipAccount6 = "ADuUkR4vqLUMWXxW9gh6D6L8pMSawimctcNZ5pGwDcEt"
	jitoTipAccount7 = "DttWaMuVvTiduZRnguLF7jNxTgiMBZ1hyAumKUiL2KRL"
	jitoTipAccount8 = "3AVi9Tg9Uo68tJfuvoKvqKNWKkC5wPdSSdeBnizKZ6jT"
)

// JitoConfig configures the Jito bundle client.
type JitoConfig struct {
	Enabled       bool            `yaml:"enabled"`
	BlockEngineURL string         `yaml:"block_engine_url"`
	TipSOL        decimal.Decimal `yaml:"tip_sol"`        // tip per bundle in SOL
	MaxBundles    int             `yaml:"max_bundles"`     // max concurrent bundles
	TimeoutMs     int             `yaml:"timeout_ms"`
}

// DefaultJitoConfig returns production defaults.
func DefaultJitoConfig() JitoConfig {
	return JitoConfig{
		Enabled:        false,
		BlockEngineURL: jitoMainnetURL,
		TipSOL:         decimal.NewFromFloat(0.001), // 0.001 SOL tip
		MaxBundles:     5,
		TimeoutMs:      5000,
	}
}

// JitoClient sends transaction bundles through Jito for MEV protection.
type JitoClient struct {
	config     JitoConfig
	httpClient *http.Client
	tipAcctIdx atomic.Uint32 // round-robin tip account selection

	// Stats.
	bundlesSent    atomic.Int64
	bundlesLanded  atomic.Int64
	bundlesFailed  atomic.Int64
	totalTipSOL    atomic.Int64 // stored as lamports for atomic
}

// NewJitoClient creates a new Jito bundle client.
func NewJitoClient(config JitoConfig) *JitoClient {
	timeout := time.Duration(config.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	return &JitoClient{
		config: config,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// BundleRequest is a Jito bundle submission request.
type BundleRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

// BundleResponse is the response from Jito bundle submission.
type BundleResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  string `json:"result,omitempty"` // bundle ID
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// BundleStatus tracks the state of a submitted bundle.
type BundleStatus struct {
	BundleID  string `json:"bundle_id"`
	Status    string `json:"status"` // pending|landed|failed
	Slot      uint64 `json:"slot,omitempty"`
	Timestamp int64  `json:"timestamp"`
}

// SendBundle submits a transaction bundle to Jito.
// transactions is a list of base64-encoded signed transactions.
func (c *JitoClient) SendBundle(ctx context.Context, transactions []string) (*BundleStatus, error) {
	if !c.config.Enabled {
		return nil, fmt.Errorf("jito: bundles not enabled")
	}

	if len(transactions) == 0 {
		return nil, fmt.Errorf("jito: empty bundle")
	}

	req := BundleRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "sendBundle",
		Params:  []any{transactions},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("jito: marshal request: %w", err)
	}

	url := c.config.BlockEngineURL + jitoBundleURL
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("jito: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		c.bundlesFailed.Add(1)
		return nil, fmt.Errorf("jito: HTTP error: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.bundlesFailed.Add(1)
		return nil, fmt.Errorf("jito: read response: %w", err)
	}

	if resp.StatusCode != 200 {
		c.bundlesFailed.Add(1)
		return nil, fmt.Errorf("jito: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var bundleResp BundleResponse
	if err := json.Unmarshal(respBody, &bundleResp); err != nil {
		c.bundlesFailed.Add(1)
		return nil, fmt.Errorf("jito: parse response: %w", err)
	}

	if bundleResp.Error != nil {
		c.bundlesFailed.Add(1)
		return nil, fmt.Errorf("jito: error %d: %s", bundleResp.Error.Code, bundleResp.Error.Message)
	}

	c.bundlesSent.Add(1)
	tipLamports := c.config.TipSOL.Mul(decimal.NewFromInt(1_000_000_000)).IntPart()
	c.totalTipSOL.Add(tipLamports)

	log.Info().
		Str("bundle_id", bundleResp.Result).
		Str("tip_sol", c.config.TipSOL.String()).
		Int("tx_count", len(transactions)).
		Msg("jito: bundle submitted")

	return &BundleStatus{
		BundleID:  bundleResp.Result,
		Status:    "pending",
		Timestamp: time.Now().UnixMilli(),
	}, nil
}

// GetBundleStatus checks the status of a submitted bundle.
func (c *JitoClient) GetBundleStatus(ctx context.Context, bundleID string) (*BundleStatus, error) {
	req := BundleRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "getBundleStatuses",
		Params:  []any{[]string{bundleID}},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("jito: marshal request: %w", err)
	}

	url := c.config.BlockEngineURL + jitoBundleURL
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("jito: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("jito: HTTP error: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("jito: read response: %w", err)
	}

	var statusResp struct {
		Result struct {
			Value []struct {
				BundleID           string `json:"bundle_id"`
				ConfirmationStatus string `json:"confirmation_status"`
				Slot               uint64 `json:"slot"`
				Err                any    `json:"err"`
			} `json:"value"`
		} `json:"result"`
	}

	if err := json.Unmarshal(respBody, &statusResp); err != nil {
		return nil, fmt.Errorf("jito: parse status: %w", err)
	}

	if len(statusResp.Result.Value) == 0 {
		return &BundleStatus{BundleID: bundleID, Status: "pending"}, nil
	}

	entry := statusResp.Result.Value[0]
	status := "pending"
	if entry.ConfirmationStatus == "confirmed" || entry.ConfirmationStatus == "finalized" {
		status = "landed"
		c.bundlesLanded.Add(1)
	}
	if entry.Err != nil {
		status = "failed"
		c.bundlesFailed.Add(1)
	}

	return &BundleStatus{
		BundleID:  bundleID,
		Status:    status,
		Slot:      entry.Slot,
		Timestamp: time.Now().UnixMilli(),
	}, nil
}

// NextTipAccount returns the next tip account (round-robin).
func (c *JitoClient) NextTipAccount() Pubkey {
	accounts := []Pubkey{
		Pubkey(jitoTipAccount1),
		Pubkey(jitoTipAccount2),
		Pubkey(jitoTipAccount3),
		Pubkey(jitoTipAccount4),
		Pubkey(jitoTipAccount5),
		Pubkey(jitoTipAccount6),
		Pubkey(jitoTipAccount7),
		Pubkey(jitoTipAccount8),
	}
	idx := c.tipAcctIdx.Add(1) - 1
	return accounts[idx%uint32(len(accounts))]
}

// JitoStats returns Jito client statistics.
type JitoStats struct {
	Enabled       bool   `json:"enabled"`
	BundlesSent   int64  `json:"bundles_sent"`
	BundlesLanded int64  `json:"bundles_landed"`
	BundlesFailed int64  `json:"bundles_failed"`
	LandRate      float64 `json:"land_rate_pct"`
	TotalTipSOL   string `json:"total_tip_sol"`
}

func (c *JitoClient) Stats() JitoStats {
	sent := c.bundlesSent.Load()
	landed := c.bundlesLanded.Load()
	landRate := 0.0
	if sent > 0 {
		landRate = float64(landed) / float64(sent) * 100.0
	}

	tipLamports := c.totalTipSOL.Load()
	tipSOL := decimal.NewFromInt(tipLamports).Div(decimal.NewFromInt(1_000_000_000))

	return JitoStats{
		Enabled:       c.config.Enabled,
		BundlesSent:   sent,
		BundlesLanded: landed,
		BundlesFailed: c.bundlesFailed.Load(),
		LandRate:      landRate,
		TotalTipSOL:   tipSOL.String(),
	}
}
