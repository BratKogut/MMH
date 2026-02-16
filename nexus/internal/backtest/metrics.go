package backtest

import (
	"math"
	"time"
)

// PerformanceMetrics holds all computed performance statistics for a backtest.
type PerformanceMetrics struct {
	TotalPnL         float64       // Gross PnL (sum of all trade PnLs)
	NetPnL           float64       // PnL after fees and slippage
	TotalFees        float64       // Sum of all fees
	TotalSlippage    float64       // Sum of all slippage
	TradeCount       int           // Number of trades
	WinRate          float64       // Fraction of winning trades [0, 1]
	ProfitFactor     float64       // Gross profit / gross loss (abs)
	SharpeRatio      float64       // Annualized Sharpe from daily returns
	SortinoRatio     float64       // Annualized Sortino from daily returns
	MaxDrawdown      float64       // Maximum peak-to-trough decline in absolute terms
	MaxDrawdownPct   float64       // Maximum peak-to-trough decline as percentage
	CalmarRatio      float64       // Annualized return / max drawdown
	AvgTrade         float64       // Average PnL per trade (net)
	AvgWin           float64       // Average PnL of winning trades
	AvgLoss          float64       // Average PnL of losing trades
	LargestWin       float64       // Largest single winning trade PnL
	LargestLoss      float64       // Largest single losing trade PnL (negative)
	Turnover         float64       // Total notional volume / initial capital
	CostImpact       float64       // Total costs / gross PnL (fraction of profits eaten by costs)
	HoldingPeriodAvg time.Duration // Average holding period across all trades
}

// TradeRecord represents a single completed round-trip trade.
type TradeRecord struct {
	Symbol     string
	Side       string // buy or sell
	EntryPrice float64
	ExitPrice  float64
	Qty        float64
	PnL        float64
	Fees       float64
	Slippage   float64
	EntryTime  time.Time
	ExitTime   time.Time
}

// ComputeMetrics calculates comprehensive performance metrics from a trade list.
// initialCapital is the starting capital used for return and drawdown calculations.
// All calculations are deterministic with no I/O or time.Now() calls.
func ComputeMetrics(trades []TradeRecord, initialCapital float64) PerformanceMetrics {
	m := PerformanceMetrics{}

	if len(trades) == 0 || initialCapital <= 0 {
		return m
	}

	m.TradeCount = len(trades)

	var grossProfit, grossLoss float64
	var winCount int
	var totalWinPnL, totalLossPnL float64
	var totalVolume float64
	var totalHoldingNs int64

	m.LargestWin = 0
	m.LargestLoss = 0

	for _, tr := range trades {
		netPnL := tr.PnL - tr.Fees - tr.Slippage

		m.TotalPnL += tr.PnL
		m.TotalFees += tr.Fees
		m.TotalSlippage += tr.Slippage
		totalVolume += math.Abs(tr.Qty * tr.EntryPrice)
		totalHoldingNs += tr.ExitTime.Sub(tr.EntryTime).Nanoseconds()

		if netPnL > 0 {
			winCount++
			grossProfit += netPnL
			totalWinPnL += netPnL
			if netPnL > m.LargestWin {
				m.LargestWin = netPnL
			}
		} else if netPnL < 0 {
			grossLoss += math.Abs(netPnL)
			totalLossPnL += netPnL
			if netPnL < m.LargestLoss {
				m.LargestLoss = netPnL
			}
		}
	}

	m.NetPnL = m.TotalPnL - m.TotalFees - m.TotalSlippage

	// Win rate.
	m.WinRate = float64(winCount) / float64(m.TradeCount)

	// Average trade PnL (net).
	m.AvgTrade = m.NetPnL / float64(m.TradeCount)

	// Average win / average loss.
	lossCount := m.TradeCount - winCount
	// Count trades with exactly zero PnL as neither win nor loss for avg calculations,
	// but they contribute to the denominator above for AvgTrade.
	if winCount > 0 {
		m.AvgWin = totalWinPnL / float64(winCount)
	}
	if lossCount > 0 {
		m.AvgLoss = totalLossPnL / float64(lossCount)
	}

	// Profit factor.
	m.ProfitFactor = ProfitFactorFromTrades(trades)

	// Turnover: total notional volume / initial capital.
	m.Turnover = totalVolume / initialCapital

	// Cost impact: total costs / gross PnL.
	totalCosts := m.TotalFees + m.TotalSlippage
	if m.TotalPnL > 0 {
		m.CostImpact = totalCosts / m.TotalPnL
	}

	// Average holding period.
	if m.TradeCount > 0 {
		avgNs := totalHoldingNs / int64(m.TradeCount)
		m.HoldingPeriodAvg = time.Duration(avgNs)
	}

	// Build equity curve for drawdown and return-based metrics.
	equity := buildEquityCurve(trades, initialCapital)

	m.MaxDrawdown, m.MaxDrawdownPct = MaxDrawdownFromEquity(equity)

	// Compute daily returns from the equity curve for Sharpe/Sortino.
	// Use trade-level returns (per-trade returns as a proxy for period returns).
	returns := tradeReturns(trades, initialCapital)
	periodsPerYear := 252.0 // trading days per year

	m.SharpeRatio = SharpeFromReturns(returns, periodsPerYear)
	m.SortinoRatio = SortinoFromReturns(returns, periodsPerYear)

	// Calmar ratio: annualized return / max drawdown.
	if m.MaxDrawdown > 0 {
		totalReturn := m.NetPnL / initialCapital
		// Estimate annualized return based on the trade span.
		annualizedReturn := annualizeReturn(trades, totalReturn)
		m.CalmarRatio = annualizedReturn / m.MaxDrawdownPct
	}

	return m
}

