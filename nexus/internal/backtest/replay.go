package backtest

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/nexus-trading/nexus/internal/audit"
	"github.com/nexus-trading/nexus/internal/bus"
)

// ---------------------------------------------------------------------------
// ReplayConfig
// ---------------------------------------------------------------------------

// ReplayConfig configures a replay run.
type ReplayConfig struct {
	// Tolerance for divergence detection.
	SignalMatchRate  float64 // e.g., 0.99 = 99% of signals must match
	PositionDeltaPct float64 // e.g., 0.01 = 1% max position difference
	PnLDeltaPct     float64 // e.g., 0.02 = 2% max PnL difference
}

// ---------------------------------------------------------------------------
// DivergenceReport
// ---------------------------------------------------------------------------

// DivergenceReport captures differences between original and replayed execution.
type DivergenceReport struct {
	TotalSignals      int          `json:"total_signals"`
	MatchedSignals    int          `json:"matched_signals"`
	MismatchedSignals int          `json:"mismatched_signals"`
	SignalMatchRate   float64      `json:"signal_match_rate"`
	PositionDelta     float64      `json:"position_delta"`
	PnLDelta          float64      `json:"pnl_delta"`
	PnLDeltaPct       float64      `json:"pnl_delta_pct"`
	Passed            bool         `json:"passed"`
	Divergences       []Divergence `json:"divergences"`
}

// Divergence captures a single difference between original and replayed execution.
type Divergence struct {
	Timestamp time.Time `json:"ts"`
	Type      string    `json:"type"` // signal_mismatch, position_delta, pnl_delta
	Expected  string    `json:"expected"`
	Actual    string    `json:"actual"`
	Delta     float64   `json:"delta"`
}

// ---------------------------------------------------------------------------
// CompareRuns
// ---------------------------------------------------------------------------

