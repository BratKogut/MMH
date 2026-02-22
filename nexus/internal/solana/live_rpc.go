package solana

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Live RPC Client — real Solana JSON-RPC with rate limiting & retry
// ---------------------------------------------------------------------------

// LiveRPCClient connects to a real Solana RPC endpoint.
type LiveRPCClient struct {
	config     RPCConfig
	httpClient *http.Client

	// Rate limiter (token bucket).
	limiter    chan struct{}
	limiterCtx context.Context
	limiterCancel context.CancelFunc

	// Unique request ID generator.
	nextID atomic.Int64

	// Circuit breaker.
	consecutiveErrors atomic.Int64
	circuitOpen       atomic.Bool

	// WebSocket state for subscriptions.
	wsURL string

	// Stats.
	requestCount  atomic.Int64
	errorCount    atomic.Int64
	latencySum    atomic.Int64 // cumulative microseconds
	lastRequestAt atomic.Int64
}

const (
	circuitBreakerThreshold = 10 // open after 10 consecutive errors
	circuitBreakerCooldown  = 30 * time.Second
)

// NewLiveRPCClient creates a live Solana RPC client.
func NewLiveRPCClient(config RPCConfig) *LiveRPCClient {
	if config.Timeout == 0 {
		config.Timeout = 10 * time.Second
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 3
	}
	if config.RateLimitRPS == 0 {
		config.RateLimitRPS = 10
	}

	// Token bucket rate limiter.
	bucketSize := int(config.RateLimitRPS)
	if bucketSize < 1 {
		bucketSize = 1
	}
	limiter := make(chan struct{}, bucketSize)
	for i := 0; i < bucketSize; i++ {
		limiter <- struct{}{}
	}

	limiterCtx, limiterCancel := context.WithCancel(context.Background())

	client := &LiveRPCClient{
		config: config,
		httpClient: &http.Client{
			Timeout: config.Timeout,
		},
		limiter:       limiter,
		limiterCtx:    limiterCtx,
		limiterCancel: limiterCancel,
		wsURL:         config.WSEndpoint,
	}

	// Refill tokens at configured RPS.
	go func() {
		interval := time.Duration(float64(time.Second) / config.RateLimitRPS)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-limiterCtx.Done():
				return
			case <-ticker.C:
				select {
				case client.limiter <- struct{}{}:
				default: // bucket full
				}
			}
		}
	}()

	return client
}

// Close shuts down the RPC client.
func (c *LiveRPCClient) Close() {
	c.limiterCancel()
}

