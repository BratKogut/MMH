package jupiter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/nexus-trading/nexus/internal/solana"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Jupiter V6 API Client — real quote + swap endpoints
// https://station.jup.ag/docs/apis/swap-api
// ---------------------------------------------------------------------------

const (
	jupiterQuoteURL = "https://quote-api.jup.ag/v6/quote"
	jupiterSwapURL  = "https://quote-api.jup.ag/v6/swap"
	jupiterPriceURL = "https://price.jup.ag/v6/price"

	maxRetries   = 2
	retryBackoff = 500 * time.Millisecond
)

// APIClient is the real Jupiter V6 API client.
type APIClient struct {
	httpClient *http.Client
	walletPub  string // base58 public key of the wallet

	quoteCount   atomic.Int64
	swapCount    atomic.Int64
	errorCount   atomic.Int64
	avgLatencyMs atomic.Int64

	// Circuit breaker.
	consecutiveErrors atomic.Int64
	circuitOpen       atomic.Bool
}

// NewAPIClient creates a new Jupiter API client.
func NewAPIClient(walletPubkey string) *APIClient {
	return &APIClient{
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		walletPub: walletPubkey,
	}
}

// ---------------------------------------------------------------------------
// Quote API — get best route for a swap
// ---------------------------------------------------------------------------

// QuoteRequest is the request to Jupiter /quote endpoint.
type QuoteRequest struct {
	InputMint        string `json:"inputMint"`
	OutputMint       string `json:"outputMint"`
	Amount           string `json:"amount"`           // in smallest unit (lamports, etc.)
	SlippageBps      int    `json:"slippageBps"`
	OnlyDirectRoutes bool   `json:"onlyDirectRoutes"` // faster but less optimal
	AsLegacyTx       bool   `json:"asLegacyTransaction"`
}

// QuoteResponse is the response from Jupiter /quote endpoint.
type QuoteResponse struct {
	InputMint            string `json:"inputMint"`
	OutputMint           string `json:"outputMint"`
	InAmount             string `json:"inAmount"`
	OutAmount            string `json:"outAmount"`
	OtherAmountThreshold string `json:"otherAmountThreshold"`
	PriceImpactPct       string `json:"priceImpactPct"`
	SlippageBps          int    `json:"slippageBps"`
	RoutePlan            []struct {
		Percent  int `json:"percent"`
		SwapInfo struct {
			AmmKey    string `json:"ammKey"`
			Label     string `json:"label"`
			FeeAmount string `json:"feeAmount"`
			FeeMint   string `json:"feeMint"`
		} `json:"swapInfo"`
	} `json:"routePlan"`
	ContextSlot uint64  `json:"contextSlot"`
	TimeTaken   float64 `json:"timeTaken"`
}

