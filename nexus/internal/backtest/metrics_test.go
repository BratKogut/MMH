package backtest

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const floatTol = 1e-9

// Helper to make timestamps at day offsets from a base.
func dayTime(dayOffset int) time.Time {
	base := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(dayOffset) * 24 * time.Hour)
}

func TestSharpeFromReturns_KnownValues(t *testing.T) {
	// Known returns: [0.01, 0.02, -0.01, 0.03, 0.005]
	// mean = (0.01 + 0.02 - 0.01 + 0.03 + 0.005) / 5 = 0.055/5 = 0.011
	// sample std: variance = [(0.01-0.011)^2 + (0.02-0.011)^2 + (-0.01-0.011)^2 + (0.03-0.011)^2 + (0.005-0.011)^2] / 4
	//   = [0.000001 + 0.000081 + 0.000441 + 0.000361 + 0.000036] / 4
	//   = 0.00092 / 4 = 0.00023
	// std = sqrt(0.00023) = 0.015165750888...
	// Sharpe = (0.011 / 0.015165750888) * sqrt(252) = 0.72528... * 15.8745... = 11.513...

	returns := []float64{0.01, 0.02, -0.01, 0.03, 0.005}
	sharpe := SharpeFromReturns(returns, 252.0)

	expectedMean := 0.011
	expectedVar := (math.Pow(0.01-expectedMean, 2) + math.Pow(0.02-expectedMean, 2) +
		math.Pow(-0.01-expectedMean, 2) + math.Pow(0.03-expectedMean, 2) +
		math.Pow(0.005-expectedMean, 2)) / 4.0
	expectedStd := math.Sqrt(expectedVar)
	expectedSharpe := (expectedMean / expectedStd) * math.Sqrt(252.0)

	assert.InDelta(t, expectedSharpe, sharpe, 1e-6, "Sharpe ratio mismatch")
}

func TestSharpeFromReturns_AllPositive(t *testing.T) {
	returns := []float64{0.01, 0.01, 0.01}
	// Mean = 0.01, std = 0 -> Sharpe should be 0 (undefined).
	sharpe := SharpeFromReturns(returns, 252.0)
	assert.Equal(t, 0.0, sharpe, "constant returns should yield Sharpe=0")
}

func TestSharpeFromReturns_InsufficientData(t *testing.T) {
	assert.Equal(t, 0.0, SharpeFromReturns(nil, 252))
	assert.Equal(t, 0.0, SharpeFromReturns([]float64{0.01}, 252))
}

func TestSortinoFromReturns_KnownValues(t *testing.T) {
	// returns: [0.01, -0.02, 0.03, -0.01, 0.02]
	// mean = (0.01 - 0.02 + 0.03 - 0.01 + 0.02) / 5 = 0.03/5 = 0.006
	// downside: only -0.02 and -0.01 contribute
	// semi_var = ((-0.02)^2 + (-0.01)^2) / (5-1) = (0.0004 + 0.0001) / 4 = 0.000125
	// downside_dev = sqrt(0.000125) = 0.01118033988749895
	// Sortino = 0.006 / 0.01118033988749895 * sqrt(252)

	returns := []float64{0.01, -0.02, 0.03, -0.01, 0.02}
	sortino := SortinoFromReturns(returns, 252.0)

	expectedMean := 0.006
	expectedDownDev := math.Sqrt((0.0004 + 0.0001) / 4.0)
	expectedSortino := (expectedMean / expectedDownDev) * math.Sqrt(252.0)

	assert.InDelta(t, expectedSortino, sortino, 1e-6)
}

func TestSortinoFromReturns_NoNegatives(t *testing.T) {
	returns := []float64{0.01, 0.02, 0.005}
	sortino := SortinoFromReturns(returns, 252.0)
	assert.Equal(t, 0.0, sortino, "no negative returns -> Sortino=0 (undefined)")
}