// CompareRuns compares two sets of audit trail entries for divergence.
//
// The comparison logic:
//  1. Index entries by trace_id and event_type for both original and replayed.
//  2. Compare signal payloads by matching on trace_id.
//  3. Track position deltas from position_update entries.
//  4. Track PnL deltas from fill entries.
//  5. Compute the divergence report and mark as passed/failed based on tolerance.
func CompareRuns(original, replayed []audit.Entry, config ReplayConfig) *DivergenceReport {
	report := &DivergenceReport{}

	// 1. Build indices keyed by (trace_id, event_type) -> entries.
	origIndex := buildEntryIndex(original)
	replayIndex := buildEntryIndex(replayed)

	// 2. Compare signal entries.
	origSignals := filterByEventType(original, audit.EventSignal)
	replaySignals := filterByEventType(replayed, audit.EventSignal)

	// Build a lookup for replayed signals by trace_id for matching.
	replaySignalByTrace := make(map[string][]audit.Entry)
	for _, e := range replaySignals {
		if e.TraceID != "" {
			replaySignalByTrace[e.TraceID] = append(replaySignalByTrace[e.TraceID], e)
		}
	}

	report.TotalSignals = len(origSignals)
	for _, origEntry := range origSignals {
		if origEntry.TraceID == "" {
			// Cannot match without trace_id; count as mismatch.
			report.MismatchedSignals++
			report.Divergences = append(report.Divergences, Divergence{
				Timestamp: origEntry.Timestamp,
				Type:      "signal_mismatch",
				Expected:  "signal with trace_id",
				Actual:    "signal without trace_id",
			})
			continue
		}

		replayMatches, found := replaySignalByTrace[origEntry.TraceID]
		if !found || len(replayMatches) == 0 {
			report.MismatchedSignals++
			report.Divergences = append(report.Divergences, Divergence{
				Timestamp: origEntry.Timestamp,
				Type:      "signal_mismatch",
				Expected:  truncatePayload(origEntry.Payload, 200),
				Actual:    "<missing>",
				Delta:     1.0,
			})
			continue
		}

		// Compare the first matching replayed signal payload.
		matched := compareSignalPayloads(origEntry.Payload, replayMatches[0].Payload)
		if matched {
			report.MatchedSignals++
		} else {
			report.MismatchedSignals++
			report.Divergences = append(report.Divergences, Divergence{
				Timestamp: origEntry.Timestamp,
				Type:      "signal_mismatch",
				Expected:  truncatePayload(origEntry.Payload, 200),
				Actual:    truncatePayload(replayMatches[0].Payload, 200),
			})
		}
	}

	// Also check for extra signals in replay that were not in original.
	origSignalTraces := make(map[string]bool)
	for _, e := range origSignals {
		if e.TraceID != "" {
			origSignalTraces[e.TraceID] = true
		}
	}
	for _, e := range replaySignals {
		if e.TraceID != "" && !origSignalTraces[e.TraceID] {
			report.MismatchedSignals++
			report.TotalSignals++
			report.Divergences = append(report.Divergences, Divergence{
				Timestamp: e.Timestamp,
				Type:      "signal_mismatch",
				Expected:  "<missing>",
				Actual:    truncatePayload(e.Payload, 200),
				Delta:     1.0,
			})
		}
	}

	if report.TotalSignals > 0 {
		report.SignalMatchRate = float64(report.MatchedSignals) / float64(report.TotalSignals)
	} else {
		report.SignalMatchRate = 1.0 // no signals => trivially matching
	}

	// 3. Compare position deltas from position_update entries.
	report.PositionDelta = computePositionDelta(origIndex, replayIndex)

	// 4. Compare PnL deltas from fill entries.
	origPnL := extractTotalPnL(original)
	replayPnL := extractTotalPnL(replayed)
	report.PnLDelta = replayPnL - origPnL
	if math.Abs(origPnL) > 1e-12 {
		report.PnLDeltaPct = math.Abs(report.PnLDelta) / math.Abs(origPnL)
	}

	if math.Abs(report.PnLDelta) > 1e-12 {
		report.Divergences = append(report.Divergences, Divergence{
			Type:     "pnl_delta",
			Expected: fmt.Sprintf("%.6f", origPnL),
			Actual:   fmt.Sprintf("%.6f", replayPnL),
			Delta:    report.PnLDelta,
		})
	}

	if math.Abs(report.PositionDelta) > 1e-12 {
		report.Divergences = append(report.Divergences, Divergence{
			Type:     "position_delta",
			Expected: "0.0",
			Actual:   fmt.Sprintf("%.6f", report.PositionDelta),
			Delta:    report.PositionDelta,
		})
	}

	// 5. Determine pass/fail.
	report.Passed = true
	if report.SignalMatchRate < config.SignalMatchRate {
		report.Passed = false
	}
	if math.Abs(report.PositionDelta) > 0 && config.PositionDeltaPct > 0 {
		// Normalize position delta against the max observed position.
		maxPos := maxAbsPosition(original, replayed)
		if maxPos > 0 && math.Abs(report.PositionDelta)/maxPos > config.PositionDeltaPct {
			report.Passed = false
		}
	}
	if config.PnLDeltaPct > 0 && report.PnLDeltaPct > config.PnLDeltaPct {
		report.Passed = false
	}

	return report
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// entryKey is a composite key for indexing audit entries.
type entryKey struct {
	traceID   string
	eventType string
}

// buildEntryIndex creates a map from (traceID, eventType) to entries.
func buildEntryIndex(entries []audit.Entry) map[entryKey][]audit.Entry {
	idx := make(map[entryKey][]audit.Entry, len(entries))
	for _, e := range entries {
		k := entryKey{traceID: e.TraceID, eventType: e.EventType}
		idx[k] = append(idx[k], e)
	}
	return idx
}

// filterByEventType returns entries matching a specific event type.
func filterByEventType(entries []audit.Entry, eventType string) []audit.Entry {
	var result []audit.Entry
	for _, e := range entries {
		if e.EventType == eventType {
			result = append(result, e)
		}
	}
	return result
}

// signalPayload extracts key fields from a signal payload for comparison.
type signalPayload struct {
	StrategyID     string  `json:"strategy_id"`
	Symbol         string  `json:"symbol"`
	SignalType     string  `json:"signal_type"`
	Confidence     float64 `json:"confidence"`
	TargetPosition string  `json:"target_position,omitempty"`
}

// compareSignalPayloads compares two signal JSON payloads for semantic equivalence.
// We compare key fields rather than raw JSON to allow for timestamp and ID differences.
func compareSignalPayloads(origJSON, replayJSON string) bool {
	var orig, replay signalPayload
	if err := json.Unmarshal([]byte(origJSON), &orig); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(replayJSON), &replay); err != nil {
		return false
	}

	if orig.StrategyID != replay.StrategyID {
		return false
	}
	if orig.Symbol != replay.Symbol {
		return false
	}
	if orig.SignalType != replay.SignalType {
		return false
	}
	// Allow small floating-point differences in confidence.
	if math.Abs(orig.Confidence-replay.Confidence) > 1e-6 {
		return false
	}
	if orig.TargetPosition != replay.TargetPosition {
		return false
	}
	return true
}

