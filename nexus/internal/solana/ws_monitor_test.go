package solana

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsPoolCreationEvent_Raydium(t *testing.T) {
	logs := []string{
		"Program 675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8 invoke [1]",
		"Program log: InitializeInstruction2",
		"Program 675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8 success",
	}

	assert.True(t, isPoolCreationEvent(logs))
}

func TestIsPoolCreationEvent_PumpFun(t *testing.T) {
	logs := []string{
		"Program 6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P invoke [1]",
		"Program log: Create",
		"Program log: InitializeMint2",
	}

	assert.True(t, isPoolCreationEvent(logs))
}

func TestIsPoolCreationEvent_Orca(t *testing.T) {
	logs := []string{
		"Program whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc invoke [1]",
		"Program log: InitializePool",
	}

	assert.True(t, isPoolCreationEvent(logs))
}

func TestIsPoolCreationEvent_NotPoolCreation(t *testing.T) {
	logs := []string{
		"Program 675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8 invoke [1]",
		"Program log: Swap",
		"Program 675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8 success",
	}

	assert.False(t, isPoolCreationEvent(logs))
}

func TestDetectDEXFromLogs(t *testing.T) {
	t.Run("raydium", func(t *testing.T) {
		logs := []string{"Program 675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8 invoke"}
		assert.Equal(t, "raydium", detectDEXFromLogs(logs))
	})

	t.Run("pumpfun", func(t *testing.T) {
		logs := []string{"Program 6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P invoke"}
		assert.Equal(t, "pumpfun", detectDEXFromLogs(logs))
	})

	t.Run("orca", func(t *testing.T) {
		logs := []string{"Program whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc invoke"}
		assert.Equal(t, "orca", detectDEXFromLogs(logs))
	})

	t.Run("meteora", func(t *testing.T) {
		logs := []string{"Program LBUZKhRxPF3XUpBCjp4YzTKgLccjZhTSDM9YuVaPwxo invoke"}
		assert.Equal(t, "meteora", detectDEXFromLogs(logs))
	})

	t.Run("unknown", func(t *testing.T) {
		logs := []string{"something else"}
		assert.Equal(t, "unknown", detectDEXFromLogs(logs))
	})
}

func TestWSMonitor_NewWSMonitor(t *testing.T) {
	config := DefaultWSMonitorConfig()
	monitor := NewWSMonitor(config)

	assert.NotNil(t, monitor)
	assert.NotNil(t, monitor.poolChan)

	stats := monitor.Stats()
	assert.False(t, stats.Connected)
	assert.Equal(t, int64(0), stats.PoolsDetected)
}

func TestWSMonitorConfig_Defaults(t *testing.T) {
	config := DefaultWSMonitorConfig()

	assert.NotEmpty(t, config.WSEndpoint)
	assert.Len(t, config.ProgramIDs, 2)
	assert.Equal(t, 1000, config.ReconnectDelayMs)
	assert.Equal(t, 30, config.PingIntervalS)
	assert.Equal(t, 0, config.MaxReconnects) // 0 = unlimited reconnects
}

func TestStringContains(t *testing.T) {
	assert.True(t, strings.Contains("hello world", "world"))
	assert.True(t, strings.Contains("hello world", "hello"))
	assert.False(t, strings.Contains("hello", "world"))
	assert.False(t, strings.Contains("hi", "hello"))
}