func TestMaxDrawdownFromEquity_KnownCurve(t *testing.T) {
	// Equity: 100, 110, 105, 115, 90, 120
	// Peak progression: 100, 110, 110, 115, 115, 120
	// Drawdowns:          0,   0,   5,   0,  25,   0
	// Max DD = 25 at equity=90 (peak was 115)
	// Max DD pct = 25/115 = 0.2173913...

	equity := []float64{100, 110, 105, 115, 90, 120}
	dd, ddPct := MaxDrawdownFromEquity(equity)

	assert.InDelta(t, 25.0, dd, floatTol)
	assert.InDelta(t, 25.0/115.0, ddPct, floatTol)
}

func TestMaxDrawdownFromEquity_MonotonicallyIncreasing(t *testing.T) {
	equity := []float64{100, 110, 120, 130, 140}
	dd, ddPct := MaxDrawdownFromEquity(equity)

	assert.Equal(t, 0.0, dd)
	assert.Equal(t, 0.0, ddPct)
}

func TestMaxDrawdownFromEquity_MonotonicallyDecreasing(t *testing.T) {
	equity := []float64{100, 90, 80, 70}
	dd, ddPct := MaxDrawdownFromEquity(equity)

	assert.InDelta(t, 30.0, dd, floatTol)
	assert.InDelta(t, 0.3, ddPct, floatTol)
}

func TestMaxDrawdownFromEquity_Empty(t *testing.T) {
	dd, ddPct := MaxDrawdownFromEquity(nil)
	assert.Equal(t, 0.0, dd)
	assert.Equal(t, 0.0, ddPct)

	dd, ddPct = MaxDrawdownFromEquity([]float64{100})
	assert.Equal(t, 0.0, dd)
	assert.Equal(t, 0.0, ddPct)
}

func TestProfitFactorFromTrades(t *testing.T) {
	trades := []TradeRecord{
		{PnL: 100, Fees: 0, Slippage: 0}, // +100
		{PnL: -50, Fees: 0, Slippage: 0}, // -50
		{PnL: 200, Fees: 0, Slippage: 0}, // +200
		{PnL: -30, Fees: 0, Slippage: 0}, // -30
	}

	// Gross profit = 300, gross loss = 80, PF = 300/80 = 3.75
	pf := ProfitFactorFromTrades(trades)
	assert.InDelta(t, 3.75, pf, floatTol)
}

func TestProfitFactorFromTrades_NoLosses(t *testing.T) {
	trades := []TradeRecord{
		{PnL: 100, Fees: 0, Slippage: 0},
		{PnL: 50, Fees: 0, Slippage: 0},
	}

	pf := ProfitFactorFromTrades(trades)
	assert.True(t, math.IsInf(pf, 1), "no losses should return +Inf")
}

func TestProfitFactorFromTrades_NoWins(t *testing.T) {
	trades := []TradeRecord{
		{PnL: -100, Fees: 0, Slippage: 0},
		{PnL: -50, Fees: 0, Slippage: 0},
	}

	pf := ProfitFactorFromTrades(trades)
	assert.Equal(t, 0.0, pf)
}

func TestProfitFactorFromTrades_Empty(t *testing.T) {
	assert.Equal(t, 0.0, ProfitFactorFromTrades(nil))
}

func TestProfitFactorWithCosts(t *testing.T) {
	// A trade that is profitable gross but not net.
	trades := []TradeRecord{
		{PnL: 10, Fees: 5, Slippage: 3}, // net = 10 - 5 - 3 = 2 (win)
		{PnL: -8, Fees: 1, Slippage: 1}, // net = -8 - 1 - 1 = -10 (loss)
	}

	// gross_profit = 2, gross_loss = 10, PF = 0.2
	pf := ProfitFactorFromTrades(trades)
	assert.InDelta(t, 0.2, pf, floatTol)
}

