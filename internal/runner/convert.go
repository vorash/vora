package runner

import (
	"fmt"
	"strings"
	"time"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/egress"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/snapshot"
)

// specFromProto converts a proto spec into a runtime spec, applying defaults.
func specFromProto(p *v1.SandboxSpec, defaultTTL time.Duration) (runtime.SandboxSpec, error) {
	if p == nil {
		return runtime.SandboxSpec{}, fmt.Errorf("spec is required")
	}
	spec := runtime.SandboxSpec{
		ID:       p.GetSandboxId(),
		TenantID: p.GetTenantId(),
		AgentID:  p.GetAgentId(),
		Image:    p.GetImage(),
		Backend:  p.GetBackend(),
		Network: runtime.NetworkPolicy{
			Mode:           networkModeFromProto(p.GetNetwork().GetMode()),
			AllowedDomains: p.GetNetwork().GetAllowedDomains(),
			BrokerAliases:  p.GetNetwork().GetBrokerAliases(),
		},
	}
	if r := p.GetResources(); r != nil {
		spec.Resources = runtime.ResourceLimits{
			CPUMillis:   r.GetCpuMillis(),
			MemoryBytes: r.GetMemoryBytes(),
			Pids:        r.GetPids(),
			DiskBytes:   r.GetDiskBytes(),
		}
	}
	if len(p.GetEnv()) > 0 {
		spec.Env = make(map[string]string, len(p.GetEnv()))
		for _, kv := range p.GetEnv() {
			// values are "KEY=VALUE"
			for i := 0; i < len(kv); i++ {
				if kv[i] == '=' {
					spec.Env[kv[:i]] = kv[i+1:]
					break
				}
			}
		}
	}
	if p.GetTtlSeconds() > int64((1<<63-1)/int64(time.Second)) {
		return runtime.SandboxSpec{}, fmt.Errorf("ttl_seconds is too large")
	}
	spec.TTL = time.Duration(p.GetTtlSeconds()) * time.Second
	if spec.TTL <= 0 {
		spec.TTL = defaultTTL
	}
	return spec, nil
}

// brokerCredentialsFromProto converts resolved broker credentials into the
// transient form handed to a backend. Every credential is validated, so a
// malformed one fails the create instead of silently starting unbrokered.
func brokerCredentialsFromProto(in []*v1.BrokerCredential) ([]egress.Credential, error) {
	out := make([]egress.Credential, 0, len(in))
	for _, c := range in {
		cred := egress.Credential{
			Alias:  c.GetAlias(),
			Host:   c.GetHost(),
			Scheme: c.GetScheme(),
			Header: c.GetHeader(),
			Value:  c.GetValue(),
		}
		if err := egress.ValidateCredential(cred); err != nil {
			return nil, err
		}
		out = append(out, cred)
	}
	return out, nil
}

func networkModeFromProto(m v1.NetworkMode) runtime.NetworkMode {
	switch m {
	case v1.NetworkMode_NETWORK_NONE:
		return runtime.NetworkNone
	case v1.NetworkMode_NETWORK_RESTRICTED:
		return runtime.NetworkRestricted
	case v1.NetworkMode_NETWORK_FULL:
		return runtime.NetworkFull
	case v1.NetworkMode_NETWORK_PRIVATE:
		return runtime.NetworkPrivate
	default:
		return ""
	}
}

func stateToProto(s sandbox.State) v1.SandboxState {
	switch s {
	case sandbox.StateRequested:
		return v1.SandboxState_REQUESTED
	case sandbox.StateScheduling:
		return v1.SandboxState_SCHEDULING
	case sandbox.StateCreating:
		return v1.SandboxState_CREATING
	case sandbox.StateStarting:
		return v1.SandboxState_STARTING
	case sandbox.StateRunning:
		return v1.SandboxState_RUNNING
	case sandbox.StateStopping:
		return v1.SandboxState_STOPPING
	case sandbox.StateStopped:
		return v1.SandboxState_STOPPED
	case sandbox.StateFailed:
		return v1.SandboxState_FAILED
	case sandbox.StateExpired:
		return v1.SandboxState_EXPIRED
	case sandbox.StateDestroyed:
		return v1.SandboxState_DESTROYED
	default:
		return v1.SandboxState_SANDBOX_STATE_UNSPECIFIED
	}
}

