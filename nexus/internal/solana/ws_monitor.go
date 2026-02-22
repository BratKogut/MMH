package solana

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

// ---------------------------------------------------------------------------
// WebSocket Pool Monitor — real-time new pool detection via logsSubscribe
// Subscribes to Raydium/PumpFun program logs to detect pool initialization
// ---------------------------------------------------------------------------

// WSMonitorConfig configures the WebSocket pool monitor.
type WSMonitorConfig struct {
	WSEndpoint       string   `yaml:"ws_endpoint"`
	ProgramIDs       []string `yaml:"program_ids"`       // DEX program IDs to watch
	ReconnectDelayMs int      `yaml:"reconnect_delay_ms"`
	PingIntervalS    int      `yaml:"ping_interval_s"`
	MaxReconnects    int      `yaml:"max_reconnects"`
}

// DefaultWSMonitorConfig returns defaults for mainnet monitoring.
func DefaultWSMonitorConfig() WSMonitorConfig {
	return WSMonitorConfig{
		WSEndpoint: "wss://api.mainnet-beta.solana.com",
		ProgramIDs: []string{
			"675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8", // Raydium AMM V4
			"6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P",  // Pump.fun
		},
		ReconnectDelayMs: 1000,
		PingIntervalS:    30,
		MaxReconnects:    0, // 0 = unlimited reconnects
	}
}

// WSMonitor monitors Solana WebSocket for new pool creation events.
type WSMonitor struct {
	config WSMonitorConfig

	mu   sync.RWMutex
	conn *websocket.Conn
	subs map[int]string // subscription ID -> program ID

	// Output channel for discovered pools.
	poolChan chan PoolEvent
	closed   atomic.Bool // tracks if poolChan is closed

	// Subscription ID counter.
	nextSubID atomic.Int64

	// Stats.
	messagesRecv  atomic.Int64
	poolsDetected atomic.Int64
	reconnects    atomic.Int64
	connected     atomic.Bool
}

// PoolEvent is emitted when a new pool creation is detected.
type PoolEvent struct {
	ProgramID  string    `json:"program_id"`
	Signature  string    `json:"signature"`
	Slot       uint64    `json:"slot"`
	Logs       []string  `json:"logs"`
	DEX        string    `json:"dex"`
	DetectedAt time.Time `json:"detected_at"`
}

// NewWSMonitor creates a new WebSocket pool monitor.
func NewWSMonitor(config WSMonitorConfig) *WSMonitor {
	return &WSMonitor{
		config:   config,
		subs:     make(map[int]string),
		poolChan: make(chan PoolEvent, 256),
	}
}

// Start connects to the WebSocket and starts monitoring.
// Returns a channel that emits PoolEvents. Blocks until ctx is cancelled.
func (m *WSMonitor) Start(ctx context.Context) (<-chan PoolEvent, error) {
	go m.runLoop(ctx)
	return m.poolChan, nil
}