func TestComputeMetrics_EmptyTrades(t *testing.T) {
	m := ComputeMetrics(nil, 10000)
	assert.Equal(t, 0, m.TradeCount)
	assert.Equal(t, 0.0, m.TotalPnL)
	assert.Equal(t, 0.0, m.NetPnL)
}

func TestComputeMetrics_ZeroCapital(t *testing.T) {
	trades := []TradeRecord{
		{PnL: 100, Fees: 0, Slippage: 0, EntryTime: dayTime(0), ExitTime: dayTime(1)},
	}
	m := ComputeMetrics(trades, 0)
	assert.Equal(t, 0, m.TradeCount)
}

func TestComputeMetrics_SingleWinningTrade(t *testing.T) {
	trades := []TradeRecord{
		{
			Symbol:     "BTC-USD",
			Side:       "buy",
			EntryPrice: 50000,
			ExitPrice:  51000,
			Qty:        0.1,
			PnL:        100,
			Fees:       2,
			Slippage:   1,
			EntryTime:  dayTime(0),
			ExitTime:   dayTime(1),
		},
	}

	m := ComputeMetrics(trades, 10000)

	assert.Equal(t, 1, m.TradeCount)
	assert.InDelta(t, 100.0, m.TotalPnL, floatTol)
	assert.InDelta(t, 97.0, m.NetPnL, floatTol) // 100 - 2 - 1
	assert.InDelta(t, 2.0, m.TotalFees, floatTol)
	assert.InDelta(t, 1.0, m.TotalSlippage, floatTol)
	assert.InDelta(t, 1.0, m.WinRate, floatTol) // 1/1
	assert.InDelta(t, 97.0, m.AvgTrade, floatTol)
	assert.InDelta(t, 97.0, m.AvgWin, floatTol)
	assert.InDelta(t, 97.0, m.LargestWin, floatTol)
	assert.Equal(t, 0.0, m.LargestLoss) // no losses
	assert.Equal(t, 24*time.Hour, m.HoldingPeriodAvg)
}

