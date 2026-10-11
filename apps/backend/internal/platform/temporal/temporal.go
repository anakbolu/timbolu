// Package temporal owns the Temporal client shared by the API and the workers.
// The API starts workflows and sends approval signals; the workers register
// workflows and activities on their own task queues.
package temporal

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

// servingStatus is the health status Temporal reports when it can serve
// requests. Compared by name so the health proto package stays out of this
// package's imports.
const (
	servingStatus      = "SERVING"
	healthCheckTimeout = 5 * time.Second
)

// Client wraps the Temporal client plus the task queues this deployment uses.
type Client struct {
	client               client.Client
	namespace            string
	taskQueueAgent       string
	taskQueueIntegration string
}

// Config holds the Temporal connection settings.
type Config struct {
	HostPort             string
	Namespace            string
	TaskQueueAgent       string
	TaskQueueIntegration string
	// TLS and APIKey are set for Temporal Cloud; self-hosted dev uses neither.
	TLS    bool
	APIKey string
}

// New dials Temporal. The call fails fast when the endpoint is unreachable so a
// misconfigured worker does not start half-alive.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.HostPort == "" {
		return nil, fmt.Errorf("temporal host port is required")
	}

	namespace := cfg.Namespace
	if namespace == "" {
		namespace = "default"
	}

	options := client.Options{
		HostPort:  cfg.HostPort,
		Namespace: namespace,
	}
	if cfg.TLS {
		options.ConnectionOptions = client.ConnectionOptions{
			TLS: &tls.Config{MinVersion: tls.VersionTLS12},
		}
	}
	if cfg.APIKey != "" {
		options.Credentials = client.NewAPIKeyStaticCredentials(cfg.APIKey)
	}

	cli, err := client.DialContext(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("dial temporal: %w", err)
	}

	// A worker that starts "successfully" but cannot reach Temporal is worse
	// than one that refuses to start, so the connection is verified here.
	healthCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	if _, err := cli.CheckHealth(healthCtx, &client.CheckHealthRequest{}); err != nil {
		cli.Close()
		return nil, fmt.Errorf("temporal health check at %s: %w", cfg.HostPort, err)
	}

	return &Client{
		client:               cli,
		namespace:            namespace,
		taskQueueAgent:       taskQueue(cfg.TaskQueueAgent, "bolu-agent"),
		taskQueueIntegration: taskQueue(cfg.TaskQueueIntegration, "bolu-integration"),
	}, nil
}

func taskQueue(configured, fallback string) string {
	if configured == "" {
		return fallback
	}
	return configured
}

// Client exposes the underlying Temporal client.
func (c *Client) Client() client.Client {
	if c == nil {
		return nil
	}
	return c.client
}

// Namespace reports the namespace this deployment talks to.
func (c *Client) Namespace() string {
	if c == nil {
		return ""
	}
	return c.namespace
}

// TaskQueueAgent is the queue the agent worker polls.
func (c *Client) TaskQueueAgent() string {
	if c == nil {
		return ""
	}
	return c.taskQueueAgent
}

// TaskQueueIntegration is the queue the integration worker polls.
func (c *Client) TaskQueueIntegration() string {
	if c == nil {
		return ""
	}
	return c.taskQueueIntegration
}

// StartOptions is what a caller passes to start a workflow on the agent queue.
//
// The workflow id is the caller's, and it is what makes a restart resume the
// work instead of opening a second copy of it: two starts with the same id and
// the same run collide, which is exactly the protection a task needs.
func (c *Client) StartOptions(workflowID string) client.StartWorkflowOptions {
	return client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: c.TaskQueueAgent(),
		// A workflow id that already ran is refused rather than reused, so a
		// duplicate dispatch cannot silently replace a finished task's history.
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}
}

// Close releases the client. Safe on a nil client.
func (c *Client) Close() {
	if c == nil || c.client == nil {
		return
	}
	c.client.Close()
}