func (m *WSMonitor) runLoop(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Error().Interface("panic", r).Msg("ws: runLoop panic recovered")
		}
		// Acquire write lock to synchronize with handleMessage's channel send.
		m.mu.Lock()
		if m.closed.CompareAndSwap(false, true) {
			close(m.poolChan)
		}
		m.mu.Unlock()
	}()

	reconnectDelay := time.Duration(m.config.ReconnectDelayMs) * time.Millisecond
	reconnectCount := 0

	for {
		select {
		case <-ctx.Done():
			m.disconnect()
			return
		default:
		}

		// Unlimited reconnects when MaxReconnects == 0.
		if m.config.MaxReconnects > 0 && reconnectCount >= m.config.MaxReconnects {
			log.Error().Int("max", m.config.MaxReconnects).Msg("ws: max reconnects reached, restarting counter after cooldown")
			select {
			case <-time.After(60 * time.Second):
				reconnectCount = 0
				continue
			case <-ctx.Done():
				m.disconnect()
				return
			}
		}

		if err := m.connect(ctx); err != nil {
			log.Warn().Err(err).Int("attempt", reconnectCount).Msg("ws: connection failed")
			reconnectCount++
			m.reconnects.Add(1)

			maxDelay := 30 * time.Second
			if reconnectDelay > maxDelay {
				reconnectDelay = maxDelay
			}
			select {
			case <-time.After(reconnectDelay):
				reconnectDelay = reconnectDelay * 2
				if reconnectDelay > maxDelay {
					reconnectDelay = maxDelay
				}
			case <-ctx.Done():
				return
			}
			continue
		}

		reconnectCount = 0
		reconnectDelay = time.Duration(m.config.ReconnectDelayMs) * time.Millisecond

		// Subscribe to all program IDs.
		for _, programID := range m.config.ProgramIDs {
			if err := m.subscribe(programID); err != nil {
				pid := programID
				if len(pid) > 8 {
					pid = pid[:8]
				}
				log.Warn().Err(err).Str("program", pid).Msg("ws: subscribe failed")
			}
		}

		// Read messages until disconnect.
		m.readLoop(ctx)
	}
}

func (m *WSMonitor) connect(ctx context.Context) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	header := http.Header{}
	conn, _, err := dialer.DialContext(ctx, m.config.WSEndpoint, header)
	if err != nil {
		return fmt.Errorf("ws: dial: %w", err)
	}

	m.mu.Lock()
	m.conn = conn
	m.subs = make(map[int]string)
	m.mu.Unlock()
	m.connected.Store(true)

	log.Info().Str("endpoint", m.config.WSEndpoint).Msg("ws: connected")
	return nil
}

func (m *WSMonitor) disconnect() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn != nil {
		m.conn.Close()
		m.conn = nil
	}
	m.connected.Store(false)
}

// subscribe sends a logsSubscribe RPC request for a program.
func (m *WSMonitor) subscribe(programID string) error {
	m.mu.RLock()
	conn := m.conn
	m.mu.RUnlock()
	if conn == nil {
		return fmt.Errorf("ws: not connected")
	}

	subID := m.nextSubID.Add(1)

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      subID,
		"method":  "logsSubscribe",
		"params": []any{
			map[string]any{
				"mentions": []string{programID},
			},
			map[string]any{
				"commitment": "confirmed",
			},
		},
	}

	m.mu.Lock()
	err := m.conn.WriteJSON(req)
	m.mu.Unlock()

	if err != nil {
		return fmt.Errorf("ws: write subscribe: %w", err)
	}

	pid := programID
	if len(pid) > 8 {
		pid = pid[:8]
	}
	log.Info().
		Str("program", pid).
		Str("dex", programIDToDEX(programID)).
		Msg("ws: subscribed to program logs")

	return nil
}

func (m *WSMonitor) readLoop(ctx context.Context) {
	// Ping ticker.
	pingInterval := time.Duration(m.config.PingIntervalS) * time.Second
	if pingInterval == 0 {
		pingInterval = 30 * time.Second
	}
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-pingTicker.C:
			m.mu.RLock()
			conn := m.conn
			m.mu.RUnlock()
			if conn != nil {
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					log.Debug().Err(err).Msg("ws: ping failed")
					return
				}
			}
		default:
		}

		m.mu.RLock()
		conn := m.conn
		m.mu.RUnlock()
		if conn == nil {
			return
		}

		conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				log.Info().Msg("ws: connection closed normally")
			} else {
				log.Warn().Err(err).Msg("ws: read error, reconnecting")
			}
			m.connected.Store(false)
			return
		}

		m.messagesRecv.Add(1)
		m.handleMessage(message)
	}
}

