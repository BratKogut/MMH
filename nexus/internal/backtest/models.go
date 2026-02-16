package backtest

import (
	"math"
	"sort"
)

// ---------------------------------------------------------------------------
// Interfaces
// ---------------------------------------------------------------------------

// SlippageModel estimates slippage for a simulated fill.
type SlippageModel interface {
	EstimateSlippage(price float64, qty float64, side string, symbol string) float64
}

// FeeModel estimates trading fees.
type FeeModel interface {
	EstimateFee(price float64, qty float64, side string, exchange string, isMaker bool) float64
}

// ---------------------------------------------------------------------------
// Slippage Models
// ---------------------------------------------------------------------------

// FixedSlippage applies a constant slippage in basis points.
// Buy side: positive slippage (pay more). Sell side: negative (receive less).
// Returns the absolute slippage amount to be added/subtracted from the price.
type FixedSlippage struct {
	BasisPoints float64
}

func (s *FixedSlippage) EstimateSlippage(price, qty float64, side, symbol string) float64 {
	// Slippage amount = price * bps / 10000
	slippageAmt := price * s.BasisPoints / 10000.0
	// buy: positive slippage (pay more), sell: negative (receive less)
	// We return the absolute slippage amount; the caller adds it for buys
	// and subtracts it for sells, or we can encode direction here.
	switch side {
	case "buy":
		return slippageAmt
	case "sell":
		return -slippageAmt
	default:
		return slippageAmt
	}
}

// ProportionalSlippage scales with order size (simple market impact model).
// Total slippage = BaseBps + (ImpactFactor * qty) in basis points.
type ProportionalSlippage struct {
	BaseBps      float64 // base slippage bps
	ImpactFactor float64 // additional bps per unit of qty
}

func (s *ProportionalSlippage) EstimateSlippage(price, qty float64, side, symbol string) float64 {
	totalBps := s.BaseBps + s.ImpactFactor*qty
	slippageAmt := price * totalBps / 10000.0
	switch side {
	case "buy":
		return slippageAmt
	case "sell":
		return -slippageAmt
	default:
		return slippageAmt
	}
}

// OrderbookSlippage uses a simplified orderbook model.
// Simulates walking through the book: half-spread + depth impact.
type OrderbookSlippage struct {
	TypicalSpreadBps float64 // typical bid-ask spread in bps
	DepthFactor      float64 // how quickly price moves with qty (bps per unit)
}

func (s *OrderbookSlippage) EstimateSlippage(price, qty float64, side, symbol string) float64 {
	// Half the spread (crossing the spread) + depth impact from walking the book
	halfSpreadBps := s.TypicalSpreadBps / 2.0
	depthImpactBps := s.DepthFactor * qty
	totalBps := halfSpreadBps + depthImpactBps
	slippageAmt := price * totalBps / 10000.0
	switch side {
	case "buy":
		return slippageAmt
	case "sell":
		return -slippageAmt
	default:
		return slippageAmt
	}
}

// ---------------------------------------------------------------------------
// Fee Models
// ---------------------------------------------------------------------------

// FixedFee applies a fixed percentage fee regardless of volume.
type FixedFee struct {
	MakerFeePct float64 // e.g. 0.001 for 0.1%
	TakerFeePct float64 // e.g. 0.002 for 0.2%
}

func (f *FixedFee) EstimateFee(price, qty float64, side, exchange string, isMaker bool) float64 {
	notional := math.Abs(price * qty)
	if isMaker {
		return notional * f.MakerFeePct
	}
	return notional * f.TakerFeePct
}

// FeeTier defines a single volume-based fee tier.
type FeeTier struct {
	MinVolume float64 // minimum 30-day volume in USD to qualify
	MakerPct  float64 // maker fee percentage
	TakerPct  float64 // taker fee percentage
}

// TieredFee uses volume-based tiers (like Kraken/Binance fee schedules).
// Tiers must be sorted by MinVolume ascending. The highest qualifying tier
// is selected based on Volume30d.
type TieredFee struct {
	Tiers        []FeeTier
	DefaultMaker float64 // fallback maker fee if no tier matches
	DefaultTaker float64 // fallback taker fee if no tier matches
	Volume30d    float64 // 30-day rolling volume in USD
}