func TestComputeMetrics_RealisticSequence(t *testing.T) {
	trades := []TradeRecord{
		{Symbol: "BTC-USD", Side: "buy", EntryPrice: 50000, ExitPrice: 51000, Qty: 0.1,
			PnL: 100, Fees: 5, Slippage: 2, EntryTime: dayTime(0), ExitTime: dayTime(2)},
		{Symbol: "BTC-USD", Side: "sell", EntryPrice: 51000, ExitPrice: 50500, Qty: 0.1,
			PnL: 50, Fees: 5, Slippage: 2, EntryTime: dayTime(3), ExitTime: dayTime(4)},
		{Symbol: "ETH-USD", Side: "buy", EntryPrice: 3000, ExitPrice: 2900, Qty: 1.0,
			PnL: -100, Fees: 3, Slippage: 1, EntryTime: dayTime(5), ExitTime: dayTime(7)},
		{Symbol: "BTC-USD", Side: "buy", EntryPrice: 50500, ExitPrice: 52000, Qty: 0.2,
			PnL: 300, Fees: 8, Slippage: 3, EntryTime: dayTime(8), ExitTime: dayTime(10)},
		{Symbol: "ETH-USD", Side: "sell", EntryPrice: 2900, ExitPrice: 2950, Qty: 1.0,
			PnL: -50, Fees: 3, Slippage: 1, EntryTime: dayTime(11), ExitTime: dayTime(12)},
	}

	m := ComputeMetrics(trades, 10000)

	require.Equal(t, 5, m.TradeCount)

	// Total PnL = 100 + 50 - 100 + 300 - 50 = 300
	assert.InDelta(t, 300.0, m.TotalPnL, floatTol)

	// Total Fees = 5 + 5 + 3 + 8 + 3 = 24
	assert.InDelta(t, 24.0, m.TotalFees, floatTol)

	// Total Slippage = 2 + 2 + 1 + 3 + 1 = 9
	assert.InDelta(t, 9.0, m.TotalSlippage, floatTol)

	// Net PnL = 300 - 24 - 9 = 267
	assert.InDelta(t, 267.0, m.NetPnL, floatTol)

	// Net per trade: [93, 43, -104, 289, -54]
	// Wins: 93, 43, 289 (3 trades) -> winRate = 3/5 = 0.6
	assert.InDelta(t, 0.6, m.WinRate, floatTol)

	// AvgTrade = 267 / 5 = 53.4
	assert.InDelta(t, 53.4, m.AvgTrade, floatTol)

	// AvgWin = (93 + 43 + 289) / 3 = 425/3 = 141.666...
	assert.InDelta(t, 425.0/3.0, m.AvgWin, 1e-4)

	// AvgLoss = (-104 + -54) / 2 = -79
	assert.InDelta(t, -79.0, m.AvgLoss, floatTol)

	// LargestWin = 289
	assert.InDelta(t, 289.0, m.LargestWin, floatTol)

	// LargestLoss = -104
	assert.InDelta(t, -104.0, m.LargestLoss, floatTol)

	// ProfitFactor = (93 + 43 + 289) / (104 + 54) = 425 / 158 = 2.6898...
	assert.InDelta(t, 425.0/158.0, m.ProfitFactor, 1e-4)

	// Cost impact = 33 / 300 = 0.11
	assert.InDelta(t, 33.0/300.0, m.CostImpact, floatTol)

	// Verify drawdown is computed (exact value depends on equity curve).
	// Equity: 10000, 10093, 10136, 10032, 10321, 10267
	// Peak:   10000, 10093, 10136, 10136, 10321, 10321
	// DD:         0,     0,     0,   104,     0,    54
	// Max DD = 104, at equity 10032 (peak was 10136)
	assert.InDelta(t, 104.0, m.MaxDrawdown, floatTol)
	assert.InDelta(t, 104.0/10136.0, m.MaxDrawdownPct, 1e-6)

	// Average holding period: (2 + 1 + 2 + 2 + 1) days = 8 days / 5 = 1.6 days
	expectedAvgHold := time.Duration(int64(8*24*time.Hour) / 5)
	assert.Equal(t, expectedAvgHold, m.HoldingPeriodAvg)
}

func TestComputeMetrics_Turnover(t *testing.T) {
	trades := []TradeRecord{
		{Symbol: "BTC-USD", EntryPrice: 50000, Qty: 0.2,
			PnL: 100, EntryTime: dayTime(0), ExitTime: dayTime(1)},
		{Symbol: "BTC-USD", EntryPrice: 51000, Qty: 0.1,
			PnL: 50, EntryTime: dayTime(2), ExitTime: dayTime(3)},
	}

	m := ComputeMetrics(trades, 10000)

	// Volume: |0.2 * 50000| + |0.1 * 51000| = 10000 + 5100 = 15100
	// Turnover = 15100 / 10000 = 1.51
	assert.InDelta(t, 1.51, m.Turnover, floatTol)
}

func TestComputeMetrics_AllLosers(t *testing.T) {
	trades := []TradeRecord{
		{PnL: -100, Fees: 5, Slippage: 2, EntryPrice: 100, Qty: 1,
			EntryTime: dayTime(0), ExitTime: dayTime(1)},
		{PnL: -50, Fees: 3, Slippage: 1, EntryPrice: 100, Qty: 1,
			EntryTime: dayTime(2), ExitTime: dayTime(3)},
	}

	m := ComputeMetrics(trades, 10000)

	assert.Equal(t, 0.0, m.WinRate)
	assert.Equal(t, 0.0, m.ProfitFactor)
	assert.Equal(t, 0.0, m.LargestWin)
	assert.True(t, m.LargestLoss < 0)
	assert.True(t, m.MaxDrawdown > 0)
	assert.True(t, m.NetPnL < 0)
	assert.True(t, m.SharpeRatio < 0, "Sharpe should be negative for all-loser sequence")
}