// SharpeFromReturns computes the annualized Sharpe ratio from a series of periodic returns.
// Formula: Sharpe = mean(returns) / std(returns) * sqrt(periodsPerYear)
// Returns 0 if there are fewer than 2 returns or std is zero.
func SharpeFromReturns(returns []float64, periodsPerYear float64) float64 {
	if len(returns) < 2 {
		return 0
	}

	mean := mean(returns)
	std := stddev(returns, mean)

	if std == 0 {
		return 0
	}

	return (mean / std) * math.Sqrt(periodsPerYear)
}

// SortinoFromReturns computes the annualized Sortino ratio from a series of periodic returns.
// Formula: Sortino = mean(returns) / downside_std(returns) * sqrt(periodsPerYear)
// Only negative returns contribute to the downside deviation.
func SortinoFromReturns(returns []float64, periodsPerYear float64) float64 {
	if len(returns) < 2 {
		return 0
	}

	m := mean(returns)
	downDev := downsideDeviation(returns)

	if downDev == 0 {
		return 0
	}

	return (m / downDev) * math.Sqrt(periodsPerYear)
}

// MaxDrawdownFromEquity computes the maximum peak-to-trough decline from an equity curve.
// Returns both the absolute drawdown and the percentage drawdown.
// The equity slice should contain sequential equity values starting from initial capital.
func MaxDrawdownFromEquity(equity []float64) (drawdown float64, drawdownPct float64) {
	if len(equity) < 2 {
		return 0, 0
	}

	peak := equity[0]
	maxDD := 0.0
	maxDDPct := 0.0

	for _, eq := range equity {
		if eq > peak {
			peak = eq
		}
		dd := peak - eq
		if dd > maxDD {
			maxDD = dd
		}
		if peak > 0 {
			ddPct := dd / peak
			if ddPct > maxDDPct {
				maxDDPct = ddPct
			}
		}
	}

	return maxDD, maxDDPct
}

// ProfitFactorFromTrades computes gross profit / gross loss from trades.
// Returns math.Inf(1) if there are no losing trades but there are winning trades.
// Returns 0 if there are no trades or no winning trades.
func ProfitFactorFromTrades(trades []TradeRecord) float64 {
	if len(trades) == 0 {
		return 0
	}

	var grossProfit, grossLoss float64
	for _, tr := range trades {
		netPnL := tr.PnL - tr.Fees - tr.Slippage
		if netPnL > 0 {
			grossProfit += netPnL
		} else if netPnL < 0 {
			grossLoss += math.Abs(netPnL)
		}
	}

	if grossLoss == 0 {
		if grossProfit > 0 {
			return math.Inf(1)
		}
		return 0
	}

	return grossProfit / grossLoss
}

// --- Internal helpers ---

// buildEquityCurve constructs an equity curve from trades, starting at initialCapital.
// Each point represents the equity after each trade.
func buildEquityCurve(trades []TradeRecord, initialCapital float64) []float64 {
	equity := make([]float64, len(trades)+1)
	equity[0] = initialCapital

	for i, tr := range trades {
		netPnL := tr.PnL - tr.Fees - tr.Slippage
		equity[i+1] = equity[i] + netPnL
	}

	return equity
}

// tradeReturns computes per-trade fractional returns relative to running capital.
func tradeReturns(trades []TradeRecord, initialCapital float64) []float64 {
	if len(trades) == 0 {
		return nil
	}

	returns := make([]float64, len(trades))
	capital := initialCapital

	for i, tr := range trades {
		netPnL := tr.PnL - tr.Fees - tr.Slippage
		if capital > 0 {
			returns[i] = netPnL / capital
		}
		capital += netPnL
	}

	return returns
}

// annualizeReturn estimates an annualized return from a total return and the
// trading time span covered by the trades.
func annualizeReturn(trades []TradeRecord, totalReturn float64) float64 {
	if len(trades) == 0 {
		return 0
	}

	firstEntry := trades[0].EntryTime
	lastExit := trades[len(trades)-1].ExitTime
	span := lastExit.Sub(firstEntry)

	if span <= 0 {
		return totalReturn
	}

	years := span.Hours() / (365.25 * 24)
	if years <= 0 {
		return totalReturn
	}

	// Compound annualization: (1 + totalReturn)^(1/years) - 1
	if totalReturn <= -1 {
		return -1 // total wipeout
	}

	return math.Pow(1+totalReturn, 1.0/years) - 1
}

// mean returns the arithmetic mean of a slice.
func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// stddev returns the sample standard deviation of a slice given its mean.
func stddev(xs []float64, m float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	sumSq := 0.0
	for _, x := range xs {
		d := x - m
		sumSq += d * d
	}
	return math.Sqrt(sumSq / float64(len(xs)-1))
}

// downsideDeviation returns the downside deviation (target = 0) from a slice of returns.
// Only negative returns contribute.
func downsideDeviation(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	sumSq := 0.0
	count := 0
	for _, x := range xs {
		if x < 0 {
			sumSq += x * x
			count++
		}
	}
	if count == 0 {
		return 0
	}
	// Use total count (not just negative count) in denominator for semi-deviation.
	return math.Sqrt(sumSq / float64(len(xs)-1))
}
