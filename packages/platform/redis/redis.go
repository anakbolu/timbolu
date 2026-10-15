// Package redis owns the shared Redis client used for realtime pub/sub
// channels, per-integration rate limit buckets and the live quota counter.
package redis

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Client wraps the Redis client.
type Client struct {
	client *redis.Client
}

// New connects to Redis and verifies the connection with a ping.
func New(ctx context.Context, url string) (*Client, error) {
	if url == "" {
		return nil, fmt.Errorf("redis url is required")
	}

	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}

	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	return &Client{client: client}, nil
}

// Raw exposes the underlying client for repositories and services.
func (c *Client) Raw() *redis.Client {
	if c == nil {
		return nil
	}
	return c.client
}

// Ping reports whether Redis is reachable; used by the readiness probe.
func (c *Client) Ping(ctx context.Context) error {
	if c == nil || c.client == nil {
		return fmt.Errorf("redis client is not configured")
	}
	return c.client.Ping(ctx).Err()
}

// Close releases the client. Safe on a nil client.
func (c *Client) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	return c.client.Close()
}
