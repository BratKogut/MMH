package solana

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRPCServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *LiveRPCClient) {
	t.Helper()
	server := httptest.NewServer(handler)
	config := RPCConfig{
		Endpoint:     server.URL,
		WSEndpoint:   "ws://localhost:0", // not used in HTTP tests
		Timeout:      5 * time.Second,
		MaxRetries:   1,
		RateLimitRPS: 100,
	}
	client := NewLiveRPCClient(config)
	t.Cleanup(func() { server.Close() })
	return server, client
}

func TestLiveRPC_Health(t *testing.T) {
	_, client := newTestRPCServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "ok",
		})
	})

	err := client.Health(context.Background())
	assert.NoError(t, err)

	stats := client.Stats()
	assert.Equal(t, int64(1), stats.RequestCount)
}

func TestLiveRPC_GetTokenInfo(t *testing.T) {
	_, client := newTestRPCServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"value": map[string]any{
					"data": map[string]any{
						"parsed": map[string]any{
							"info": map[string]any{
								"decimals":        9,
								"supply":          "1000000000000000",
								"mintAuthority":   "",
								"freezeAuthority": "",
							},
						},
					},
				},
			},
		})
	})

	info, err := client.GetTokenInfo(context.Background(), Pubkey("test-mint"))
	require.NoError(t, err)
	assert.Equal(t, uint8(9), info.Decimals)
	assert.True(t, info.IsMintRenounced())
	assert.True(t, info.IsFreezeRenounced())
}

func TestLiveRPC_GetTopHolders(t *testing.T) {
	_, client := newTestRPCServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		json.NewDecoder(r.Body).Decode(&req)

		if req.Method == "getTokenLargestAccounts" {
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]any{
					"value": []map[string]any{
						{"address": "holder1", "amount": "500000", "decimals": 9, "uiAmount": 0.0005},
						{"address": "holder2", "amount": "300000", "decimals": 9, "uiAmount": 0.0003},
					},
				},
			})
		} else {
			// getAccountInfo for supply lookup.
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]any{
					"value": map[string]any{
						"data": map[string]any{
							"parsed": map[string]any{
								"info": map[string]any{
									"decimals": 9,
									"supply":   "1000000",
								},
							},
						},
					},
				},
			})
		}
	})

	holders, err := client.GetTopHolders(context.Background(), Pubkey("test-mint"), 5)
	require.NoError(t, err)
	assert.Len(t, holders, 2)
	assert.Equal(t, Pubkey("holder1"), holders[0].Address)
}

func TestLiveRPC_GetWalletBalance(t *testing.T) {
	_, client := newTestRPCServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		json.NewDecoder(r.Body).Decode(&req)

		switch req.Method {
		case "getBalance":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  map[string]any{"value": 5000000000}, // 5 SOL
			})
		case "getTokenAccountsByOwner":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  map[string]any{"value": []any{}},
			})
		}
	})

	bal, err := client.GetWalletBalance(context.Background(), Pubkey("test-wallet"))
	require.NoError(t, err)
	assert.Equal(t, "5", bal.SOL.String())
}

func TestLiveRPC_SendTransaction(t *testing.T) {
	_, client := newTestRPCServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "5VERv8NMvzbJMEkV8xnrLkEaWRtSz9CosKDYjCJjBRnbJLgp8uirBgmQpjKhoR4tjF3ZpRzrFmBV6UjKdiSZkQUW",
		})
	})

	sig, err := client.SendTransaction(context.Background(), "base64-tx")
	require.NoError(t, err)
	assert.NotEmpty(t, sig)
}

func TestLiveRPC_GetTransactionStatus(t *testing.T) {
	_, client := newTestRPCServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"value": []map[string]any{
					{"confirmationStatus": "confirmed", "err": nil},
				},
			},
		})
	})

	status, err := client.GetTransactionStatus(context.Background(), Signature("test-sig"))
	require.NoError(t, err)
	assert.Equal(t, "confirmed", status)
}

func TestLiveRPC_RateLimiting(t *testing.T) {
	callCount := 0
	_, client := newTestRPCServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "ok",
		})
	})

	// Rapid fire 5 calls. Rate limiter should allow the initial bucket.
	for i := 0; i < 5; i++ {
		client.Health(context.Background())
	}

	assert.GreaterOrEqual(t, callCount, 3, "Should handle burst within bucket")
}

func TestLiveRPC_RetryOnError(t *testing.T) {
	callCount := 0
	_, client := newTestRPCServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.WriteHeader(500)
			w.Write([]byte("internal error"))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "ok",
		})
	})

	err := client.Health(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, 2, callCount, "Should retry once after failure")
}

func TestLiveRPC_RPCError(t *testing.T) {
	_, client := newTestRPCServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"error": map[string]any{
				"code":    -32600,
				"message": "Invalid request",
			},
		})
	})

	err := client.Health(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Invalid request")
}

func TestLiveRPC_ContextCancellation(t *testing.T) {
	_, client := newTestRPCServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second) // simulate slow response
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := client.Health(ctx)
	assert.Error(t, err)
}

func TestDexProgramID(t *testing.T) {
	assert.NotEmpty(t, dexProgramID("raydium"))
	assert.NotEmpty(t, dexProgramID("pumpfun"))
	assert.NotEmpty(t, dexProgramID("orca"))
	assert.NotEmpty(t, dexProgramID("meteora"))
	assert.Empty(t, dexProgramID("unknown"))
}

func TestProgramIDToDEX(t *testing.T) {
	assert.Equal(t, "raydium", programIDToDEX("675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8"))
	assert.Equal(t, "pumpfun", programIDToDEX("6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P"))
	assert.Equal(t, "unknown", programIDToDEX("something-else"))
}
