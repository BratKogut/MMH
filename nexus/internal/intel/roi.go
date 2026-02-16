package intel

import (
	"sync"
	"time"
)

// TradeLink connects an intel event to a trade for ROI measurement.
// If a trade executes on the same asset within the link window after
// an intel event, the trade is attributed to that intel event.
type TradeLink struct {
	IntelEventID string    `json:"intel_event_id"`
	TradeID      string    `json:"trade_id"`
	StrategyID   string    `json:"strategy_id"`
	Symbol       string    `json:"symbol"`
	Timestamp    time.Time `json:"ts"`
	PnL          float64   `json:"pnl"`            // realized PnL of the linked trade
	IntelCostUSD float64   `json:"intel_cost_usd"` // cost of the intel event
}

// ROITracker measures the return on investment of intel events.
// It links trades to intel events by asset and time proximity.
//
// The core question: is our intel spending actually making money?
type ROITracker struct {
	mu    sync.RWMutex
	links []TradeLink

	// Window for linking: if a trade happens within this duration
	// after an intel event about the same asset, they're linked.
	linkWindowMs int64

	// Active intel events (not yet expired).
	activeEvents map[string]*IntelEvent // eventID -> event
}

// NewROITracker creates a new ROITracker.
// linkWindowMs is the maximum time (in milliseconds) after an intel event
// during which a trade on the same asset will be attributed to the event.
func NewROITracker(linkWindowMs int64) *ROITracker {
	return &ROITracker{
		links:        make([]TradeLink, 0, 256),
		linkWindowMs: linkWindowMs,
		activeEvents: make(map[string]*IntelEvent),
	}
}

// RecordIntelEvent registers an intel event for potential future trade linking.
func (r *ROITracker) RecordIntelEvent(event *IntelEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.activeEvents[event.EventID] = event
}

// RecordTrade checks all active intel events for asset overlap within the
// link window. Returns all newly created TradeLinks.
func (r *ROITracker) RecordTrade(tradeID, strategyID, symbol string, pnl float64, ts time.Time) []TradeLink {
	r.mu.Lock()
	defer r.mu.Unlock()

	var newLinks []TradeLink

	for _, event := range r.activeEvents {
		// Check if trade timestamp is within the link window.
		linkWindow := time.Duration(r.linkWindowMs) * time.Millisecond
		if ts.Before(event.Timestamp) || ts.After(event.Timestamp.Add(linkWindow)) {
			continue
		}

		// Check if the event covers this symbol.
		if !containsAsset(event.AssetTags, symbol) {
			continue
		}

		link := TradeLink{
			IntelEventID: event.EventID,
			TradeID:      tradeID,
			StrategyID:   strategyID,
			Symbol:       symbol,
			Timestamp:    ts,
			PnL:          pnl,
			IntelCostUSD: event.Cost.ProviderCostUSD,
		}

		r.links = append(r.links, link)
		newLinks = append(newLinks, link)
	}

	return newLinks
}

// ComputeROI calculates the aggregate ROI summary across all tracked links.
func (r *ROITracker) ComputeROI() ROISummary {
	r.mu.RLock()
	defer r.mu.RUnlock()

	summary := ROISummary{}

	// Track which events have at least one link.
	linkedEventIDs := make(map[string]bool)

	for _, link := range r.links {
		summary.TotalIntelCostUSD += link.IntelCostUSD
		summary.TotalLinkedPnL += link.PnL
		summary.TotalLinks++
		linkedEventIDs[link.IntelEventID] = true

		if link.PnL > link.IntelCostUSD {
			summary.EventsWithPositiveROI++
		}
	}

	// Count unlinked events (intel that didn't lead to trades).
	for eventID := range r.activeEvents {
		if !linkedEventIDs[eventID] {
			summary.UnlinkedEvents++
		}
	}

	// Compute net ROI.
	if summary.TotalIntelCostUSD > 0 {
		summary.NetROI = (summary.TotalLinkedPnL - summary.TotalIntelCostUSD) / summary.TotalIntelCostUSD
	}

	// Compute cost per dollar earned.
	if summary.TotalLinkedPnL > 0 {
		summary.CostPerDollarEarned = summary.TotalIntelCostUSD / summary.TotalLinkedPnL
	}

	return summary
}

// CleanExpired removes intel events that have exceeded their TTL from the
// active set. Links are retained permanently for historical ROI analysis.
func (r *ROITracker) CleanExpired() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for eventID, event := range r.activeEvents {
		if event.IsExpired() {
			delete(r.activeEvents, eventID)
		}
	}
}

// ActiveEventCount returns the number of active (non-expired) intel events.
func (r *ROITracker) ActiveEventCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.activeEvents)
}

// LinkCount returns the total number of trade links.
func (r *ROITracker) LinkCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.links)
}

// ROISummary is the aggregate ROI report.
type ROISummary struct {
	TotalIntelCostUSD     float64 `json:"total_intel_cost_usd"`
	TotalLinkedPnL        float64 `json:"total_linked_pnl"`
	NetROI                float64 `json:"net_roi"`                  // (PnL - cost) / cost
	TotalLinks            int     `json:"total_links"`
	UnlinkedEvents        int     `json:"unlinked_events"`          // intel events that didn't lead to trades
	EventsWithPositiveROI int     `json:"events_with_positive_roi"`
	CostPerDollarEarned   float64 `json:"cost_per_dollar_earned"`   // how much intel costs per $ of linked PnL
}

// containsAsset checks if any asset tag matches the given symbol.
func containsAsset(tags []string, symbol string) bool {
	for _, tag := range tags {
		if tag == symbol {
			return true
		}
	}
	return false
}