func stateFromProto(s v1.SandboxState) sandbox.State {
	switch s {
	case v1.SandboxState_REQUESTED:
		return sandbox.StateRequested
	case v1.SandboxState_SCHEDULING:
		return sandbox.StateScheduling
	case v1.SandboxState_CREATING:
		return sandbox.StateCreating
	case v1.SandboxState_STARTING:
		return sandbox.StateStarting
	case v1.SandboxState_RUNNING:
		return sandbox.StateRunning
	case v1.SandboxState_STOPPING:
		return sandbox.StateStopping
	case v1.SandboxState_STOPPED:
		return sandbox.StateStopped
	case v1.SandboxState_FAILED:
		return sandbox.StateFailed
	case v1.SandboxState_EXPIRED:
		return sandbox.StateExpired
	case v1.SandboxState_DESTROYED:
		return sandbox.StateDestroyed
	default:
		return ""
	}
}

func reasonToProto(r runtime.ExitReason) v1.ExitReason {
	switch r {
	case runtime.ReasonExited:
		return v1.ExitReason_EXITED
	case runtime.ReasonOOMKilled:
		return v1.ExitReason_OOM_KILLED
	case runtime.ReasonPidsLimit:
		return v1.ExitReason_PIDS_LIMIT_EXCEEDED
	case runtime.ReasonTimeout:
		return v1.ExitReason_TIMEOUT_EXCEEDED
	case runtime.ReasonPolicyViolation:
		return v1.ExitReason_POLICY_VIOLATION
	case runtime.ReasonDiskQuota:
		return v1.ExitReason_DISK_QUOTA_EXCEEDED
	case runtime.ReasonRuntimeFailure:
		return v1.ExitReason_RUNTIME_FAILURE
	default:
		return v1.ExitReason_EXIT_REASON_UNSPECIFIED
	}
}

func resourceLimitsToProto(r runtime.ResourceLimits) *v1.ResourceLimits {
	return &v1.ResourceLimits{
		CpuMillis:   r.CPUMillis,
		MemoryBytes: r.MemoryBytes,
		Pids:        r.Pids,
		DiskBytes:   r.DiskBytes,
	}
}

func networkPolicyToProto(n runtime.NetworkPolicy) *v1.NetworkPolicy {
	return &v1.NetworkPolicy{
		Mode:           networkModeToProto(n.Mode),
		AllowedDomains: n.AllowedDomains,
	}
}

func networkModeToProto(m runtime.NetworkMode) v1.NetworkMode {
	switch m {
	case runtime.NetworkNone:
		return v1.NetworkMode_NETWORK_NONE
	case runtime.NetworkRestricted:
		return v1.NetworkMode_NETWORK_RESTRICTED
	case runtime.NetworkFull:
		return v1.NetworkMode_NETWORK_FULL
	case runtime.NetworkPrivate:
		return v1.NetworkMode_NETWORK_PRIVATE
	default:
		return v1.NetworkMode_NETWORK_MODE_UNSPECIFIED
	}
}

// toProtoSandbox converts a persisted record into its proto representation.
// execRequestFromProto converts a proto exec request to a runtime one.
func execRequestFromProto(p *v1.ExecRequest) runtime.ExecRequest {
	return runtime.ExecRequest{
		ExecutionID: p.GetExecutionId(),
		Command:     p.GetCommand(),
		Args:        p.GetArgs(),
		Cwd:         p.GetCwd(),
		Env:         envMap(p.GetEnv()),
		Timeout:     time.Duration(p.GetTimeoutSeconds()) * time.Second,
		Stdin:       p.GetStdin(),
	}
}

