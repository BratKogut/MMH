package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration structure for NEXUS.
type Config struct {
	General    GeneralConfig    `yaml:"general"`
	Kafka      KafkaConfig      `yaml:"kafka"`
	ClickHouse ClickHouseConfig `yaml:"clickhouse"`
	Exchanges  ExchangesConfig  `yaml:"exchanges"`
	Risk       RiskConfig       `yaml:"risk"`
	Intel      IntelConfig      `yaml:"intel"`
	Metrics    MetricsConfig    `yaml:"metrics"`
}

type GeneralConfig struct {
	InstanceID  string `yaml:"instance_id"`
	Environment string `yaml:"environment"` // production|staging|development
	DryRun      bool   `yaml:"dry_run"`
	LogLevel    string `yaml:"log_level"`
	LogFormat   string `yaml:"log_format"` // json|text
}

type KafkaConfig struct {
	Brokers        []string `yaml:"brokers"`
	SchemaVersion  string   `yaml:"schema_version"`
	ProducerConfig struct {
		Acks            string `yaml:"acks"`             // all|1|0
		LingerMs        int    `yaml:"linger_ms"`
		BatchSize       int    `yaml:"batch_size"`
		CompressionType string `yaml:"compression_type"` // snappy|lz4|zstd|none
	} `yaml:"producer"`
	ConsumerConfig struct {
		GroupIDPrefix   string `yaml:"group_id_prefix"`
		AutoOffsetReset string `yaml:"auto_offset_reset"` // earliest|latest
		MaxPollRecords  int    `yaml:"max_poll_records"`
	} `yaml:"consumer"`
}

type ClickHouseConfig struct {
	DSN          string `yaml:"dsn"`
	Database     string `yaml:"database"`
	MaxOpenConns int    `yaml:"max_open_conns"`
	MaxIdleConns int    `yaml:"max_idle_conns"`
}

type ExchangesConfig struct {
	Enabled []string             `yaml:"enabled"`
	Kraken  ExchangeDetailConfig `yaml:"kraken"`
	Binance ExchangeDetailConfig `yaml:"binance"`
}

type ExchangeDetailConfig struct {
	WSURL        string   `yaml:"ws_url"`
	RESTURL      string   `yaml:"rest_url"`
	APIKey       string   `yaml:"api_key"`
	APISecret    string   `yaml:"api_secret"`
	Symbols      []string `yaml:"symbols"`
	RateLimitRPS float64  `yaml:"rate_limit_rps"`
}

type RiskConfig struct {
	MaxDailyLossUSD           float64 `yaml:"max_daily_loss_usd"`
	MaxTotalExposureUSD       float64 `yaml:"max_total_exposure_usd"`
	MaxPositionPerSymbol      float64 `yaml:"max_position_per_symbol"`
	MaxNotionalPerOrder       float64 `yaml:"max_notional_per_order"`
	CooldownAfterLosses       int     `yaml:"cooldown_after_losses"`
	CooldownMinutes           int     `yaml:"cooldown_minutes"`
	FeedLagThresholdMs        int     `yaml:"feed_lag_threshold_ms"`
	OrderRejectSpikeThreshold int     `yaml:"order_reject_spike_threshold"`
}

type IntelConfig struct {
	Enabled            bool    `yaml:"enabled"`
	MaxEventsPerHour   int     `yaml:"max_events_per_hour"`
	DailyBudgetUSD     float64 `yaml:"daily_budget_usd"`
	InternalLLMAddress string  `yaml:"internal_llm_address"`
	GrokEnabled        bool    `yaml:"grok_enabled"`
	GrokAPIKey         string  `yaml:"grok_api_key"`
	ClaudeEnabled      bool    `yaml:"claude_enabled"`
	ClaudeAPIKey       string  `yaml:"claude_api_key"`
}

type MetricsConfig struct {
	PrometheusPort int  `yaml:"prometheus_port"`
	Enabled        bool `yaml:"enabled"`
}

// Load reads and parses a YAML configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	// Expand environment variables
	expanded := os.ExpandEnv(string(data))

	cfg := &Config{}
	if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Apply defaults
	applyDefaults(cfg)

	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.General.InstanceID == "" {
		cfg.General.InstanceID = "nexus-1"
	}
	if cfg.General.Environment == "" {
		cfg.General.Environment = "development"
	}
	if cfg.General.LogLevel == "" {
		cfg.General.LogLevel = "info"
	}
	if cfg.General.LogFormat == "" {
		cfg.General.LogFormat = "json"
	}
	if len(cfg.Kafka.Brokers) == 0 {
		cfg.Kafka.Brokers = []string{"localhost:9092"}
	}
	if cfg.Kafka.ProducerConfig.Acks == "" {
		cfg.Kafka.ProducerConfig.Acks = "all"
	}
	if cfg.Kafka.ProducerConfig.CompressionType == "" {
		cfg.Kafka.ProducerConfig.CompressionType = "snappy"
	}
	if cfg.Kafka.ConsumerConfig.AutoOffsetReset == "" {
		cfg.Kafka.ConsumerConfig.AutoOffsetReset = "earliest"
	}
	if cfg.ClickHouse.DSN == "" {
		cfg.ClickHouse.DSN = "clickhouse://localhost:9000/nexus"
	}
	if cfg.ClickHouse.Database == "" {
		cfg.ClickHouse.Database = "nexus"
	}
	if cfg.ClickHouse.MaxOpenConns == 0 {
		cfg.ClickHouse.MaxOpenConns = 10
	}
	if cfg.Metrics.PrometheusPort == 0 {
		cfg.Metrics.PrometheusPort = 9090
	}
	if cfg.Risk.MaxDailyLossUSD == 0 {
		cfg.Risk.MaxDailyLossUSD = 1000
	}
	if cfg.Risk.MaxTotalExposureUSD == 0 {
		cfg.Risk.MaxTotalExposureUSD = 10000
	}
	if cfg.Risk.FeedLagThresholdMs == 0 {
		cfg.Risk.FeedLagThresholdMs = 5000
	}
}