// GetQuote fetches the best swap route from Jupiter.
func (c *APIClient) GetQuote(ctx context.Context, params solana.SwapParams) (*QuoteResponse, error) {
	if c.circuitOpen.Load() {
		return nil, fmt.Errorf("jupiter: circuit breaker open")
	}

	start := time.Now()

	// Convert amount to lamports/smallest unit.
	amountLamports := params.AmountIn.Mul(decimal.NewFromInt(1_000_000_000)).IntPart()

	// Use proper URL encoding.
	queryURL, err := url.Parse(jupiterQuoteURL)
	if err != nil {
		return nil, fmt.Errorf("jupiter: parse URL: %w", err)
	}
	q := queryURL.Query()
	q.Set("inputMint", string(params.InputMint))
	q.Set("outputMint", string(params.OutputMint))
	q.Set("amount", fmt.Sprintf("%d", amountLamports))
	q.Set("slippageBps", fmt.Sprintf("%d", params.SlippageBps))
	q.Set("onlyDirectRoutes", "false")
	queryURL.RawQuery = q.Encode()

	var quote QuoteResponse
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(retryBackoff * time.Duration(1<<uint(attempt-1))):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, "GET", queryURL.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("jupiter: create quote request: %w", err)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("jupiter: quote HTTP error: %w", err)
			c.errorCount.Add(1)
			c.recordError()
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("jupiter: read quote response: %w", err)
			c.errorCount.Add(1)
			c.recordError()
			continue
		}

		if resp.StatusCode == 429 {
			lastErr = fmt.Errorf("jupiter: rate limited (429)")
			c.errorCount.Add(1)
			continue
		}

		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("jupiter: quote HTTP %d: %s (mint=%s)", resp.StatusCode, string(body), params.OutputMint)
			c.errorCount.Add(1)
			c.recordError()
			continue
		}

		if err := json.Unmarshal(body, &quote); err != nil {
			return nil, fmt.Errorf("jupiter: parse quote: %w", err)
		}

		c.resetErrors()
		break
	}

	if lastErr != nil && quote.InAmount == "" {
		return nil, fmt.Errorf("jupiter: quote failed after %d attempts: %w", maxRetries+1, lastErr)
	}

	latency := time.Since(start).Milliseconds()
	c.quoteCount.Add(1)
	c.avgLatencyMs.Store(latency)

	inMint := quote.InputMint
	outMint := quote.OutputMint
	if len(inMint) > 8 {
		inMint = inMint[:8]
	}
	if len(outMint) > 8 {
		outMint = outMint[:8]
	}

	log.Debug().
		Str("in", inMint).
		Str("out", outMint).
		Str("in_amount", quote.InAmount).
		Str("out_amount", quote.OutAmount).
		Str("price_impact", quote.PriceImpactPct).
		Int64("latency_ms", latency).
		Msg("jupiter: quote received")

	return &quote, nil
}

// ---------------------------------------------------------------------------
// Swap API — build and execute the swap transaction
// ---------------------------------------------------------------------------

// SwapRequest is the request to Jupiter /swap endpoint.
type SwapRequest struct {
	QuoteResponse                 json.RawMessage `json:"quoteResponse"`
	UserPublicKey                 string          `json:"userPublicKey"`
	WrapAndUnwrapSOL              bool            `json:"wrapAndUnwrapSol"`
	UseSharedAccounts             bool            `json:"useSharedAccounts"`
	ComputeUnitPriceMicroLamports uint64          `json:"computeUnitPriceMicroLamports,omitempty"`
	AsLegacyTransaction           bool            `json:"asLegacyTransaction"`
	DynamicComputeUnitLimit       bool            `json:"dynamicComputeUnitLimit"`
}

// SwapResponse is the response from Jupiter /swap endpoint.
type SwapResponse struct {
	SwapTransaction      string `json:"swapTransaction"` // base64 encoded transaction
	LastValidBlockHeight uint64 `json:"lastValidBlockHeight"`
}

// BuildSwapTx builds a swap transaction from a quote.
func (c *APIClient) BuildSwapTx(ctx context.Context, quote *QuoteResponse, priorityFee uint64) (*SwapResponse, error) {
	if c.circuitOpen.Load() {
		return nil, fmt.Errorf("jupiter: circuit breaker open")
	}

	quoteJSON, err := json.Marshal(quote)
	if err != nil {
		return nil, fmt.Errorf("jupiter: marshal quote: %w", err)
	}

	swapReq := SwapRequest{
		QuoteResponse:                 quoteJSON,
		UserPublicKey:                 c.walletPub,
		WrapAndUnwrapSOL:              true,
		UseSharedAccounts:             true,
		ComputeUnitPriceMicroLamports: priorityFee,
		AsLegacyTransaction:           false,
		DynamicComputeUnitLimit:       true,
	}

	body, err := json.Marshal(swapReq)
	if err != nil {
		return nil, fmt.Errorf("jupiter: marshal swap request: %w", err)
	}

	var swapResp SwapResponse
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(retryBackoff * time.Duration(1<<uint(attempt-1))):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, "POST", jupiterSwapURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("jupiter: create swap request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("jupiter: swap HTTP error: %w", err)
			c.errorCount.Add(1)
			c.recordError()
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("jupiter: read swap response: %w", err)
			c.errorCount.Add(1)
			c.recordError()
			continue
		}

		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("jupiter: swap HTTP %d: %s", resp.StatusCode, string(respBody))
			c.errorCount.Add(1)
			c.recordError()
			continue
		}

		if err := json.Unmarshal(respBody, &swapResp); err != nil {
			return nil, fmt.Errorf("jupiter: parse swap response: %w", err)
		}

		c.resetErrors()
		c.swapCount.Add(1)
		return &swapResp, nil
	}

	return nil, fmt.Errorf("jupiter: swap failed after %d attempts: %w", maxRetries+1, lastErr)
}

