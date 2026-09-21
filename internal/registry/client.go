// Package registry implements the runner-side client for vora-api's
// RunnerRegistry: a runner dials out, registers, and then heartbeats.
package registry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	v1 "github.com/vorash/vora/api/proto/v1"
)

// Client is a gRPC client for the RunnerRegistry service.
type Client struct {
	conn  *grpc.ClientConn
	stub  v1.RunnerRegistryClient
	token string
}

// Dial connects to vora-api's gRPC endpoint. joinToken, when set, is sent as a
// bearer token on every call. Dial options set the transport credentials;
// without any, the connection is plaintext.
func Dial(addr, joinToken string, opts ...grpc.DialOption) (*Client, error) {
	if len(opts) == 0 {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("registry: dial %s: %w", addr, err)
	}
	return &Client{conn: conn, stub: v1.NewRunnerRegistryClient(conn), token: joinToken}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) ctx(ctx context.Context) context.Context {
	if c.token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.token)
}

// Register registers this runner with the API.
func (c *Client) Register(ctx context.Context, req *v1.RegisterRunnerRequest) (*v1.RegisterRunnerResponse, error) {
	return c.stub.Register(c.ctx(ctx), req)
}

// Heartbeat sends a liveness/capacity update.
func (c *Client) Heartbeat(ctx context.Context, req *v1.HeartbeatRequest) (*v1.HeartbeatResponse, error) {
	return c.stub.Heartbeat(c.ctx(ctx), req)
}

// Reporter keeps a runner registered and sends heartbeats.
type Reporter struct {
	Client       *Client
	RunnerID     string
	Address      string
	Version      string
	Backends     []string
	NetworkModes []string
	CPUMillis    int64
	MemoryBytes  int64
	MaxSandboxes int32
	Running      func() int32
	Logger       *slog.Logger
	Interval     time.Duration
}

// Run registers (retrying until it succeeds or ctx is cancelled) and then
// heartbeats until ctx is cancelled.
func (r *Reporter) Run(ctx context.Context) {
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := r.Interval
	if interval <= 0 {
		interval = 10 * time.Second
	}

	for {
		resp, err := r.Client.Register(ctx, &v1.RegisterRunnerRequest{
			RunnerId:     r.RunnerID,
			Address:      r.Address,
			Backends:     r.Backends,
			NetworkModes: r.NetworkModes,
			Version:      r.Version,
			Capacity: &v1.RunnerCapacity{
				CpuMillis:    r.CPUMillis,
				MemoryBytes:  r.MemoryBytes,
				MaxSandboxes: r.MaxSandboxes,
			},
		})
		if err == nil {
			if s := resp.GetHeartbeatIntervalSeconds(); s > 0 {
				interval = time.Duration(s) * time.Second
			}
			logger.Info("registered with vora-api", "runner_id", r.RunnerID, "address", r.Address)
			break
		}
		logger.Warn("registration failed; retrying", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			running := int32(0)
			if r.Running != nil {
				running = r.Running()
			}
			if _, err := r.Client.Heartbeat(ctx, &v1.HeartbeatRequest{
				RunnerId:         r.RunnerID,
				SandboxesRunning: running,
				Capacity: &v1.RunnerCapacity{
					CpuMillis:    r.CPUMillis,
					MemoryBytes:  r.MemoryBytes,
					MaxSandboxes: r.MaxSandboxes,
				},
			}); err != nil {
				logger.Warn("heartbeat failed", "error", err)
			}
		}
	}
}
