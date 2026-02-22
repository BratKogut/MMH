package graph

// ---------------------------------------------------------------------------
// CEX Hot Wallet Database — known exchange addresses
// Architecture v3.1: ~200 known CEX addresses, edge cut on known exchanges
// ---------------------------------------------------------------------------

// CEXWallets maps known centralized exchange hot wallet addresses.
// Transfer to/from CEX = stop graph traversal, don't propagate labels.
var CEXWallets = map[string]string{
	// Binance
	"5tzFkiKscXHK5ZXCGbXZxdw7gTjjD1mBwuoFbhUvuAi9": "binance",
	"9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM": "binance",
	"2ojv9BAiHUrvsm9gxDe7fJSzbNZSJcxZvf8dqmWGHG8S": "binance",
	"3yFwqXBfZY4jBVUafQ1YEXw189y2dN3V5KQq9uzBDy1E": "binance",
	"HN7cABqLq46Es1jh92dQQisAq662SmxELLLsHHe4YWrH": "binance",

	// Coinbase
	"GJRs4FwHtemZ5ZE9x3FNvJ8TMwitKTh21yxdRPqn7npE": "coinbase",
	"H8sMJSCQxfKiFTCfDR3DUMLPwcRbM61LGFJ8N4dK3WjS": "coinbase",
	"2AQdpHJ2JpcEgPiATUXjQxA8QmafFegfQwSLWSprPicm": "coinbase",

	// Kraken
	"FWznbcNXWQuHTawe9RxvQ2LdCENssh12dsznf4RiouN5": "kraken",

	// OKX
	"5VCwKtCXgCJ6kit5FybXjvFnPXCrKoKwFqgq5YVe1rAS": "okx",
	"GBCxMjyaNya5cQk7rAFj6AeUQRYXs2NxaVyUgQsq87nS": "okx",

	// Bybit
	"AC5RDfQFmDS1deWZos921JfqscXdByf6BKHAbETSYnh7": "bybit",

	// Gate.io
	"u6PJ8DtQuPFnfmwHbGFULQ4u4EgjDiyYKjVEsynXq2w":  "gateio",

	// KuCoin
	"BmFdpraQhkiDQE6SnfG5PVddTtR3GYBnCkEHAowHvPLJ": "kucoin",

	// Raydium Authority (not CEX but high-traffic program account)
	"5Q544fKrFoe6tsEbD7S8EmxGTJYAKtTVhAW5Q5pge4j1": "raydium_authority",

	// Jupiter Aggregator
	"JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4": "jupiter_aggregator",
}

// IsCEXWallet checks if an address is a known CEX hot wallet.
func IsCEXWallet(address string) (string, bool) {
	exchange, ok := CEXWallets[address]
	return exchange, ok
}

// AddCEXWallet adds a CEX wallet address at runtime.
func AddCEXWallet(address, exchange string) {
	CEXWallets[address] = exchange
}

// CEXWalletCount returns the number of known CEX wallets.
func CEXWalletCount() int {
	return len(CEXWallets)
}

// ShouldCutEdge returns true if a graph edge should be cut (not traversed)
// because it involves a CEX wallet. This prevents label propagation through
// exchanges (CEX chain breaking evasion mitigation).
func ShouldCutEdge(from, to string) bool {
	_, fromCEX := CEXWallets[from]
	_, toCEX := CEXWallets[to]
	return fromCEX || toCEX
}
