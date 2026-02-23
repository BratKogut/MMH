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
	Hunter     HunterConfig     `yaml:"hunter"`
	Solana     SolanaConfig     `yaml:"solana"`
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

// SolanaConfig configures Solana RPC connection.
type SolanaConfig struct {
	RPCEndpoint  string  `yaml:"rpc_endpoint"`
	WSEndpoint   string  `yaml:"ws_endpoint"`
	PrivateKey   string  `yaml:"private_key"`   // base58 encoded
	WalletPubkey string  `yaml:"wallet_pubkey"` // base58 public key (if empty, derived from private_key context)
	RateLimitRPS float64 `yaml:"rate_limit_rps"`
}

// TrackedWalletConfig configures a tracked wallet for copy-trade intelligence.
type TrackedWalletConfig struct {
	Address string `yaml:"address"`
	Tier    string `yaml:"tier"`  // WHALE|SMART_MONEY|KOL|INSIDER|FRESH
	Label   string `yaml:"label"` // human-readable label
}

// HunterConfig configures the memecoin hunter/sniper.
type HunterConfig struct {
	Enabled              bool                  `yaml:"enabled"`
	DryRun               bool                  `yaml:"dry_run"`
	MonitorDEXes         []string              `yaml:"monitor_dexes"`
	MaxBuySOL            float64               `yaml:"max_buy_sol"`
	SlippageBps          int                   `yaml:"slippage_bps"`
	TakeProfitMultiplier float64               `yaml:"take_profit_multiplier"`
	StopLossPct          float64               `yaml:"stop_loss_pct"`
	TrailingStopEnabled  bool                  `yaml:"trailing_stop_enabled"`
	TrailingStopPct      float64               `yaml:"trailing_stop_pct"`
	MaxPositions         int                   `yaml:"max_positions"`
	MaxDailyLossSOL      float64               `yaml:"max_daily_loss_sol"`
	MaxDailySpendSOL     float64               `yaml:"max_daily_spend_sol"`
	MinSafetyScore       int                   `yaml:"min_safety_score"`
	MinLiquidityUSD      float64               `yaml:"min_liquidity_usd"`
	MaxTokenAgeMinutes   int                   `yaml:"max_token_age_minutes"`
	AutoSellAfterMinutes int                   `yaml:"auto_sell_after_minutes"`
	UseJito              bool                  `yaml:"use_jito"`
	JitoTipSOL           float64               `yaml:"jito_tip_sol"`
	PriorityFee          uint64                `yaml:"priority_fee"`
	TrackedWallets       []TrackedWalletConfig `yaml:"tracked_wallets"`
}

// Validate checks the configuration for invalid or dangerous values.
func (cfg *Config) Validate() error {
	// Hunter validation.
	h := cfg.Hunter
	if h.Enabled {
		if h.MaxBuySOL <= 0 {
			return fmt.Errorf("config: hunter.max_buy_sol must be > 0 (got %f)", h.MaxBuySOL)
		}
		if h.MaxBuySOL > 10 {
			return fmt.Errorf("config: hunter.max_buy_sol=%f is dangerously high (max recommended: 10 SOL)", h.MaxBuySOL)
		}
		if h.TakeProfitMultiplier <= 1.0 {
			return fmt.Errorf("config: hunter.take_profit_multiplier must be > 1.0 (got %f)", h.TakeProfitMultiplier)
		}
		if h.StopLossPct <= 0 || h.StopLossPct > 100 {
			return fmt.Errorf("config: hunter.stop_loss_pct must be >0 and <=100 (got %f)", h.StopLossPct)
		}
		if h.SlippageBps <= 0 || h.SlippageBps > 5000 {
			return fmt.Errorf("config: hunter.slippage_bps must be 1-5000 (got %d)", h.SlippageBps)
		}
		if h.MaxPositions <= 0 {
			return fmt.Errorf("config: hunter.max_positions must be > 0 (got %d)", h.MaxPositions)
		}
		if h.MaxDailySpendSOL <= 0 {
			return fmt.Errorf("config: hunter.max_daily_spend_sol must be > 0 (got %f)", h.MaxDailySpendSOL)
		}
		if h.MaxDailyLossSOL <= 0 {
			return fmt.Errorf("config: hunter.max_daily_loss_sol must be > 0 (got %f)", h.MaxDailyLossSOL)
		}
	}

	// Solana validation.
	s := cfg.Solana
	if s.RPCEndpoint == "" {
		return fmt.Errorf("config: solana.rpc_endpoint must be set")
	}
	if s.RateLimitRPS <= 0 || s.RateLimitRPS > 1000 {
		return fmt.Errorf("config: solana.rate_limit_rps must be 1-1000 (got %f)", s.RateLimitRPS)
	}

	// Warn about dry_run.
	if !cfg.Hunter.DryRun && cfg.Hunter.Enabled {
		if s.PrivateKey == "" {
			return fmt.Errorf("config: solana.private_key required when hunter.dry_run=false")
		}
		if s.WalletPubkey == "" {
			return fmt.Errorf("config: solana.wallet_pubkey required when hunter.dry_run=false")
		}
	}

	return nil
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
	// Solana defaults.
	if cfg.Solana.RPCEndpoint == "" {
		cfg.Solana.RPCEndpoint = "https://api.mainnet-beta.solana.com"
	}
	if cfg.Solana.WSEndpoint == "" {
		cfg.Solana.WSEndpoint = "wss://api.mainnet-beta.solana.com"
	}
	if cfg.Solana.RateLimitRPS == 0 {
		cfg.Solana.RateLimitRPS = 10
	}
	// Hunter defaults.
	if len(cfg.Hunter.MonitorDEXes) == 0 {
		cfg.Hunter.MonitorDEXes = []string{"raydium", "pumpfun"}
	}
	if cfg.Hunter.MaxBuySOL == 0 {
		cfg.Hunter.MaxBuySOL = 0.1
	}
	if cfg.Hunter.SlippageBps == 0 {
		cfg.Hunter.SlippageBps = 200
	}
	if cfg.Hunter.TakeProfitMultiplier == 0 {
		cfg.Hunter.TakeProfitMultiplier = 2.0
	}
	if cfg.Hunter.StopLossPct == 0 {
		cfg.Hunter.StopLossPct = 50
	}
	if cfg.Hunter.TrailingStopPct == 0 {
		cfg.Hunter.TrailingStopPct = 20
	}
	if cfg.Hunter.MaxPositions == 0 {
		cfg.Hunter.MaxPositions = 5
	}
	if cfg.Hunter.MaxDailyLossSOL == 0 {
		cfg.Hunter.MaxDailyLossSOL = 1.0
	}
	if cfg.Hunter.MaxDailySpendSOL == 0 {
		cfg.Hunter.MaxDailySpendSOL = 2.0
	}
	if cfg.Hunter.MinSafetyScore == 0 {
		cfg.Hunter.MinSafetyScore = 40
	}
	if cfg.Hunter.MinLiquidityUSD == 0 {
		cfg.Hunter.MinLiquidityUSD = 500
	}
	if cfg.Hunter.MaxTokenAgeMinutes == 0 {
		cfg.Hunter.MaxTokenAgeMinutes = 30
	}
}
