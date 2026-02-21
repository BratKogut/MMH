package jupiter

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexus-trading/nexus/internal/adapters"
	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Jupiter DEX Adapter — swap execution on Solana via Jupiter aggregator
// ---------------------------------------------------------------------------

// Config configures the Jupiter adapter.
type Config struct {
	RPCEndpoint    string  `yaml:"rpc_endpoint"`
	WSEndpoint     string  `yaml:"ws_endpoint"`
	WalletKey      string  `yaml:"wallet_key"`      // base58 private key
	DefaultSlipBps int     `yaml:"default_slip_bps"` // default slippage in bps (e.g. 100 = 1%)
	PriorityFee    uint64  `yaml:"priority_fee"`     // priority fee in lamports
	UseJito        bool    `yaml:"use_jito"`
	JitoTipSOL     float64 `yaml:"jito_tip_sol"`
	MaxRetries     int     `yaml:"max_retries"`
}

// DefaultConfig returns development defaults.
func DefaultConfig() Config {
	return Config{
		RPCEndpoint:    "https://api.mainnet-beta.solana.com",
		WSEndpoint:     "wss://api.mainnet-beta.solana.com",
		DefaultSlipBps: 200, // 2% default slippage
		PriorityFee:    100_000,
		UseJito:        false,
		JitoTipSOL:     0.001,
		MaxRetries:     2,
	}
}

// Adapter implements adapters.ExchangeAdapter for Jupiter (Solana DEX).
type Adapter struct {
	config    Config
	rpc       solana.RPCClient
	wallet    solana.Pubkey

	mu        sync.RWMutex
	connected bool

	// Stats.
	swapsExecuted  atomic.Int64
	swapsFailed    atomic.Int64
	totalVolumeSOL atomic.Int64 // stored as lamports
	lastSwapTime   atomic.Int64

	// Callbacks for sniper integration.
	onSwapComplete func(result solana.SwapResult)
}

// New creates a new Jupiter adapter.
func New(config Config, rpc solana.RPCClient, wallet solana.Pubkey) *Adapter {
	return &Adapter{
		config: config,
		rpc:    rpc,
		wallet: wallet,
	}
}

// SetOnSwapComplete sets a callback for completed swaps.
func (a *Adapter) SetOnSwapComplete(fn func(result solana.SwapResult)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onSwapComplete = fn
}

// --- ExchangeAdapter interface ---

func (a *Adapter) Name() string { return "jupiter" }

func (a *Adapter) Connect(ctx context.Context) error {
	if err := a.rpc.Health(ctx); err != nil {
		return fmt.Errorf("jupiter: RPC health check failed: %w", err)
	}
	a.mu.Lock()
	a.connected = true
	a.mu.Unlock()
	log.Info().Str("wallet", string(a.wallet)).Msg("jupiter: connected")
	return nil
}

func (a *Adapter) Disconnect() error {
	a.mu.Lock()
	a.connected = false
	a.mu.Unlock()
	log.Info().Msg("jupiter: disconnected")
	return nil
}

func (a *Adapter) StreamTrades(_ context.Context, _ string) (<-chan bus.Trade, error) {
	return nil, fmt.Errorf("jupiter: StreamTrades not supported for DEX (use scanner)")
}

func (a *Adapter) StreamOrderbook(_ context.Context, _ string) (<-chan bus.OrderbookUpdate, error) {
	return nil, fmt.Errorf("jupiter: StreamOrderbook not supported for DEX")
}

func (a *Adapter) StreamTicks(_ context.Context, _ string) (<-chan bus.Tick, error) {
	return nil, fmt.Errorf("jupiter: StreamTicks not supported for DEX")
}

// SubmitOrder executes a swap on Jupiter.
// The symbol format is "TOKEN_MINT/QUOTE_MINT" or just "TOKEN_MINT" (assumes SOL quote).
func (a *Adapter) SubmitOrder(ctx context.Context, intent bus.OrderIntent) (*adapters.OrderAck, error) {
	start := time.Now()

	// Parse the swap parameters from the order intent.
	params := a.intentToSwapParams(intent)

	log.Info().
		Str("input", string(params.InputMint)).
		Str("output", string(params.OutputMint)).
		Str("amount", params.AmountIn.String()).
		Int("slippage_bps", params.SlippageBps).
		Msg("jupiter: executing swap")

	// Execute the swap.
	result, err := a.executeSwap(ctx, params)
	if err != nil {
		a.swapsFailed.Add(1)
		return &adapters.OrderAck{
			OrderID:   "",
			Status:    "rejected",
			Reason:    err.Error(),
			Timestamp: time.Now().UnixMilli(),
		}, err
	}

	a.swapsExecuted.Add(1)
	a.lastSwapTime.Store(time.Now().UnixMilli())

	// Notify callback.
	a.mu.RLock()
	cb := a.onSwapComplete
	a.mu.RUnlock()
	if cb != nil {
		cb(result)
	}

	log.Info().
		Str("sig", string(result.Signature)).
		Str("amount_out", result.AmountOut.String()).
		Float64("slippage_bps", result.SlippageBps).
		Int64("latency_ms", time.Since(start).Milliseconds()).
		Msg("jupiter: swap executed")

	return &adapters.OrderAck{
		OrderID:   string(result.Signature),
		Status:    "accepted",
		Timestamp: time.Now().UnixMilli(),
	}, nil
}

