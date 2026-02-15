package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/rs/zerolog/log"
)

// Client wraps a ClickHouse native protocol connection with configuration
// and lifecycle management.
type Client struct {
	conn driver.Conn
}

// ClientOption configures a Client.
type ClientOption func(*clientConfig)

type clientConfig struct {
	maxOpenConns    int
	maxIdleConns    int
	connMaxLifetime time.Duration
	dialTimeout     time.Duration
	debug           bool
}

// WithMaxOpenConns sets the maximum number of open connections.
func WithMaxOpenConns(n int) ClientOption {
	return func(c *clientConfig) { c.maxOpenConns = n }
}

// WithMaxIdleConns sets the maximum number of idle connections in the pool.
func WithMaxIdleConns(n int) ClientOption {
	return func(c *clientConfig) { c.maxIdleConns = n }
}

// WithConnMaxLifetime sets the maximum lifetime of a connection before it is recycled.
func WithConnMaxLifetime(d time.Duration) ClientOption {
	return func(c *clientConfig) { c.connMaxLifetime = d }
}

// WithDialTimeout sets the connection dial timeout.
func WithDialTimeout(d time.Duration) ClientOption {
	return func(c *clientConfig) { c.dialTimeout = d }
}

// WithDebug enables debug logging on the ClickHouse connection.
func WithDebug(enabled bool) ClientOption {
	return func(c *clientConfig) { c.debug = enabled }
}

// NewClient creates a new ClickHouse client from a DSN string.
// The DSN format is: clickhouse://user:password@host:port/database?param=value
// ClientOptions can override specific settings parsed from the DSN.
func NewClient(dsn string, opts ...ClientOption) (*Client, error) {
	cfg := &clientConfig{
		maxOpenConns:    10,
		maxIdleConns:    5,
		connMaxLifetime: 10 * time.Minute,
		dialTimeout:     5 * time.Second,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	options, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse clickhouse dsn: %w", err)
	}

	// Apply ClientOption overrides.
	options.MaxOpenConns = cfg.maxOpenConns
	options.MaxIdleConns = cfg.maxIdleConns
	options.ConnMaxLifetime = cfg.connMaxLifetime
	options.DialTimeout = cfg.dialTimeout
	options.Debug = cfg.debug

	// Default to LZ4 compression if not already set via DSN.
	if options.Compression == nil {
		options.Compression = &clickhouse.Compression{
			Method: clickhouse.CompressionLZ4,
		}
	}

	conn, err := clickhouse.Open(options)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse connection: %w", err)
	}

	log.Info().
		Str("dsn", dsn).
		Int("max_open_conns", cfg.maxOpenConns).
		Int("max_idle_conns", cfg.maxIdleConns).
		Msg("clickhouse client created")

	return &Client{conn: conn}, nil
}

// Ping verifies the ClickHouse connection is alive.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.conn.Ping(ctx); err != nil {
		return fmt.Errorf("clickhouse ping: %w", err)
	}
	return nil
}

// Close closes the ClickHouse connection.
func (c *Client) Close() error {
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("clickhouse close: %w", err)
	}
	log.Info().Msg("clickhouse client closed")
	return nil
}

// Exec executes a DDL or non-query statement.
func (c *Client) Exec(ctx context.Context, query string, args ...any) error {
	return c.conn.Exec(ctx, query, args...)
}

// Conn returns the underlying driver.Conn for advanced use cases
// such as direct batch operations.
func (c *Client) Conn() driver.Conn {
	return c.conn
}
