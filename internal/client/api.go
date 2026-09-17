package client

import (
	"context"

	"google.golang.org/grpc"

	v1 "github.com/vorash/vora/api/proto/v1"
)

// API is the sandbox control surface shared by the local gRPC client (over the
// runner Unix socket) and the remote HTTP client (over vora-api).
type API interface {
	CreateSandbox(ctx context.Context, spec *v1.SandboxSpec) (*v1.Sandbox, error)
	StartSandbox(ctx context.Context, id string) (*v1.Sandbox, error)
	GetSandbox(ctx context.Context, id string) (*v1.Sandbox, error)
	ListSandboxes(ctx context.Context, includeDestroyed bool) ([]*v1.Sandbox, error)
	DestroySandbox(ctx context.Context, id string) error
	StatsSandbox(ctx context.Context, id string) (*v1.Stats, error)
	Events(ctx context.Context, id string) ([]*v1.SandboxEvent, error)
	Execs(ctx context.Context, id string) ([]*v1.SandboxExec, error)
	Exec(ctx context.Context, req *v1.ExecSandboxRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[v1.ExecChunk], error)
	Close() error
}

var (
	_ API = (*Client)(nil)
	_ API = (*HTTPClient)(nil)
)