// rpcRequest is a JSON-RPC 2.0 request.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC 2.0 response.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// call makes a rate-limited, retried JSON-RPC call.
func (c *LiveRPCClient) call(ctx context.Context, method string, params []any) (json.RawMessage, error) {
	// Circuit breaker check.
	if c.circuitOpen.Load() {
		return nil, fmt.Errorf("rpc: circuit breaker open for %s (too many consecutive errors)", method)
	}

	// Acquire rate limit token.
	select {
	case <-c.limiter:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	reqID := c.nextID.Add(1)

	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      reqID,
		Method:  method,
		Params:  params,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("rpc: marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.config.MaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * 500 * time.Millisecond
			if attempt > 1 {
				backoff = time.Duration(1<<uint(attempt-1)) * time.Second // exponential: 1s, 2s, 4s
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		start := time.Now()

		httpReq, err := http.NewRequestWithContext(ctx, "POST", c.config.Endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("rpc: create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("rpc: %s http error: %w", method, err)
			c.errorCount.Add(1)
			c.recordError()
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("rpc: %s read response: %w", method, err)
			c.errorCount.Add(1)
			c.recordError()
			continue
		}

		latency := time.Since(start)
		c.requestCount.Add(1)
		c.latencySum.Add(latency.Microseconds())
		c.lastRequestAt.Store(time.Now().UnixMilli())

		if resp.StatusCode == 429 {
			lastErr = fmt.Errorf("rpc: %s rate limited (429)", method)
			c.errorCount.Add(1)
			// Longer backoff on 429 - don't count as circuit-breaker error.
			select {
			case <-time.After(time.Duration(2<<uint(attempt)) * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}

		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("rpc: %s HTTP %d: %s", method, resp.StatusCode, string(respBody))
			c.errorCount.Add(1)
			c.recordError()
			continue
		}

		var rpcResp rpcResponse
		if err := json.Unmarshal(respBody, &rpcResp); err != nil {
			lastErr = fmt.Errorf("rpc: %s unmarshal response: %w", method, err)
			c.errorCount.Add(1)
			c.recordError()
			continue
		}

		if rpcResp.Error != nil {
			c.resetErrors()
			return nil, fmt.Errorf("rpc: %s error %d: %s", method, rpcResp.Error.Code, rpcResp.Error.Message)
		}

		// Success - reset circuit breaker.
		c.resetErrors()
		return rpcResp.Result, nil
	}

	return nil, fmt.Errorf("rpc: %s failed after %d attempts: %w", method, c.config.MaxRetries+1, lastErr)
}

// recordError increments consecutive errors and opens circuit breaker if needed.
func (c *LiveRPCClient) recordError() {
	count := c.consecutiveErrors.Add(1)
	if count >= circuitBreakerThreshold {
		if c.circuitOpen.CompareAndSwap(false, true) {
			log.Error().Int64("errors", count).Msg("rpc: CIRCUIT BREAKER OPEN - too many consecutive errors")
			// Auto-reset after cooldown.
			go func() {
				time.Sleep(circuitBreakerCooldown)
				c.circuitOpen.Store(false)
				c.consecutiveErrors.Store(0)
				log.Info().Msg("rpc: circuit breaker reset")
			}()
		}
	}
}

// resetErrors resets the consecutive error counter.
func (c *LiveRPCClient) resetErrors() {
	c.consecutiveErrors.Store(0)
}

// ---------------------------------------------------------------------------
// RPCClient interface implementation
// ---------------------------------------------------------------------------

// GetTokenInfo fetches token metadata via getAccountInfo + Metaplex.
func (c *LiveRPCClient) GetTokenInfo(ctx context.Context, mint Pubkey) (*TokenInfo, error) {
	result, err := c.call(ctx, "getAccountInfo", []any{
		string(mint),
		map[string]any{"encoding": "jsonParsed"},
	})
	if err != nil {
		return nil, err
	}

	var accountResp struct {
		Value *struct {
			Data struct {
				Parsed struct {
					Info struct {
						Decimals        uint8  `json:"decimals"`
						Supply          string `json:"supply"`
						MintAuthority   string `json:"mintAuthority"`
						FreezeAuthority string `json:"freezeAuthority"`
					} `json:"info"`
				} `json:"parsed"`
			} `json:"data"`
		} `json:"value"`
	}

	if err := json.Unmarshal(result, &accountResp); err != nil {
		return nil, fmt.Errorf("rpc: parse token info: %w", err)
	}

	if accountResp.Value == nil {
		return nil, fmt.Errorf("rpc: token %s not found", mint)
	}

	info := accountResp.Value.Data.Parsed.Info
	supply, _ := decimal.NewFromString(info.Supply)

	return &TokenInfo{
		Mint:            mint,
		Decimals:        info.Decimals,
		Supply:          supply,
		MintAuthority:   Pubkey(info.MintAuthority),
		FreezeAuthority: Pubkey(info.FreezeAuthority),
	}, nil
}

// GetTopHolders returns the largest token accounts for a mint.
func (c *LiveRPCClient) GetTopHolders(ctx context.Context, mint Pubkey, limit int) ([]HolderInfo, error) {
	result, err := c.call(ctx, "getTokenLargestAccounts", []any{string(mint)})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Value []struct {
			Address  string  `json:"address"`
			Amount   string  `json:"amount"`
			Decimals uint8   `json:"decimals"`
			UIAmount float64 `json:"uiAmount"`
		} `json:"value"`
	}

	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("rpc: parse holders: %w", err)
	}

	// Get total supply for percentage calculation.
	tokenInfo, tokenErr := c.GetTokenInfo(ctx, mint)
	totalSupply := decimal.Zero
	if tokenErr == nil && tokenInfo != nil && tokenInfo.Supply.IsPositive() {
		totalSupply = tokenInfo.Supply
	}

	holders := make([]HolderInfo, 0, limit)
	for i, h := range resp.Value {
		if i >= limit {
			break
		}
		balance, _ := decimal.NewFromString(h.Amount)
		pct := 0.0
		if totalSupply.IsPositive() {
			pctDec := balance.Div(totalSupply).Mul(decimal.NewFromInt(100))
			pct, _ = pctDec.Float64()
		}
		holders = append(holders, HolderInfo{
			Address:    Pubkey(h.Address),
			Balance:    balance,
			Percentage: pct,
		})
	}

	return holders, nil
}

// GetWalletBalance fetches SOL balance + SPL token accounts.
func (c *LiveRPCClient) GetWalletBalance(ctx context.Context, wallet Pubkey) (*WalletBalance, error) {
	// Get SOL balance.
	solResult, err := c.call(ctx, "getBalance", []any{string(wallet)})
	if err != nil {
		return nil, err
	}

	var balResp struct {
		Value uint64 `json:"value"`
	}
	if err := json.Unmarshal(solResult, &balResp); err != nil {
		return nil, fmt.Errorf("rpc: parse balance: %w", err)
	}

	// Convert lamports to SOL.
	solBalance := decimal.NewFromUint64(balResp.Value).Div(decimal.NewFromInt(1_000_000_000))

	// Get SPL token accounts.
	tokenResult, err := c.call(ctx, "getTokenAccountsByOwner", []any{
		string(wallet),
		map[string]any{"programId": "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
		map[string]any{"encoding": "jsonParsed"},
	})
	if err != nil {
		// Non-fatal: return SOL balance only.
		return &WalletBalance{
			SOL:    solBalance,
			Tokens: make(map[Pubkey]decimal.Decimal),
		}, nil
	}

	var tokenResp struct {
		Value []struct {
			Account struct {
				Data struct {
					Parsed struct {
						Info struct {
							Mint        string `json:"mint"`
							TokenAmount struct {
								UIAmountString string `json:"uiAmountString"`
							} `json:"tokenAmount"`
						} `json:"info"`
					} `json:"parsed"`
				} `json:"data"`
			} `json:"account"`
		} `json:"value"`
	}

	tokens := make(map[Pubkey]decimal.Decimal)
	if err := json.Unmarshal(tokenResult, &tokenResp); err == nil {
		for _, ta := range tokenResp.Value {
			mint := Pubkey(ta.Account.Data.Parsed.Info.Mint)
			amount, _ := decimal.NewFromString(ta.Account.Data.Parsed.Info.TokenAmount.UIAmountString)
			if amount.IsPositive() {
				tokens[mint] = amount
			}
		}
	}

	return &WalletBalance{
		SOL:    solBalance,
		Tokens: tokens,
	}, nil
}

// GetRecentPools fetches recently created pools by scanning program logs.
func (c *LiveRPCClient) GetRecentPools(ctx context.Context, dex string, sinceMinutes int) ([]PoolInfo, error) {
	programID := dexProgramID(dex)
	if programID == "" {
		return nil, fmt.Errorf("rpc: unknown DEX: %s", dex)
	}

	result, err := c.call(ctx, "getSignaturesForAddress", []any{
		programID,
		map[string]any{"limit": 20},
	})
	if err != nil {
		return nil, err
	}

	var sigs []struct {
		Signature string `json:"signature"`
		BlockTime int64  `json:"blockTime"`
	}
	if err := json.Unmarshal(result, &sigs); err != nil {
		return nil, fmt.Errorf("rpc: parse signatures: %w", err)
	}

	cutoff := time.Now().Add(-time.Duration(sinceMinutes) * time.Minute).Unix()
	var pools []PoolInfo
	for _, sig := range sigs {
		if sig.BlockTime < cutoff {
			continue
		}
		pools = append(pools, PoolInfo{
			DEX:       dex,
			CreatedAt: time.Unix(sig.BlockTime, 0),
		})
	}

	return pools, nil
}

// GetPoolInfo fetches pool state by reading the account data.
func (c *LiveRPCClient) GetPoolInfo(ctx context.Context, poolAddress Pubkey) (*PoolInfo, error) {
	result, err := c.call(ctx, "getAccountInfo", []any{
		string(poolAddress),
		map[string]any{"encoding": "base64"},
	})
	if err != nil {
		return nil, err
	}

	var accountResp struct {
		Value *struct {
			Data  []string `json:"data"` // [base64_data, "base64"]
			Owner string   `json:"owner"`
		} `json:"value"`
	}

	if err := json.Unmarshal(result, &accountResp); err != nil {
		return nil, fmt.Errorf("rpc: parse pool info: %w", err)
	}

	if accountResp.Value == nil {
		return nil, fmt.Errorf("rpc: pool %s not found", poolAddress)
	}

	dex := programIDToDEX(accountResp.Value.Owner)

	return &PoolInfo{
		PoolAddress: poolAddress,
		DEX:         dex,
	}, nil
}

// SendTransaction submits a signed transaction.
func (c *LiveRPCClient) SendTransaction(ctx context.Context, txBase64 string) (Signature, error) {
	result, err := c.call(ctx, "sendTransaction", []any{
		txBase64,
		map[string]any{
			"encoding":            "base64",
			"skipPreflight":       false,
			"preflightCommitment": "confirmed",
		},
	})
	if err != nil {
		return "", err
	}

	var sig string
	if err := json.Unmarshal(result, &sig); err != nil {
		return "", fmt.Errorf("rpc: parse signature: %w", err)
	}

	return Signature(sig), nil
}

// GetTransactionStatus checks transaction confirmation status.
func (c *LiveRPCClient) GetTransactionStatus(ctx context.Context, sig Signature) (string, error) {
	result, err := c.call(ctx, "getSignatureStatuses", []any{
		[]string{string(sig)},
	})
	if err != nil {
		return "", err
	}

	var resp struct {
		Value []struct {
			ConfirmationStatus string `json:"confirmationStatus"`
			Err                any    `json:"err"`
		} `json:"value"`
	}

	if err := json.Unmarshal(result, &resp); err != nil {
		return "", fmt.Errorf("rpc: parse status: %w", err)
	}

	if len(resp.Value) == 0 || resp.Value[0].ConfirmationStatus == "" {
		return "pending", nil
	}

	if resp.Value[0].Err != nil {
		return "failed", nil
	}

	return resp.Value[0].ConfirmationStatus, nil
}

// SubscribeNewPools opens a WebSocket subscription for new pool creation.
// Uses HTTP polling as fallback (WS handled by WSMonitor).
func (c *LiveRPCClient) SubscribeNewPools(ctx context.Context, dex string) (<-chan PoolInfo, error) {
	programID := dexProgramID(dex)
	if programID == "" {
		return nil, fmt.Errorf("rpc: unknown DEX: %s", dex)
	}

	out := make(chan PoolInfo, 100)

	go func() {
		defer close(out)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		seen := make(map[string]bool)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pools, err := c.GetRecentPools(ctx, dex, 5)
				if err != nil {
					log.Debug().Err(err).Str("dex", dex).Msg("rpc: poll pools error")
					continue
				}
				for _, pool := range pools {
					key := string(pool.PoolAddress)
					if key == "" || seen[key] {
						continue
					}
					seen[key] = true
					select {
					case out <- pool:
					default:
					}
				}
			}
		}
	}()

	return out, nil
}

// Health checks the RPC endpoint health.
func (c *LiveRPCClient) Health(ctx context.Context) error {
	healthCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.call(healthCtx, "getHealth", nil)
	return err
}

// RPCStats returns RPC client statistics.
type RPCStats struct {
	RequestCount    int64 `json:"request_count"`
	ErrorCount      int64 `json:"error_count"`
	AvgLatencyUs    int64 `json:"avg_latency_us"`
	LastRequestAt   int64 `json:"last_request_at"`
	CircuitOpen     bool  `json:"circuit_open"`
	ConsecErrors    int64 `json:"consecutive_errors"`
}

func (c *LiveRPCClient) Stats() RPCStats {
	reqCount := c.requestCount.Load()
	avgLatency := int64(0)
	if reqCount > 0 {
		avgLatency = c.latencySum.Load() / reqCount
	}
	return RPCStats{
		RequestCount:  reqCount,
		ErrorCount:    c.errorCount.Load(),
		AvgLatencyUs:  avgLatency,
		LastRequestAt: c.lastRequestAt.Load(),
		CircuitOpen:   c.circuitOpen.Load(),
		ConsecErrors:  c.consecutiveErrors.Load(),
	}
}

// ---------------------------------------------------------------------------
// DEX program ID mappings
// ---------------------------------------------------------------------------

// Known DEX program IDs on Solana mainnet.
var dexPrograms = map[string]string{
	"raydium":  "675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8", // Raydium AMM V4
	"pumpfun":  "6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P",  // Pump.fun
	"orca":     "whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc",  // Orca Whirlpool
	"meteora":  "LBUZKhRxPF3XUpBCjp4YzTKgLccjZhTSDM9YuVaPwxo",  // Meteora DLMM
}

var programToDEX map[string]string

func init() {
	programToDEX = make(map[string]string, len(dexPrograms))
	for dex, pid := range dexPrograms {
		programToDEX[pid] = dex
	}
}

func dexProgramID(dex string) string {
	return dexPrograms[dex]
}

func programIDToDEX(programID string) string {
	if dex, ok := programToDEX[programID]; ok {
		return dex
	}
	return "unknown"
}

// mu protects wsMonitor.
var wsMonitorMu sync.Mutex
