// Package client is the gRPC client used by the vora CLI to talk to the runner.
package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
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
	conn, err := grpc.NewClient("unix://"+socket, dialOptions(insecure.NewCredentials())...)
	if err != nil {
		return nil, fmt.Errorf("client: dial %s: %w", socket, err)
	}
	return &Client{conn: conn, stub: v1.NewRunnerServiceClient(conn)}, nil
}

// dialOptions carries the transport credentials plus the OTel client handler,
// so a CLI-to-runner or API-to-runner RPC continues the caller's trace.
func dialOptions(creds credentials.TransportCredentials) []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	}
}

// DialAddress connects to a runner's TCP gRPC endpoint (used by vora-api).
func DialAddress(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr, dialOptions(insecure.NewCredentials())...)
	if err != nil {
		return nil, fmt.Errorf("client: dial %s: %w", addr, err)
	}
	return &Client{conn: conn, stub: v1.NewRunnerServiceClient(conn)}, nil
}

// DialAddressTLS connects to a runner's TCP gRPC endpoint over TLS.
func DialAddressTLS(addr string, cfg *tls.Config) (*Client, error) {
	conn, err := grpc.NewClient(addr, dialOptions(credentials.NewTLS(cfg))...)
	if err != nil {
		return nil, fmt.Errorf("client: dial %s: %w", addr, err)
	}
	return &Client{conn: conn, stub: v1.NewRunnerServiceClient(conn)}, nil
}

// TunnelDialer opens a connection to a runner the API cannot dial directly.
type TunnelDialer interface {
	Dial(ctx context.Context, runnerID string) (net.Conn, error)
}

// DialTunnel connects to a runner over a reverse tunnel instead of the network,
// so a runner behind NAT can still be driven by the control plane.
func DialTunnel(runnerID string, t TunnelDialer) (*Client, error) {
	opts := append(dialOptions(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return t.Dial(ctx, runnerID)
		}))
	conn, err := grpc.NewClient("passthrough:///vora-tunnel", opts...)
	if err != nil {
		return nil, fmt.Errorf("client: dial tunnel %s: %w", runnerID, err)
	}
	return &Client{conn: conn, stub: v1.NewRunnerServiceClient(conn)}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// CreateSandbox requests a new sandbox.
func (c *Client) CreateSandbox(ctx context.Context, spec *v1.SandboxSpec) (*v1.Sandbox, error) {
	return c.CreateSandboxWithCredentials(ctx, spec, nil)
}

// CreateSandboxWithCredentials requests a new sandbox and carries the resolved
// broker credentials for the aliases its spec grants. The credentials are used
// only to build the sandbox's broker; they are never persisted.
func (c *Client) CreateSandboxWithCredentials(ctx context.Context, spec *v1.SandboxSpec, creds []*v1.BrokerCredential) (*v1.Sandbox, error) {
	resp, err := c.stub.CreateSandbox(ctx, &v1.CreateSandboxRequest{Spec: spec, BrokerSecrets: creds})
	if err != nil {
		return nil, err
	}
	return resp.GetSandbox(), nil
}

// StartSandbox starts a requested sandbox.
// StartSandbox starts a created sandbox.
func (c *Client) StartSandbox(ctx context.Context, id string) (*v1.Sandbox, error) {
	return c.StartSandboxAs(ctx, id, nil)
}

// StartSandboxAs starts a created sandbox, attributing the transition to the
// caller's actor for audit.
func (c *Client) StartSandboxAs(ctx context.Context, id string, actor *v1.Actor) (*v1.Sandbox, error) {
	return c.stub.StartSandbox(ctx, &v1.StartSandboxRequest{SandboxId: id, Actor: actor})
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
// DestroySandbox destroys a sandbox.
func (c *Client) DestroySandbox(ctx context.Context, id string) error {
	return c.DestroySandboxAs(ctx, id, nil)
}

// DestroySandboxAs destroys a sandbox, attributing the transition to the
// caller's actor for audit.
func (c *Client) DestroySandboxAs(ctx context.Context, id string, actor *v1.Actor) error {
	_, err := c.stub.DestroySandbox(ctx, &v1.DestroySandboxRequest{SandboxId: id, Actor: actor})
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

// Egress returns restricted-network policy decisions for a sandbox.
func (c *Client) Egress(ctx context.Context, id string) ([]*v1.SandboxEgress, error) {
	resp, err := c.stub.GetSandboxEgress(ctx, &v1.GetSandboxEgressRequest{SandboxId: id})
	if err != nil {
		return nil, err
	}
	return resp.GetRecords(), nil
}

// GetFile reads a file from a sandbox's writable roots.
func (c *Client) GetFile(ctx context.Context, id, path string) ([]byte, error) {
	resp, err := c.stub.GetSandboxFile(ctx, &v1.GetSandboxFileRequest{SandboxId: id, Path: path})
	if err != nil {
		return nil, err
	}
	return resp.GetContent(), nil
}

// PutFile writes a file into a sandbox's writable roots.
func (c *Client) PutFile(ctx context.Context, id, path string, content []byte, createParents bool) (int64, error) {
	resp, err := c.stub.PutSandboxFile(ctx, &v1.PutSandboxFileRequest{
		SandboxId:     id,
		Path:          path,
		Content:       content,
		CreateParents: createParents,
	})
	if err != nil {
		return 0, err
	}
	return resp.GetBytesWritten(), nil
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

// SnapshotSandbox captures a sandbox's writable state.
func (c *Client) SnapshotSandbox(ctx context.Context, id string) (*v1.Snapshot, error) {
	resp, err := c.stub.SnapshotSandbox(ctx, &v1.SnapshotSandboxRequest{SandboxId: id})
	if err != nil {
		return nil, err
	}
	return resp.GetSnapshot(), nil
}

// GetSnapshot returns a snapshot by id.
func (c *Client) GetSnapshot(ctx context.Context, id string) (*v1.Snapshot, error) {
	return c.stub.GetSnapshot(ctx, &v1.GetSnapshotRequest{SnapshotId: id})
}

// ListSnapshots lists snapshots.
func (c *Client) ListSnapshots(ctx context.Context) ([]*v1.Snapshot, error) {
	resp, err := c.stub.ListSnapshots(ctx, &v1.ListSnapshotsRequest{})
	if err != nil {
		return nil, err
	}
	return resp.GetSnapshots(), nil
}

// DeleteSnapshot removes a snapshot and its archive.
func (c *Client) DeleteSnapshot(ctx context.Context, id string) error {
	_, err := c.stub.DeleteSnapshot(ctx, &v1.DeleteSnapshotRequest{SnapshotId: id})
	return err
}

// ForkSnapshot creates and starts a sandbox from a snapshot.
func (c *Client) ForkSnapshot(ctx context.Context, id string) (*v1.Sandbox, error) {
	return c.ForkSnapshotWithCredentials(ctx, id, nil, nil)
}

// ForkSnapshotWithCredentials forks a snapshot and carries the resolved broker
// credentials for the inherited spec's aliases. The credentials are used only
// to build the fork's broker; they are never persisted.
func (c *Client) ForkSnapshotWithCredentials(ctx context.Context, id string, creds []*v1.BrokerCredential, actor *v1.Actor) (*v1.Sandbox, error) {
	resp, err := c.stub.ForkSnapshot(ctx, &v1.ForkSnapshotRequest{SnapshotId: id, BrokerSecrets: creds, Actor: actor})
	if err != nil {
		return nil, err
	}
	return resp.GetSandbox(), nil
}