// positionPayload extracts position fields from a position_update payload.
type positionPayload struct {
	StrategyID string  `json:"strategy_id"`
	Symbol     string  `json:"symbol"`
	Qty        float64 `json:"qty"`
}

// computePositionDelta compares position_update entries between runs.
// Returns the sum of absolute position differences at matching trace points.
func computePositionDelta(origIndex, replayIndex map[entryKey][]audit.Entry) float64 {
	totalDelta := 0.0

	// Iterate over original position updates.
	for k, origEntries := range origIndex {
		if k.eventType != audit.EventPositionUpdate {
			continue
		}
		replayEntries, found := replayIndex[k]
		if !found {
			// Position update exists in original but not replay.
			for _, e := range origEntries {
				var pp positionPayload
				if err := json.Unmarshal([]byte(e.Payload), &pp); err == nil {
					totalDelta += math.Abs(pp.Qty)
				}
			}
			continue
		}
		// Compare the last position update for each matching key.
		origLast := origEntries[len(origEntries)-1]
		replayLast := replayEntries[len(replayEntries)-1]

		var origPos, replayPos positionPayload
		json.Unmarshal([]byte(origLast.Payload), &origPos)
		json.Unmarshal([]byte(replayLast.Payload), &replayPos)

		totalDelta += math.Abs(origPos.Qty - replayPos.Qty)
	}

	// Also check for position updates in replay that are not in original.
	for k, replayEntries := range replayIndex {
		if k.eventType != audit.EventPositionUpdate {
			continue
		}
		if _, found := origIndex[k]; !found {
			for _, e := range replayEntries {
				var pp positionPayload
				if err := json.Unmarshal([]byte(e.Payload), &pp); err == nil {
					totalDelta += math.Abs(pp.Qty)
				}
			}
		}
	}

	return totalDelta
}

// fillPayload extracts PnL-relevant fields from a fill payload.
type fillPayload struct {
	Price    float64 `json:"price"`
	Qty      float64 `json:"qty"`
	Fee      float64 `json:"fee"`
	Side     string  `json:"side"`
	Symbol   string  `json:"symbol"`
}

// extractTotalPnL computes total realized PnL from fill entries in an audit trail.
// This is a simplified estimation: it sums (price * qty) for sells and subtracts
// for buys, plus fees.
func extractTotalPnL(entries []audit.Entry) float64 {
	// We parse fill entries to extract a simplified PnL.
	// For a proper comparison we look at the realized_pnl field if present,
	// otherwise we approximate from fills.
	totalPnL := 0.0
	netCash := 0.0
	totalFees := 0.0

	for _, e := range entries {
		if e.EventType != audit.EventFill {
			continue
		}

		// Try to extract a full bus.Fill from the payload.
		var fill bus.Fill
		if err := json.Unmarshal([]byte(e.Payload), &fill); err != nil {
			// Fall back to simplified payload.
			var fp fillPayload
			if err := json.Unmarshal([]byte(e.Payload), &fp); err != nil {
				continue
			}
			notional := fp.Price * fp.Qty
			if fp.Side == "sell" {
				netCash += notional
			} else {
				netCash -= notional
			}
			totalFees += fp.Fee
			continue
		}

		price := fill.Price.InexactFloat64()
		qty := fill.Qty.InexactFloat64()
		fee := fill.Fee.InexactFloat64()
		notional := price * qty

		if fill.Side == "sell" {
			netCash += notional
		} else {
			netCash -= notional
		}
		totalFees += fee
	}

	// Total PnL = net cash from fills - fees.
	totalPnL = netCash - totalFees
	return totalPnL
}

// maxAbsPosition finds the maximum absolute position observed across both runs.
// Used to normalize position delta.
func maxAbsPosition(original, replayed []audit.Entry) float64 {
	maxPos := 0.0
	for _, entries := range [][]audit.Entry{original, replayed} {
		for _, e := range entries {
			if e.EventType != audit.EventPositionUpdate {
				continue
			}
			var pp positionPayload
			if err := json.Unmarshal([]byte(e.Payload), &pp); err == nil {
				absQty := math.Abs(pp.Qty)
				if absQty > maxPos {
					maxPos = absQty
				}
			}
		}
	}
	return maxPos
}

// truncatePayload truncates a payload string to maxLen for readable divergence reports.
func truncatePayload(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
