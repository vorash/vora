// Package client is the gRPC client used by the vora CLI to talk to the runner.
package client

import (
	"context"
	"crypto/tls"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	v1 "github.com/vorash/vora/api/proto/v1"
)

// Client wraps the generated RunnerService client.
type Client struct {
	conn *grpc.ClientConn
	stub v1.RunnerServiceClient
}

// Dial connects to the runner control socket.
func Dial(socket string) (*Client, error) {
	conn, err := grpc.NewClient("unix://"+socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("client: dial %s: %w", socket, err)
	}
	return &Client{conn: conn, stub: v1.NewRunnerServiceClient(conn)}, nil
}

// DialAddress connects to a runner's TCP gRPC endpoint (used by vora-api).
func DialAddress(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("client: dial %s: %w", addr, err)
	}
	return &Client{conn: conn, stub: v1.NewRunnerServiceClient(conn)}, nil
}

// DialAddressTLS connects to a runner's TCP gRPC endpoint over TLS.
func DialAddressTLS(addr string, cfg *tls.Config) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		return nil, fmt.Errorf("client: dial %s: %w", addr, err)
	}
	return &Client{conn: conn, stub: v1.NewRunnerServiceClient(conn)}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// CreateSandbox requests a new sandbox.
func (c *Client) CreateSandbox(ctx context.Context, spec *v1.SandboxSpec) (*v1.Sandbox, error) {
	resp, err := c.stub.CreateSandbox(ctx, &v1.CreateSandboxRequest{Spec: spec})
	if err != nil {
		return nil, err
	}
	return resp.GetSandbox(), nil
}

// StartSandbox starts a requested sandbox.
func (c *Client) StartSandbox(ctx context.Context, id string) (*v1.Sandbox, error) {
	return c.stub.StartSandbox(ctx, &v1.StartSandboxRequest{SandboxId: id})
}

// GetSandbox returns a sandbox by id.
func (c *Client) GetSandbox(ctx context.Context, id string) (*v1.Sandbox, error) {
	return c.stub.GetSandbox(ctx, &v1.GetSandboxRequest{SandboxId: id})
}

// ListSandboxes lists sandboxes.
func (c *Client) ListSandboxes(ctx context.Context, includeDestroyed bool) ([]*v1.Sandbox, error) {
	resp, err := c.stub.ListSandboxes(ctx, &v1.ListSandboxesRequest{IncludeDestroyed: includeDestroyed})
	if err != nil {
		return nil, err
	}
	return resp.GetSandboxes(), nil
}

// DestroySandbox destroys a sandbox.
func (c *Client) DestroySandbox(ctx context.Context, id string) error {
	_, err := c.stub.DestroySandbox(ctx, &v1.DestroySandboxRequest{SandboxId: id})
	return err
}

// StatsSandbox returns resource usage.
func (c *Client) StatsSandbox(ctx context.Context, id string) (*v1.Stats, error) {
	return c.stub.StatsSandbox(ctx, &v1.StatsSandboxRequest{SandboxId: id})
}

// Events returns the lifecycle event history.
func (c *Client) Events(ctx context.Context, id string) ([]*v1.SandboxEvent, error) {
	resp, err := c.stub.GetSandboxEvents(ctx, &v1.GetSandboxEventsRequest{SandboxId: id})
	if err != nil {
		return nil, err
	}
	return resp.GetEvents(), nil
}

// Execs returns the execution audit records for a sandbox.
func (c *Client) Execs(ctx context.Context, id string) ([]*v1.SandboxExec, error) {
	resp, err := c.stub.GetSandboxExecs(ctx, &v1.GetSandboxExecsRequest{SandboxId: id})
	if err != nil {
		return nil, err
	}
	return resp.GetExecs(), nil
}

// Exec starts a command and returns the output stream.
func (c *Client) Exec(ctx context.Context, req *v1.ExecSandboxRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[v1.ExecChunk], error) {
	return c.stub.ExecSandbox(ctx, req, opts...)
}

// SignalSandbox sends a signal to a sandbox.
func (c *Client) SignalSandbox(ctx context.Context, id string, sig int32) error {
	_, err := c.stub.SignalSandbox(ctx, &v1.SignalSandboxRequest{SandboxId: id, Signal: sig})
	return err
}