func TestComputeMetrics_AllWinners(t *testing.T) {
	trades := []TradeRecord{
		{PnL: 100, Fees: 0, Slippage: 0, EntryPrice: 100, Qty: 1,
			EntryTime: dayTime(0), ExitTime: dayTime(1)},
		{PnL: 200, Fees: 0, Slippage: 0, EntryPrice: 100, Qty: 1,
			EntryTime: dayTime(2), ExitTime: dayTime(3)},
		{PnL: 150, Fees: 0, Slippage: 0, EntryPrice: 100, Qty: 1,
			EntryTime: dayTime(4), ExitTime: dayTime(5)},
	}

	m := ComputeMetrics(trades, 10000)

	assert.InDelta(t, 1.0, m.WinRate, floatTol)
	assert.True(t, math.IsInf(m.ProfitFactor, 1))
	assert.InDelta(t, 0.0, m.MaxDrawdown, floatTol)
	assert.InDelta(t, 200.0, m.LargestWin, floatTol)
	assert.Equal(t, 0.0, m.LargestLoss)
}

func TestBuildEquityCurve(t *testing.T) {
	trades := []TradeRecord{
		{PnL: 100, Fees: 10, Slippage: 5},  // net = 85
		{PnL: -50, Fees: 5, Slippage: 2},   // net = -57
		{PnL: 200, Fees: 8, Slippage: 3},   // net = 189
	}

	equity := buildEquityCurve(trades, 10000)

	require.Len(t, equity, 4)
	assert.InDelta(t, 10000.0, equity[0], floatTol)
	assert.InDelta(t, 10085.0, equity[1], floatTol)
	assert.InDelta(t, 10028.0, equity[2], floatTol)
	assert.InDelta(t, 10217.0, equity[3], floatTol)
}

func TestTradeReturns(t *testing.T) {
	trades := []TradeRecord{
		{PnL: 100, Fees: 0, Slippage: 0},
		{PnL: -50, Fees: 0, Slippage: 0},
	}

	returns := tradeReturns(trades, 10000)
	require.Len(t, returns, 2)
	assert.InDelta(t, 0.01, returns[0], floatTol)       // 100/10000
	assert.InDelta(t, -50.0/10100, returns[1], floatTol) // -50/10100
}

func TestMean(t *testing.T) {
	assert.InDelta(t, 2.0, mean([]float64{1, 2, 3}), floatTol)
	assert.Equal(t, 0.0, mean(nil))
	assert.InDelta(t, 5.0, mean([]float64{5}), floatTol)
}

func TestStddev(t *testing.T) {
	xs := []float64{2, 4, 4, 4, 5, 5, 7, 9}
	m := mean(xs)
	// Sample stddev: variance = 32/7 ≈ 4.5714, sd = sqrt(32/7) ≈ 2.13809
	sd := stddev(xs, m)
	assert.InDelta(t, math.Sqrt(32.0/7.0), sd, 1e-6)
}

func TestDownsideDeviation(t *testing.T) {
	xs := []float64{0.01, -0.02, 0.03, -0.01}
	dd := downsideDeviation(xs)

	// Sum of squared negatives: 0.0004 + 0.0001 = 0.0005
	// divided by (n-1) = 3
	expected := math.Sqrt(0.0005 / 3.0)
	assert.InDelta(t, expected, dd, 1e-9)
}

func TestDownsideDeviationNoNegatives(t *testing.T) {
	xs := []float64{0.01, 0.02, 0.03}
	assert.Equal(t, 0.0, downsideDeviation(xs))
}

func TestMaxDrawdown_TwoDrawdowns(t *testing.T) {
	// Two drawdowns: first a small one, then a larger one.
	equity := []float64{100, 110, 105, 115, 108, 120, 95}
	dd, ddPct := MaxDrawdownFromEquity(equity)

	// Peak at 120, trough at 95 -> dd = 25, ddPct = 25/120
	assert.InDelta(t, 25.0, dd, floatTol)
	assert.InDelta(t, 25.0/120.0, ddPct, floatTol)
}

