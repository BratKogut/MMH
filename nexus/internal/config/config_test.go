package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig(t *testing.T) {
	yaml := `
general:
  instance_id: "test-node"
  environment: "development"
  dry_run: true
  log_level: "debug"

kafka:
  brokers:
    - "localhost:19092"
  schema_version: "1.0.0"

clickhouse:
  dsn: "clickhouse://localhost:9000/nexus_test"

exchanges:
  enabled:
    - kraken
    - binance
  kraken:
    ws_url: "wss://ws.kraken.com/v2"
    symbols:
      - "BTC/USD"
      - "ETH/USD"

risk:
  max_daily_loss_usd: 500
  max_total_exposure_usd: 5000
`
	tmpFile, err := os.CreateTemp("", "nexus-config-*.yaml")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(yaml)
	require.NoError(t, err)
	tmpFile.Close()

	cfg, err := Load(tmpFile.Name())
	require.NoError(t, err)

	assert.Equal(t, "test-node", cfg.General.InstanceID)
	assert.Equal(t, "development", cfg.General.Environment)
	assert.True(t, cfg.General.DryRun)
	assert.Equal(t, []string{"localhost:19092"}, cfg.Kafka.Brokers)
	assert.Equal(t, []string{"kraken", "binance"}, cfg.Exchanges.Enabled)
	assert.Equal(t, 500.0, cfg.Risk.MaxDailyLossUSD)
	assert.Equal(t, "wss://ws.kraken.com/v2", cfg.Exchanges.Kraken.WSURL)
}

func TestLoadConfigDefaults(t *testing.T) {
	yaml := `
general:
  dry_run: true
`
	tmpFile, err := os.CreateTemp("", "nexus-config-*.yaml")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(yaml)
	require.NoError(t, err)
	tmpFile.Close()

	cfg, err := Load(tmpFile.Name())
	require.NoError(t, err)

	assert.Equal(t, "nexus-1", cfg.General.InstanceID)
	assert.Equal(t, "info", cfg.General.LogLevel)
	assert.Equal(t, []string{"localhost:9092"}, cfg.Kafka.Brokers)
	assert.Equal(t, "all", cfg.Kafka.ProducerConfig.Acks)
	assert.Equal(t, 1000.0, cfg.Risk.MaxDailyLossUSD)
}

func TestLoadConfigEnvExpansion(t *testing.T) {
	os.Setenv("TEST_NEXUS_INSTANCE", "env-node")
	defer os.Unsetenv("TEST_NEXUS_INSTANCE")

	yaml := `
general:
  instance_id: "${TEST_NEXUS_INSTANCE}"
`
	tmpFile, err := os.CreateTemp("", "nexus-config-*.yaml")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(yaml)
	require.NoError(t, err)
	tmpFile.Close()

	cfg, err := Load(tmpFile.Name())
	require.NoError(t, err)

	assert.Equal(t, "env-node", cfg.General.InstanceID)
}
