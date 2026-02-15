package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/rs/zerolog/log"
)

// Client wraps a ClickHouse connection.
type Client struct {
	conn driver.Conn
	dsn  string
}

// NewClient creates a new ClickHouse client from a DSN.
// DSN format: clickhouse://user:password@host:port/database
func NewClient(dsn string) (*Client, error) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse clickhouse DSN: %w", err)
	}

	opts.MaxOpenConns = 10
	opts.MaxIdleConns = 5
	opts.ConnMaxLifetime = 10 * time.Minute
	opts.DialTimeout = 5 * time.Second

	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}

	log.Info().Str("dsn", dsn).Msg("ClickHouse client created")

	return &Client{conn: conn, dsn: dsn}, nil
}

// Ping verifies the connection to ClickHouse.
func (c *Client) Ping(ctx context.Context) error {
	return c.conn.Ping(ctx)
}

// Conn returns the underlying clickhouse driver connection.
func (c *Client) Conn() driver.Conn {
	return c.conn
}

// Close closes the ClickHouse connection.
func (c *Client) Close() error {
	return c.conn.Close()
}