// execChunkToProto converts a runtime exec chunk to its proto form.
func execChunkToProto(c runtime.ExecChunk) *v1.ExecChunk {
	switch {
	case c.Exit != nil:
		return &v1.ExecChunk{Payload: &v1.ExecChunk_Exit{Exit: &v1.ExecExit{
			ExitCode: int32(c.Exit.ExitCode),
			Reason:   reasonToProto(c.Exit.Reason),
		}}}
	case c.Stderr != nil:
		return &v1.ExecChunk{Payload: &v1.ExecChunk_Stderr{Stderr: c.Stderr}}
	default:
		return &v1.ExecChunk{Payload: &v1.ExecChunk_Stdout{Stdout: c.Stdout}}
	}
}

func eventsToProto(events []sandbox.Event) []*v1.SandboxEvent {
	out := make([]*v1.SandboxEvent, 0, len(events))
	for _, e := range events {
		out = append(out, &v1.SandboxEvent{
			SandboxId:       e.SandboxID,
			From:            string(e.From),
			To:              string(e.To),
			Reason:          e.Reason,
			RunnerId:        e.RunnerID,
			TimestampUnixMs: e.Timestamp.UnixMilli(),
		})
	}
	return out
}

func execsToProto(execs []sandbox.ExecRecord) []*v1.SandboxExec {
	out := make([]*v1.SandboxExec, 0, len(execs))
	for _, e := range execs {
		out = append(out, &v1.SandboxExec{
			ExecutionId:      e.ExecutionID,
			Command:          e.Command,
			Args:             e.Args,
			Cwd:              e.Cwd,
			StartedAtUnixMs:  e.StartedAt.UnixMilli(),
			FinishedAtUnixMs: e.FinishedAt.UnixMilli(),
			ExitCode:         int32(e.ExitCode),
			Reason:           reasonToProto(e.Reason),
			StdoutBytes:      e.StdoutBytes,
			StderrBytes:      e.StderrBytes,
			CpuMillis:        e.CPUMillis,
			MemoryPeakBytes:  e.MemoryPeakBytes,
			EgressAllowed:    e.EgressAllowed,
			EgressDenied:     e.EgressDenied,
		})
	}
	return out
}

func egressToProto(records []sandbox.EgressRecord) []*v1.SandboxEgress {
	out := make([]*v1.SandboxEgress, 0, len(records))
	for _, r := range records {
		out = append(out, &v1.SandboxEgress{
			SandboxId:       r.SandboxID,
			Host:            r.Host,
			Remote:          r.Remote,
			Alias:           r.Alias,
			Allowed:         r.Allowed,
			Reason:          r.Reason,
			TimestampUnixMs: r.Timestamp.UnixMilli(),
		})
	}
	return out
}

func envMap(env []string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

func toProtoSandbox(rec *sandbox.Record) *v1.Sandbox {
	return &v1.Sandbox{
		Id:              rec.ID,
		State:           stateToProto(rec.State),
		Spec:            specToProto(rec.Spec),
		RunnerId:        rec.RunnerID,
		Reason:          reasonToProto(rec.Reason),
		CreatedAtUnixMs: rec.CreatedAt.UnixMilli(),
		UpdatedAtUnixMs: rec.UpdatedAt.UnixMilli(),
	}
}

func specToProto(spec runtime.SandboxSpec) *v1.SandboxSpec {
	return &v1.SandboxSpec{
		SandboxId:  spec.ID,
		TenantId:   spec.TenantID,
		AgentId:    spec.AgentID,
		Image:      spec.Image,
		Backend:    spec.Backend,
		Resources:  resourceLimitsToProto(spec.Resources),
		Network:    networkPolicyToProto(spec.Network),
		TtlSeconds: int64(spec.TTL / time.Second),
	}
}

func toProtoSnapshot(rec *snapshot.Record) *v1.Snapshot {
	return &v1.Snapshot{
		Id:              rec.ID,
		SandboxId:       rec.SandboxID,
		TenantId:        rec.TenantID,
		RunnerId:        rec.RunnerID,
		Spec:            specToProto(rec.Spec),
		Bytes:           rec.Bytes,
		CreatedAtUnixMs: rec.CreatedAt.UnixMilli(),
	}
}