func TestComputeMetrics_SharpeAndSortinoSign(t *testing.T) {
	// Profitable strategy: Sharpe should be positive.
	winningTrades := []TradeRecord{
		{PnL: 50, Fees: 1, Slippage: 0, EntryPrice: 100, Qty: 1,
			EntryTime: dayTime(0), ExitTime: dayTime(1)},
		{PnL: 30, Fees: 1, Slippage: 0, EntryPrice: 100, Qty: 1,
			EntryTime: dayTime(2), ExitTime: dayTime(3)},
		{PnL: -10, Fees: 1, Slippage: 0, EntryPrice: 100, Qty: 1,
			EntryTime: dayTime(4), ExitTime: dayTime(5)},
		{PnL: 40, Fees: 1, Slippage: 0, EntryPrice: 100, Qty: 1,
			EntryTime: dayTime(6), ExitTime: dayTime(7)},
	}

	m := ComputeMetrics(winningTrades, 10000)
	assert.True(t, m.SharpeRatio > 0, "profitable strategy should have positive Sharpe, got %f", m.SharpeRatio)
	// Sortino may or may not be computable depending on downside returns.
	// With one losing trade, it should be defined.
	assert.True(t, m.SortinoRatio > 0, "profitable strategy with losses should have positive Sortino, got %f", m.SortinoRatio)
}

func TestComputeMetrics_CalmarRatio(t *testing.T) {
	// Trades spanning ~1 year with known net PnL and drawdown.
	trades := []TradeRecord{
		{PnL: 500, Fees: 0, Slippage: 0, EntryPrice: 100, Qty: 1,
			EntryTime: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			ExitTime:  time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)},
		{PnL: -200, Fees: 0, Slippage: 0, EntryPrice: 100, Qty: 1,
			EntryTime: time.Date(2025, 3, 2, 0, 0, 0, 0, time.UTC),
			ExitTime:  time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)},
		{PnL: 800, Fees: 0, Slippage: 0, EntryPrice: 100, Qty: 1,
			EntryTime: time.Date(2025, 6, 2, 0, 0, 0, 0, time.UTC),
			ExitTime:  time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)},
	}

	m := ComputeMetrics(trades, 10000)

	// Net PnL = 1100, equity curve: 10000, 10500, 10300, 11100
	// Max DD = 200 (10500 -> 10300), max DD pct = 200/10500
	assert.InDelta(t, 200.0, m.MaxDrawdown, floatTol)
	assert.True(t, m.CalmarRatio > 0, "Calmar should be positive for profitable strategy, got %f", m.CalmarRatio)
}

func TestHoldingPeriodAvg(t *testing.T) {
	trades := []TradeRecord{
		{PnL: 10, EntryTime: dayTime(0), ExitTime: dayTime(3)},  // 3 days
		{PnL: 10, EntryTime: dayTime(5), ExitTime: dayTime(10)}, // 5 days
		{PnL: 10, EntryTime: dayTime(12), ExitTime: dayTime(14)}, // 2 days
	}

	m := ComputeMetrics(trades, 10000)

	// Average: (3 + 5 + 2) / 3 = 3.333... days
	expectedDays := (3 + 5 + 2) * 24 * time.Hour / 3
	assert.Equal(t, expectedDays, m.HoldingPeriodAvg)
}

func TestAnnualizeReturn(t *testing.T) {
	// 10% return over half a year -> annualized ~21%
	trades := []TradeRecord{
		{EntryTime: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			ExitTime: time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)},
	}

	result := annualizeReturn(trades, 0.10)
	// (1.10)^(365.25/181) - 1 ~ 0.2117... (depends on exact day count)
	assert.True(t, result > 0.10, "annualized return should be > total return for < 1 year")
	assert.True(t, result < 0.30, "should be reasonable, got %f", result)
}
