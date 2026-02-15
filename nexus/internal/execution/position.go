package execution

import (
	"fmt"
	"sync"
	"time"

	"github.com/nexus-trading/nexus/internal/bus"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Position
// ---------------------------------------------------------------------------

// Position tracks the state of a single position for a strategy+exchange+symbol.
type Position struct {
	StrategyID    string          `json:"strategy_id"`
	Exchange      string          `json:"exchange"`
	Symbol        string          `json:"symbol"`
	Side          string          `json:"side"` // long|short|flat
	Qty           decimal.Decimal `json:"qty"`  // signed: positive=long, negative=short
	AvgEntry      decimal.Decimal `json:"avg_entry"`
	UnrealizedPnL decimal.Decimal `json:"unrealized_pnl"`
	RealizedPnL   decimal.Decimal `json:"realized_pnl"`
	TotalFees     decimal.Decimal `json:"total_fees"`
	OpenedAt      time.Time       `json:"opened_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	TradeCount    int             `json:"trade_count"`
}

// ---------------------------------------------------------------------------
// PositionManager
// ---------------------------------------------------------------------------

// PositionManager tracks per-strategy and global (netted) positions.
// Thread-safe for concurrent access.
type PositionManager struct {
	mu        sync.RWMutex
	positions map[string]*Position // key: "strategy.exchange.symbol"
	globalPos map[string]*Position // key: "exchange.symbol" (netted across strategies)
}

// NewPositionManager creates a new empty PositionManager.
func NewPositionManager() *PositionManager {
	return &PositionManager{
		positions: make(map[string]*Position),
		globalPos: make(map[string]*Position),
	}
}

// positionKey builds the map key for a strategy-level position.
func positionKey(strategyID, exchange, symbol string) string {
	return fmt.Sprintf("%s.%s.%s", strategyID, exchange, symbol)
}

// globalKey builds the map key for a global (cross-strategy) position.
func globalKey(exchange, symbol string) string {
	return fmt.Sprintf("%s.%s", exchange, symbol)
}

// sideFromQty derives the side string from a signed quantity.
func sideFromQty(qty decimal.Decimal) string {
	switch qty.Sign() {
	case 1:
		return "long"
	case -1:
		return "short"
	default:
		return "flat"
	}
}

// ---------------------------------------------------------------------------
// ApplyFill
// ---------------------------------------------------------------------------

// ApplyFill applies a fill event to both the strategy-level and global-level
// positions. It computes realized PnL on position reduction/closure, and
// updates the weighted-average entry price on position additions.
func (pm *PositionManager) ApplyFill(fill bus.Fill) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Determine signed fill quantity: buy is positive, sell is negative.
	signedQty := fill.Qty
	if fill.Side == "sell" {
		signedQty = signedQty.Neg()
	}
	ts := fill.Timestamp

	// --- Update strategy-level position ---
	sKey := positionKey(fill.StrategyID, fill.Exchange, fill.Symbol)
	sPos, ok := pm.positions[sKey]
	if !ok {
		sPos = &Position{
			StrategyID:  fill.StrategyID,
			Exchange:    fill.Exchange,
			Symbol:      fill.Symbol,
			Side:        "flat",
			Qty:         decimal.Zero,
			AvgEntry:    decimal.Zero,
			RealizedPnL: decimal.Zero,
			TotalFees:   decimal.Zero,
		}
		pm.positions[sKey] = sPos
	}
	applyFillToPosition(sPos, fill.Price, signedQty, fill.Fee, ts)

	// --- Update global-level position (netted across strategies) ---
	gKey := globalKey(fill.Exchange, fill.Symbol)
	gPos, ok := pm.globalPos[gKey]
	if !ok {
		gPos = &Position{
			StrategyID:  "_global",
			Exchange:    fill.Exchange,
			Symbol:      fill.Symbol,
			Side:        "flat",
			Qty:         decimal.Zero,
			AvgEntry:    decimal.Zero,
			RealizedPnL: decimal.Zero,
			TotalFees:   decimal.Zero,
		}
		pm.globalPos[gKey] = gPos
	}
	applyFillToPosition(gPos, fill.Price, signedQty, fill.Fee, ts)

	return nil
}

// applyFillToPosition is the core position update logic. It handles:
//   - Opening a new position (from flat)
//   - Adding to an existing position (same direction) with weighted-average entry
//   - Reducing / closing a position (opposite direction) with realized PnL
//   - Flipping a position (e.g. long -> short in one fill)
func applyFillToPosition(pos *Position, fillPrice, signedQty, fee decimal.Decimal, ts time.Time) {
	oldQty := pos.Qty
	newQty := oldQty.Add(signedQty)
	absOld := oldQty.Abs()
	absFill := signedQty.Abs()

	// Always accumulate fees and trade count.
	pos.TotalFees = pos.TotalFees.Add(fee)
	pos.TradeCount++
	pos.UpdatedAt = ts

	// --- Case 1: Opening from flat ---
	if oldQty.IsZero() {
		pos.AvgEntry = fillPrice
		pos.Qty = newQty
		pos.Side = sideFromQty(newQty)
		pos.OpenedAt = ts
		return
	}

	// --- Case 2: Adding to position (same direction) ---
	sameDirection := (oldQty.Sign() > 0 && signedQty.Sign() > 0) ||
		(oldQty.Sign() < 0 && signedQty.Sign() < 0)

	if sameDirection {
		// Weighted-average entry: (avgEntry * |oldQty| + fillPrice * |fillQty|) / (|oldQty| + |fillQty|)
		totalCost := pos.AvgEntry.Mul(absOld).Add(fillPrice.Mul(absFill))
		totalQty := absOld.Add(absFill)
		pos.AvgEntry = totalCost.Div(totalQty)
		pos.Qty = newQty
		pos.Side = sideFromQty(newQty)
		return
	}

	// --- Case 3: Reducing / closing / flipping (opposite direction) ---
	closeQty := absOld
	if absFill.LessThan(absOld) {
		closeQty = absFill
	}

	// Realize PnL on the closed portion.
	if oldQty.Sign() > 0 {
		// Closing long: realized = (fillPrice - avgEntry) * closeQty
		pos.RealizedPnL = pos.RealizedPnL.Add(
			fillPrice.Sub(pos.AvgEntry).Mul(closeQty),
		)
	} else {
		// Closing short: realized = (avgEntry - fillPrice) * closeQty
		pos.RealizedPnL = pos.RealizedPnL.Add(
			pos.AvgEntry.Sub(fillPrice).Mul(closeQty),
		)
	}

	pos.Qty = newQty
	pos.Side = sideFromQty(newQty)

	// Determine new average entry.
	if newQty.IsZero() {
		// Fully closed — reset avg entry.
		pos.AvgEntry = decimal.Zero
	} else if newQty.Sign() != oldQty.Sign() {
		// Position flipped — the excess quantity has the fill price as its entry.
		pos.AvgEntry = fillPrice
		pos.OpenedAt = ts
	}
	// If merely reduced (same sign as before), avgEntry stays unchanged.
}

// ---------------------------------------------------------------------------
// Query methods
// ---------------------------------------------------------------------------

// GetPosition returns the strategy-level position, or nil if none exists.
func (pm *PositionManager) GetPosition(strategyID, exchange, symbol string) *Position {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.positions[positionKey(strategyID, exchange, symbol)]
}

// GetGlobalPosition returns the global (cross-strategy) position, or nil.
func (pm *PositionManager) GetGlobalPosition(exchange, symbol string) *Position {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.globalPos[globalKey(exchange, symbol)]
}

// AllPositions returns a snapshot of all strategy-level positions.
func (pm *PositionManager) AllPositions() []*Position {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	result := make([]*Position, 0, len(pm.positions))
	for _, p := range pm.positions {
		result = append(result, p)
	}
	return result
}

// TotalExposureUSD returns the total notional exposure across all global
// positions, using avg entry price as the price proxy.
func (pm *PositionManager) TotalExposureUSD() float64 {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	total := decimal.Zero
	for _, p := range pm.globalPos {
		if !p.Qty.IsZero() {
			notional := p.Qty.Abs().Mul(p.AvgEntry)
			total = total.Add(notional)
		}
	}
	return total.InexactFloat64()
}

// StrategyPnL returns the total realized PnL for a given strategy across
// all its positions.
func (pm *PositionManager) StrategyPnL(strategyID string) decimal.Decimal {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	total := decimal.Zero
	for _, p := range pm.positions {
		if p.StrategyID == strategyID {
			total = total.Add(p.RealizedPnL)
		}
	}
	return total
}