// ---------------------------------------------------------------------------
// Price API — get current token price
// ---------------------------------------------------------------------------

// PriceResponse is the response from Jupiter price endpoint.
type PriceResponse struct {
	Data map[string]struct {
		ID         string  `json:"id"`
		MintSymbol string  `json:"mintSymbol"`
		VSToken    string  `json:"vsToken"`
		Price      float64 `json:"price"`
	} `json:"data"`
	TimeTaken float64 `json:"timeTaken"`
}

// GetPrice fetches the current price for a token.
func (c *APIClient) GetPrice(ctx context.Context, mint solana.Pubkey) (decimal.Decimal, error) {
	queryURL, err := url.Parse(jupiterPriceURL)
	if err != nil {
		return decimal.Zero, fmt.Errorf("jupiter: parse URL: %w", err)
	}
	q := queryURL.Query()
	q.Set("ids", string(mint))
	q.Set("vsToken", string(solana.USDCMint))
	queryURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", queryURL.String(), nil)
	if err != nil {
		return decimal.Zero, fmt.Errorf("jupiter: create price request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return decimal.Zero, fmt.Errorf("jupiter: price HTTP error: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return decimal.Zero, fmt.Errorf("jupiter: read price response: %w", err)
	}

	if resp.StatusCode != 200 {
		return decimal.Zero, fmt.Errorf("jupiter: price HTTP %d", resp.StatusCode)
	}

	var priceResp PriceResponse
	if err := json.Unmarshal(body, &priceResp); err != nil {
		return decimal.Zero, fmt.Errorf("jupiter: parse price: %w", err)
	}

	data, ok := priceResp.Data[string(mint)]
	if !ok {
		return decimal.Zero, fmt.Errorf("jupiter: price not found for %s", mint)
	}

	price := decimal.NewFromFloat(data.Price)
	if !price.IsPositive() {
		return decimal.Zero, fmt.Errorf("jupiter: zero/negative price for %s", mint)
	}

	return price, nil
}

// recordError increments consecutive errors and opens circuit breaker.
func (c *APIClient) recordError() {
	count := c.consecutiveErrors.Add(1)
	if count >= 5 {
		if c.circuitOpen.CompareAndSwap(false, true) {
			log.Error().Int64("errors", count).Msg("jupiter: CIRCUIT BREAKER OPEN")
			go func() {
				time.Sleep(30 * time.Second)
				c.circuitOpen.Store(false)
				c.consecutiveErrors.Store(0)
				log.Info().Msg("jupiter: circuit breaker reset")
			}()
		}
	}
}

// resetErrors resets the consecutive error counter.
func (c *APIClient) resetErrors() {
	c.consecutiveErrors.Store(0)
}

// APIStats returns Jupiter API client stats.
type APIStats struct {
	QuoteCount   int64 `json:"quote_count"`
	SwapCount    int64 `json:"swap_count"`
	ErrorCount   int64 `json:"error_count"`
	AvgLatencyMs int64 `json:"avg_latency_ms"`
	CircuitOpen  bool  `json:"circuit_open"`
}

func (c *APIClient) APIStats() APIStats {
	return APIStats{
		QuoteCount:   c.quoteCount.Load(),
		SwapCount:    c.swapCount.Load(),
		ErrorCount:   c.errorCount.Load(),
		AvgLatencyMs: c.avgLatencyMs.Load(),
		CircuitOpen:  c.circuitOpen.Load(),
	}
}
