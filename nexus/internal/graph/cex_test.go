package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsCEXWallet(t *testing.T) {
	// Known Binance address.
	exchange, ok := IsCEXWallet("5tzFkiKscXHK5ZXCGbXZxdw7gTjjD1mBwuoFbhUvuAi9")
	assert.True(t, ok)
	assert.Equal(t, "binance", exchange)

	// Known Coinbase address.
	exchange, ok = IsCEXWallet("GJRs4FwHtemZ5ZE9x3FNvJ8TMwitKTh21yxdRPqn7npE")
	assert.True(t, ok)
	assert.Equal(t, "coinbase", exchange)

	// Unknown address.
	_, ok = IsCEXWallet("RandomWalletXYZ123")
	assert.False(t, ok)
}

func TestShouldCutEdge(t *testing.T) {
	// From CEX.
	assert.True(t, ShouldCutEdge("5tzFkiKscXHK5ZXCGbXZxdw7gTjjD1mBwuoFbhUvuAi9", "random_wallet"))
	// To CEX.
	assert.True(t, ShouldCutEdge("random_wallet", "GJRs4FwHtemZ5ZE9x3FNvJ8TMwitKTh21yxdRPqn7npE"))
	// Neither.
	assert.False(t, ShouldCutEdge("wallet_a", "wallet_b"))
}

func TestCEXWalletCount(t *testing.T) {
	count := CEXWalletCount()
	assert.True(t, count >= 15, "expected at least 15 CEX wallets, got %d", count)
}

func TestAddCEXWallet(t *testing.T) {
	addr := "TestCEXWallet_" + t.Name()
	AddCEXWallet(addr, "test_exchange")

	exchange, ok := IsCEXWallet(addr)
	assert.True(t, ok)
	assert.Equal(t, "test_exchange", exchange)

	// Cleanup.
	delete(CEXWallets, addr)
}