func (m *WSMonitor) handleMessage(data []byte) {
	defer func() {
		if r := recover(); r != nil {
			log.Error().Interface("panic", r).Msg("ws: handleMessage panic recovered")
		}
	}()

	// Parse subscription notification.
	var notification struct {
		Method string `json:"method"`
		Params struct {
			Result struct {
				Value struct {
					Signature string   `json:"signature"`
					Logs      []string `json:"logs"`
				} `json:"value"`
				Context struct {
					Slot uint64 `json:"slot"`
				} `json:"context"`
			} `json:"result"`
			Subscription int `json:"subscription"`
		} `json:"params"`
	}

	if err := json.Unmarshal(data, &notification); err != nil {
		return
	}

	if notification.Method != "logsNotification" {
		// Could be a subscription confirmation response.
		var subResp struct {
			Result int `json:"result"`
		}
		if json.Unmarshal(data, &subResp) == nil && subResp.Result > 0 {
			log.Debug().Int("sub_id", subResp.Result).Msg("ws: subscription confirmed")
		}
		return
	}

	logs := notification.Params.Result.Value.Logs
	sig := notification.Params.Result.Value.Signature
	slot := notification.Params.Result.Context.Slot

	// Check if this is a pool initialization event.
	if !isPoolCreationEvent(logs) {
		return
	}

	// Determine DEX from log patterns.
	dex := detectDEXFromLogs(logs)

	event := PoolEvent{
		Signature:  sig,
		Slot:       slot,
		Logs:       logs,
		DEX:        dex,
		DetectedAt: time.Now(),
	}

	m.poolsDetected.Add(1)

	sigPrefix := sig
	if len(sigPrefix) > 12 {
		sigPrefix = sigPrefix[:12]
	}

	// Synchronize channel send with close using mutex to prevent
	// send-on-closed-channel panic (atomic check alone is racy).
	m.mu.RLock()
	closed := m.closed.Load()
	if !closed {
		select {
		case m.poolChan <- event:
			log.Info().
				Str("sig", sigPrefix).
				Str("dex", dex).
				Uint64("slot", slot).
				Msg("ws: NEW POOL DETECTED")
		default:
			log.Warn().Msg("ws: pool channel full, dropping event")
		}
	}
	m.mu.RUnlock()
}

// isPoolCreationEvent checks logs for pool initialization markers.
func isPoolCreationEvent(logs []string) bool {
	hasCreate := false
	hasInitMint := false

	for _, l := range logs {
		// Raydium AMM pool init.
		if strings.Contains(l, "InitializeInstruction2") || strings.Contains(l, "initialize2") {
			return true
		}
		// Orca Whirlpool init.
		if strings.Contains(l, "InitializePool") {
			return true
		}
		// Meteora DLMM.
		if strings.Contains(l, "InitializeLbPair") {
			return true
		}
		// Pump.fun markers (may be on separate log lines).
		if strings.Contains(l, "Create") {
			hasCreate = true
		}
		if strings.Contains(l, "InitializeMint2") {
			hasInitMint = true
		}
	}

	// Pump.fun bonding curve creation requires both markers.
	return hasCreate && hasInitMint
}

// detectDEXFromLogs determines which DEX created the pool.
func detectDEXFromLogs(logs []string) string {
	for _, l := range logs {
		if strings.Contains(l, "675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8") {
			return "raydium"
		}
		if strings.Contains(l, "6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P") {
			return "pumpfun"
		}
		if strings.Contains(l, "whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc") {
			return "orca"
		}
		if strings.Contains(l, "LBUZKhRxPF3XUpBCjp4YzTKgLccjZhTSDM9YuVaPwxo") {
			return "meteora"
		}
	}
	return "unknown"
}

// WSStats returns monitor statistics.
type WSStats struct {
	Connected     bool  `json:"connected"`
	MessagesRecv  int64 `json:"messages_recv"`
	PoolsDetected int64 `json:"pools_detected"`
	Reconnects    int64 `json:"reconnects"`
}

func (m *WSMonitor) Stats() WSStats {
	return WSStats{
		Connected:     m.connected.Load(),
		MessagesRecv:  m.messagesRecv.Load(),
		PoolsDetected: m.poolsDetected.Load(),
		Reconnects:    m.reconnects.Load(),
	}
}
