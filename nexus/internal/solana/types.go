package solana

import (
	"time"

	"github.com/shopspring/decimal"
)

// Pubkey is a Solana public key (base58 string).
type Pubkey string

// Signature is a Solana transaction signature.
type Signature string

// ---------------------------------------------------------------------------
// Token & Pool types
// ---------------------------------------------------------------------------

// TokenInfo describes a Solana SPL token.
type TokenInfo struct {
	Mint            Pubkey          `json:"mint"`
	Symbol          string          `json:"symbol"`
	Name            string          `json:"name"`
	Decimals        uint8           `json:"decimals"`
	Supply          decimal.Decimal `json:"supply"`
	MintAuthority   Pubkey          `json:"mint_authority"`    // empty = renounced
	FreezeAuthority Pubkey          `json:"freeze_authority"`  // empty = renounced
	CreatedAt       time.Time       `json:"created_at"`
	MetadataURI     string          `json:"metadata_uri,omitempty"`
}

// IsMintRenounced returns true if the mint authority is empty (good sign).
func (t TokenInfo) IsMintRenounced() bool {
	return t.MintAuthority == ""
}

// IsFreezeRenounced returns true if the freeze authority is empty (good sign).
func (t TokenInfo) IsFreezeRenounced() bool {
	return t.FreezeAuthority == ""
}

// PoolInfo describes a DEX liquidity pool (Raydium AMM, Pump.fun bonding curve, etc).
type PoolInfo struct {
	PoolAddress   Pubkey          `json:"pool_address"`
	DEX           string          `json:"dex"` // raydium|pumpfun|orca|meteora
	TokenMint     Pubkey          `json:"token_mint"`
	QuoteMint     Pubkey          `json:"quote_mint"` // usually SOL or USDC
	TokenReserve  decimal.Decimal `json:"token_reserve"`
	QuoteReserve  decimal.Decimal `json:"quote_reserve"`
	LiquidityUSD  decimal.Decimal `json:"liquidity_usd"`
	PriceUSD      decimal.Decimal `json:"price_usd"`
	Volume24hUSD  decimal.Decimal `json:"volume_24h_usd"`
	CreatedAt     time.Time       `json:"created_at"`
	LPBurned      bool            `json:"lp_burned"`  // LP tokens burned = locked liquidity
	LPLocked      bool            `json:"lp_locked"`  // LP locked in locker contract
	PoolOpenTime  time.Time       `json:"pool_open_time,omitempty"`
}

// IsLiquiditySafe returns true if liquidity is locked or burned.
func (p PoolInfo) IsLiquiditySafe() bool {
	return p.LPBurned || p.LPLocked
}

// HolderInfo describes a token holder.
type HolderInfo struct {
	Address    Pubkey          `json:"address"`
	Balance    decimal.Decimal `json:"balance"`
	Percentage float64         `json:"percentage"` // % of total supply
	IsCreator  bool            `json:"is_creator"`
}

// ---------------------------------------------------------------------------
// Transaction types
// ---------------------------------------------------------------------------

// SwapParams are the parameters for a token swap.
type SwapParams struct {
	InputMint     Pubkey          `json:"input_mint"`
	OutputMint    Pubkey          `json:"output_mint"`
	AmountIn      decimal.Decimal `json:"amount_in"`
	SlippageBps   int             `json:"slippage_bps"` // e.g. 100 = 1%
	PriorityFee   uint64          `json:"priority_fee_lamports"`
	UseJitoBundle bool            `json:"use_jito_bundle"`
	JitoTipSOL    decimal.Decimal `json:"jito_tip_sol,omitempty"`
}

// SwapResult is the result of an executed swap.
type SwapResult struct {
	Signature   Signature       `json:"signature"`
	InputMint   Pubkey          `json:"input_mint"`
	OutputMint  Pubkey          `json:"output_mint"`
	AmountIn    decimal.Decimal `json:"amount_in"`
	AmountOut   decimal.Decimal `json:"amount_out"`
	PricePerToken decimal.Decimal `json:"price_per_token"`
	FeeSOL      decimal.Decimal `json:"fee_sol"`
	SlippageBps float64         `json:"actual_slippage_bps"`
	LatencyMs   int64           `json:"latency_ms"`
	Confirmed   bool            `json:"confirmed"`
	Error       string          `json:"error,omitempty"`
}

// WalletBalance represents the balance of a wallet.
type WalletBalance struct {
	SOL    decimal.Decimal            `json:"sol"`
	Tokens map[Pubkey]decimal.Decimal `json:"tokens"` // mint -> amount
}

// Well-known mints.
const (
	SOLMint  Pubkey = "So11111111111111111111111111111111111111112"
	USDCMint Pubkey = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
)
