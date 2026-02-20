package solana

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// RPC Client Interface
// ---------------------------------------------------------------------------

// RPCClient is the interface for Solana RPC interactions.
// Implementations: LiveRPCClient (real Solana), StubRPCClient (testing).
type RPCClient interface {
	// GetTokenInfo fetches token metadata for a mint address.
	GetTokenInfo(ctx context.Context, mint Pubkey) (*TokenInfo, error)

	// GetTopHolders returns the top N holders of a token.
	GetTopHolders(ctx context.Context, mint Pubkey, limit int) ([]HolderInfo, error)

	// GetWalletBalance returns SOL + SPL token balances for a wallet.
	GetWalletBalance(ctx context.Context, wallet Pubkey) (*WalletBalance, error)

	// GetRecentPools returns pools created within the last N minutes.
	GetRecentPools(ctx context.Context, dex string, sinceMinutes int) ([]PoolInfo, error)

	// GetPoolInfo fetches details for a specific pool.
	GetPoolInfo(ctx context.Context, poolAddress Pubkey) (*PoolInfo, error)

	// SendTransaction submits a signed transaction to the network.
	SendTransaction(ctx context.Context, txBase64 string) (Signature, error)

	// GetTransactionStatus checks if a transaction is confirmed.
	GetTransactionStatus(ctx context.Context, sig Signature) (string, error) // confirmed|finalized|failed

	// SubscribeNewPools opens a websocket subscription for new pool creation events.
	SubscribeNewPools(ctx context.Context, dex string) (<-chan PoolInfo, error)

	// Health returns the RPC endpoint health.
	Health(ctx context.Context) error
}

// RPCConfig configures the Solana RPC client.
type RPCConfig struct {
	Endpoint      string        `yaml:"endpoint"`       // e.g. https://api.mainnet-beta.solana.com
	WSEndpoint    string        `yaml:"ws_endpoint"`    // e.g. wss://api.mainnet-beta.solana.com
	Timeout       time.Duration `yaml:"timeout"`
	MaxRetries    int           `yaml:"max_retries"`
	RateLimitRPS  float64       `yaml:"rate_limit_rps"` // requests per second limit
	PrivateKey    string        `yaml:"private_key"`    // base58 encoded wallet private key
}

// DefaultRPCConfig returns development defaults.
func DefaultRPCConfig() RPCConfig {
	return RPCConfig{
		Endpoint:     "https://api.mainnet-beta.solana.com",
		WSEndpoint:   "wss://api.mainnet-beta.solana.com",
		Timeout:      10 * time.Second,
		MaxRetries:   3,
		RateLimitRPS: 10,
	}
}

// ---------------------------------------------------------------------------
// Stub RPC Client (for testing and development)
// ---------------------------------------------------------------------------

// StubRPCClient is a mock RPC client for testing.
type StubRPCClient struct {
	mu             sync.RWMutex
	tokens         map[Pubkey]*TokenInfo
	pools          map[Pubkey]*PoolInfo
	holders        map[Pubkey][]HolderInfo
	balance        *WalletBalance
	poolChan       chan PoolInfo
	swapResults    []SwapResult
	swapCallCount  int
	failNext       bool
}

// NewStubRPCClient creates a stub RPC client for testing.
func NewStubRPCClient() *StubRPCClient {
	return &StubRPCClient{
		tokens:  make(map[Pubkey]*TokenInfo),
		pools:   make(map[Pubkey]*PoolInfo),
		holders: make(map[Pubkey][]HolderInfo),
		balance: &WalletBalance{
			SOL:    decimal.NewFromFloat(10.0),
			Tokens: make(map[Pubkey]decimal.Decimal),
		},
		poolChan: make(chan PoolInfo, 100),
	}
}

// AddToken registers a token for the stub to return.
func (s *StubRPCClient) AddToken(info TokenInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[info.Mint] = &info
}

// AddPool registers a pool for the stub to return.
func (s *StubRPCClient) AddPool(info PoolInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pools[info.PoolAddress] = &info
}

// AddHolders registers holders for a token mint.
func (s *StubRPCClient) AddHolders(mint Pubkey, holders []HolderInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holders[mint] = holders
}

// EmitPool sends a new pool event on the subscription channel.
func (s *StubRPCClient) EmitPool(pool PoolInfo) {
	s.poolChan <- pool
}

// SetBalance sets the stub wallet balance.
func (s *StubRPCClient) SetBalance(bal WalletBalance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.balance = &bal
}

// SetFailNext makes the next call fail.
func (s *StubRPCClient) SetFailNext() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = true
}

func (s *StubRPCClient) shouldFail() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext {
		s.failNext = false
		return true
	}
	return false
}

// --- Interface implementation ---

func (s *StubRPCClient) GetTokenInfo(_ context.Context, mint Pubkey) (*TokenInfo, error) {
	if s.shouldFail() {
		return nil, fmt.Errorf("stub: simulated RPC failure")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if info, ok := s.tokens[mint]; ok {
		return info, nil
	}
	return nil, fmt.Errorf("stub: token %s not found", mint)
}

func (s *StubRPCClient) GetTopHolders(_ context.Context, mint Pubkey, limit int) ([]HolderInfo, error) {
	if s.shouldFail() {
		return nil, fmt.Errorf("stub: simulated RPC failure")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	holders := s.holders[mint]
	if len(holders) > limit {
		holders = holders[:limit]
	}
	return holders, nil
}

func (s *StubRPCClient) GetWalletBalance(_ context.Context, _ Pubkey) (*WalletBalance, error) {
	if s.shouldFail() {
		return nil, fmt.Errorf("stub: simulated RPC failure")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.balance, nil
}

func (s *StubRPCClient) GetRecentPools(_ context.Context, _ string, _ int) ([]PoolInfo, error) {
	if s.shouldFail() {
		return nil, fmt.Errorf("stub: simulated RPC failure")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	pools := make([]PoolInfo, 0, len(s.pools))
	for _, p := range s.pools {
		pools = append(pools, *p)
	}
	return pools, nil
}

func (s *StubRPCClient) GetPoolInfo(_ context.Context, poolAddress Pubkey) (*PoolInfo, error) {
	if s.shouldFail() {
		return nil, fmt.Errorf("stub: simulated RPC failure")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if info, ok := s.pools[poolAddress]; ok {
		return info, nil
	}
	return nil, fmt.Errorf("stub: pool %s not found", poolAddress)
}

func (s *StubRPCClient) SendTransaction(_ context.Context, _ string) (Signature, error) {
	if s.shouldFail() {
		return "", fmt.Errorf("stub: simulated RPC failure")
	}
	return Signature(fmt.Sprintf("stub-sig-%d", time.Now().UnixNano())), nil
}

func (s *StubRPCClient) GetTransactionStatus(_ context.Context, _ Signature) (string, error) {
	if s.shouldFail() {
		return "", fmt.Errorf("stub: simulated RPC failure")
	}
	return "confirmed", nil
}

func (s *StubRPCClient) SubscribeNewPools(ctx context.Context, _ string) (<-chan PoolInfo, error) {
	if s.shouldFail() {
		return nil, fmt.Errorf("stub: simulated RPC failure")
	}
	out := make(chan PoolInfo, 100)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case pool, ok := <-s.poolChan:
				if !ok {
					return
				}
				out <- pool
			}
		}
	}()
	return out, nil
}

func (s *StubRPCClient) Health(_ context.Context) error {
	if s.shouldFail() {
		return fmt.Errorf("stub: simulated RPC failure")
	}
	return nil
}