func (a *Adapter) CancelOrder(_ context.Context, _ string) error {
	return fmt.Errorf("jupiter: cancellation not supported for DEX swaps")
}

func (a *Adapter) GetBalances(ctx context.Context) ([]adapters.Balance, error) {
	bal, err := a.rpc.GetWalletBalance(ctx, a.wallet)
	if err != nil {
		return nil, fmt.Errorf("jupiter: get balance: %w", err)
	}

	balances := []adapters.Balance{
		{
			Asset: "SOL",
			Free:  bal.SOL,
			Total: bal.SOL,
		},
	}

	for mint, amount := range bal.Tokens {
		balances = append(balances, adapters.Balance{
			Asset: string(mint),
			Free:  amount,
			Total: amount,
		})
	}

	return balances, nil
}

func (a *Adapter) GetOpenOrders(_ context.Context, _ string) ([]adapters.Order, error) {
	return nil, nil // DEX swaps are instant, no open orders
}

func (a *Adapter) Health() adapters.HealthStatus {
	a.mu.RLock()
	connected := a.connected
	a.mu.RUnlock()

	state := "disconnected"
	if connected {
		state = "healthy"
	}

	return adapters.HealthStatus{
		Connected:     connected,
		LastHeartbeat: a.lastSwapTime.Load(),
		State:         state,
	}
}

// --- Swap execution ---

// SwapDirect executes a swap directly (bypassing the order intent system).
// Used by the Sniper for maximum speed.
func (a *Adapter) SwapDirect(ctx context.Context, params solana.SwapParams) (solana.SwapResult, error) {
	return a.executeSwap(ctx, params)
}

func (a *Adapter) executeSwap(ctx context.Context, params solana.SwapParams) (solana.SwapResult, error) {
	// In real implementation: build Jupiter swap TX, sign, send via RPC.
	// For now, use the RPC client's transaction sending capability.

	sig, err := a.rpc.SendTransaction(ctx, "jupiter-swap-placeholder")
	if err != nil {
		return solana.SwapResult{
			Error: err.Error(),
		}, err
	}

	// Wait for confirmation.
	status, err := a.rpc.GetTransactionStatus(ctx, sig)
	if err != nil || status == "failed" {
		errMsg := "transaction failed"
		if err != nil {
			errMsg = err.Error()
		}
		return solana.SwapResult{
			Signature: sig,
			Error:     errMsg,
		}, fmt.Errorf("jupiter: %s", errMsg)
	}

	return solana.SwapResult{
		Signature:     sig,
		InputMint:     params.InputMint,
		OutputMint:    params.OutputMint,
		AmountIn:      params.AmountIn,
		AmountOut:     params.AmountIn.Mul(decimal.NewFromFloat(0.98)), // placeholder
		PricePerToken: decimal.NewFromFloat(0.001),                     // placeholder
		FeeSOL:        decimal.NewFromFloat(0.000005),                  // ~5000 lamports
		SlippageBps:   float64(params.SlippageBps),
		Confirmed:     status == "confirmed" || status == "finalized",
	}, nil
}

func (a *Adapter) intentToSwapParams(intent bus.OrderIntent) solana.SwapParams {
	var inputMint, outputMint solana.Pubkey

	if intent.Side == "buy" {
		inputMint = solana.SOLMint
		outputMint = solana.Pubkey(intent.Symbol)
	} else {
		inputMint = solana.Pubkey(intent.Symbol)
		outputMint = solana.SOLMint
	}

	return solana.SwapParams{
		InputMint:     inputMint,
		OutputMint:    outputMint,
		AmountIn:      intent.Qty,
		SlippageBps:   a.config.DefaultSlipBps,
		PriorityFee:   a.config.PriorityFee,
		UseJitoBundle: a.config.UseJito,
		JitoTipSOL:    decimal.NewFromFloat(a.config.JitoTipSOL),
	}
}

// Stats returns adapter statistics.
type JupiterStats struct {
	SwapsExecuted int64 `json:"swaps_executed"`
	SwapsFailed   int64 `json:"swaps_failed"`
	Connected     bool  `json:"connected"`
}

func (a *Adapter) Stats() JupiterStats {
	a.mu.RLock()
	connected := a.connected
	a.mu.RUnlock()

	return JupiterStats{
		SwapsExecuted: a.swapsExecuted.Load(),
		SwapsFailed:   a.swapsFailed.Load(),
		Connected:     connected,
	}
}