func (f *TieredFee) EstimateFee(price, qty float64, side, exchange string, isMaker bool) float64 {
	notional := math.Abs(price * qty)

	makerPct := f.DefaultMaker
	takerPct := f.DefaultTaker

	// Tiers are sorted ascending by MinVolume. Walk them to find the
	// highest tier whose MinVolume we meet.
	for _, tier := range f.Tiers {
		if f.Volume30d >= tier.MinVolume {
			makerPct = tier.MakerPct
			takerPct = tier.TakerPct
		} else {
			break
		}
	}

	if isMaker {
		return notional * makerPct
	}
	return notional * takerPct
}

// ---------------------------------------------------------------------------
// Preconfigured Exchange Models
// ---------------------------------------------------------------------------

// KrakenFeeModel returns a TieredFee configured with Kraken's fee schedule.
// Tiers based on Kraken's 30-day volume tiers (USD).
func KrakenFeeModel() FeeModel {
	tiers := []FeeTier{
		{MinVolume: 0, MakerPct: 0.0016, TakerPct: 0.0026},
		{MinVolume: 50_000, MakerPct: 0.0014, TakerPct: 0.0024},
		{MinVolume: 100_000, MakerPct: 0.0012, TakerPct: 0.0022},
		{MinVolume: 250_000, MakerPct: 0.0010, TakerPct: 0.0020},
		{MinVolume: 500_000, MakerPct: 0.0008, TakerPct: 0.0018},
		{MinVolume: 1_000_000, MakerPct: 0.0006, TakerPct: 0.0016},
		{MinVolume: 2_500_000, MakerPct: 0.0004, TakerPct: 0.0014},
		{MinVolume: 5_000_000, MakerPct: 0.0002, TakerPct: 0.0012},
		{MinVolume: 10_000_000, MakerPct: 0.0, TakerPct: 0.0010},
	}
	// Ensure sorted by MinVolume ascending.
	sort.Slice(tiers, func(i, j int) bool {
		return tiers[i].MinVolume < tiers[j].MinVolume
	})
	return &TieredFee{
		Tiers:        tiers,
		DefaultMaker: 0.0016,
		DefaultTaker: 0.0026,
		Volume30d:    0,
	}
}

// BinanceFeeModel returns a TieredFee configured with Binance's fee schedule.
// Tiers based on Binance's 30-day spot volume tiers (USD).
func BinanceFeeModel() FeeModel {
	tiers := []FeeTier{
		{MinVolume: 0, MakerPct: 0.0010, TakerPct: 0.0010},
		{MinVolume: 1_000_000, MakerPct: 0.0009, TakerPct: 0.0010},
		{MinVolume: 5_000_000, MakerPct: 0.0008, TakerPct: 0.0010},
		{MinVolume: 10_000_000, MakerPct: 0.0007, TakerPct: 0.0009},
		{MinVolume: 50_000_000, MakerPct: 0.0005, TakerPct: 0.0008},
		{MinVolume: 100_000_000, MakerPct: 0.0003, TakerPct: 0.0007},
		{MinVolume: 500_000_000, MakerPct: 0.0002, TakerPct: 0.0006},
		{MinVolume: 1_000_000_000, MakerPct: 0.0001, TakerPct: 0.0005},
	}
	sort.Slice(tiers, func(i, j int) bool {
		return tiers[i].MinVolume < tiers[j].MinVolume
	})
	return &TieredFee{
		Tiers:        tiers,
		DefaultMaker: 0.0010,
		DefaultTaker: 0.0010,
		Volume30d:    0,
	}
}

// DefaultSlippageModel returns a reasonable default slippage model.
// Uses proportional slippage with 2 bps base and 0.1 bps per unit impact.
func DefaultSlippageModel() SlippageModel {
	return &ProportionalSlippage{
		BaseBps:      2.0,
		ImpactFactor: 0.1,
	}
}

// computeRunnerMetrics calculates PerformanceMetrics from trade records and equity curve.
// This is the internal backtest runner's metric computation that produces the
// comprehensive PerformanceMetrics defined in metrics.go.
func computeRunnerMetrics(trades []TradeRecord, equity []EquityPoint, initialCapital float64) PerformanceMetrics {
	// Build trade-level equity for ComputeMetrics.
	m := ComputeMetrics(trades, initialCapital)
	return m
}
