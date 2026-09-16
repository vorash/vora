// Package runtime defines the isolation-backend abstraction. Control-plane
// code depends only on this package; concrete backends live in subpackages
// (runtime/bwrap, runtime/gvisor) and are selected via a Registry.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors returned by backends.
var (
	// ErrNotFound means the sandbox handle is unknown to the backend.
	ErrNotFound = errors.New("runtime: sandbox not found")
	// ErrUnsupported means the backend cannot satisfy the request
	// (e.g. a network mode it does not implement).
	ErrUnsupported = errors.New("runtime: unsupported")
	// ErrInvalidSpec means the sandbox specification failed validation.
	ErrInvalidSpec = errors.New("runtime: invalid spec")
)

// NetworkMode is the egress policy applied to a sandbox.
type NetworkMode string

const (
	NetworkNone       NetworkMode = "none"
	NetworkRestricted NetworkMode = "restricted"
	NetworkFull       NetworkMode = "full"
	NetworkPrivate    NetworkMode = "private"
)

// ExitReason is the semantic reason an execution or sandbox terminated.
type ExitReason string

const (
	ReasonExited          ExitReason = "Exited"
	ReasonOOMKilled       ExitReason = "OOMKilled"
	ReasonPidsLimit       ExitReason = "PidsLimitExceeded"
	ReasonTimeout         ExitReason = "TimeoutExceeded"
	ReasonPolicyViolation ExitReason = "PolicyViolation"
	ReasonRuntimeFailure  ExitReason = "RuntimeFailure"
)

// ResourceLimits are enforced through the sandbox cgroup.
type ResourceLimits struct {
	CPUMillis   int64
	MemoryBytes int64
	Pids        int64
	DiskBytes   int64
}

// NetworkPolicy is the egress policy for a sandbox.
type NetworkPolicy struct {
	Mode           NetworkMode
	AllowedDomains []string
}

// WorkspaceSpec describes the per-sandbox paths prepared by the runner. The
// backend binds the read-only rootfs and the writable directories into the
// sandbox and exposes the control socket to the in-sandbox agent.
type WorkspaceSpec struct {
	// RootDir is the per-sandbox state directory.
	RootDir string
	// RootfsDir is the read-only image rootfs to bind as /.
	RootfsDir string
	// WorkspaceDir is bound read-write at /workspace.
	WorkspaceDir string
	// HomeDir is bound read-write at /root.
	HomeDir string
	// IPCDir is bound at /run/vora and carries the agent control socket.
	IPCDir string
	// ControlSocket is the agent socket path on the host
	// (inside IPCDir).
	ControlSocket string
}

// SandboxSpec is the desired state for a sandbox.
type SandboxSpec struct {
	ID        string
	TenantID  string
	AgentID   string
	Image     string
	Resources ResourceLimits
	Network   NetworkPolicy
	Env       map[string]string
	TTL       time.Duration
	Workspace WorkspaceSpec
}

// Validate checks that a spec is structurally usable by any backend. Backends
// may impose additional constraints in Create.
func (s SandboxSpec) Validate() error {
	if s.ID == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidSpec)
	}
	if s.Image == "" {
		return fmt.Errorf("%w: image is required", ErrInvalidSpec)
	}
	if s.Resources.MemoryBytes <= 0 {
		return fmt.Errorf("%w: memory_bytes must be > 0", ErrInvalidSpec)
	}
	if s.Resources.CPUMillis <= 0 {
		return fmt.Errorf("%w: cpu_millis must be > 0", ErrInvalidSpec)
	}
	if s.Resources.Pids <= 0 {
		return fmt.Errorf("%w: pids must be > 0", ErrInvalidSpec)
	}
	switch s.Network.Mode {
	case NetworkNone:
	case "":
		return fmt.Errorf("%w: network mode is required", ErrInvalidSpec)
	default:
		// Non-none modes are validated per backend; the spec itself is valid.
	}
	return nil
}

// Name returns a human-readable identifier for a spec.
func (s SandboxSpec) Name() string { return s.ID }

// ExecRequest is a command to run inside an existing sandbox.
type ExecRequest struct {
	ExecutionID string
	Command     string
	Args        []string
	Cwd         string
	Env         map[string]string
	Timeout     time.Duration
}

// ExecResult reports how an execution finished.
type ExecResult struct {
	ExitCode int
	Reason   ExitReason
}

// ExecChunk is one streamed unit of exec output. Exactly one field is set.
type ExecChunk struct {
	Stdout []byte
	Stderr []byte
	Exit   *ExecResult
}

// ExecStream is a stream of exec output. Recv returns io.EOF after the final
// chunk (which carries Exit). Close releases the stream.
type ExecStream interface {
	Recv() (ExecChunk, error)
	Close() error
}

// Stats are the observed resource usage of a sandbox.
type Stats struct {
	MemoryBytes   int64
	MemoryPeak    int64
	CPUMillis     int64
	Pids          int64
	OOMKills      int64
	PidsLimitHits int64
}

// Capabilities describes what a backend can do, so the runner can adapt
// without special-casing backend names.
type Capabilities struct {
	// NativeExec is true when the backend can exec into a running sandbox
	// itself (gVisor). When false, the runner uses the in-sandbox agent.
	NativeExec bool
	// OwnsCgroups is true when the backend manages resource limits itself
	// (containerd-backed). When false the runner applies limits via
	// cgroup.Manager during Start.
	OwnsCgroups bool
	// NetworkModes lists the modes the backend implements.
	NetworkModes []NetworkMode
}

// SupportsNetwork reports whether the backend implements a network mode.
func (c Capabilities) SupportsNetwork(m NetworkMode) bool {
	for _, n := range c.NetworkModes {
		if n == m {
			return true
		}
	}
	return false
}

// Handle identifies a created sandbox within a backend. Backends return their
// own concrete handle type.
type Handle interface {
	ID() string
}

// Backend is implemented by every isolation technology.
type Backend interface {
	// Name is the backend identifier (e.g. "bwrap", "gvisor").
	Name() string
	// Capabilities reports backend abilities.
	Capabilities() Capabilities

	// Create prepares a sandbox without starting it.
	Create(ctx context.Context, spec SandboxSpec) (Handle, error)
	// Start starts the sandbox workload.
	Start(ctx context.Context, h Handle) error
	// Exec runs a command in a running sandbox.
	Exec(ctx context.Context, h Handle, req ExecRequest) (ExecStream, error)
	// Signal sends a signal to the sandbox's init process.
	Signal(ctx context.Context, h Handle, sig int32) error
	// Stats returns resource usage for the sandbox.
	Stats(ctx context.Context, h Handle) (*Stats, error)
	// Stop gracefully stops the sandbox, then kills it.
	Stop(ctx context.Context, h Handle) error
	// Destroy releases all resources for the sandbox.
	Destroy(ctx context.Context, h Handle) error
}
