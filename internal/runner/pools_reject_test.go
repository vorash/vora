package runner

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"

	v1 "github.com/vorash/vora/api/proto/v1"
)

// pooledPolicy is what one RunnerService RPC does with a sandbox that
// belongs to a pool.
type pooledPolicy int

const (
	// rejects: the RPC refuses a pool member with FailedPrecondition. Anything
	// that could change a member, or read a run's files, between runs.
	rejects pooledPolicy = iota + 1
	// allows: a read of the member's record or audit trail, harmless to the
	// next run's proof.
	allows
	// noSandbox: the RPC does not address a sandbox by id.
	noSandbox
)

// pooledSandboxRPCs classifies every RPC. TestPooledSandboxRPCs walks the
// service descriptor, so a new RPC fails until it has a row here, and a row
// that claims "rejects" is called against a real member to prove it.
var pooledSandboxRPCs = map[string]struct {
	policy pooledPolicy
	call   func(context.Context, v1.RunnerServiceClient, string) error
}{
	"CreateSandbox":  {noSandbox, nil},
	"ListSandboxes":  {noSandbox, nil},
	"GetSnapshot":    {noSandbox, nil},
	"ListSnapshots":  {noSandbox, nil},
	"DeleteSnapshot": {noSandbox, nil},
	"ForkSnapshot":   {noSandbox, nil}, // addresses a snapshot, and a member cannot be snapshotted
	"CreatePool":     {noSandbox, nil},
	"GetPool":        {noSandbox, nil},
	"DeletePool":     {noSandbox, nil},
	"GetPoolEvents":  {noSandbox, nil},
	"RunInPool":      {noSandbox, nil},

	"GetSandbox": {allows, func(ctx context.Context, c v1.RunnerServiceClient, id string) error {
		_, err := c.GetSandbox(ctx, &v1.GetSandboxRequest{SandboxId: id})
		return err
	}},
	"StatsSandbox": {allows, func(ctx context.Context, c v1.RunnerServiceClient, id string) error {
		_, err := c.StatsSandbox(ctx, &v1.StatsSandboxRequest{SandboxId: id})
		return err
	}},
	"GetSandboxEvents": {allows, func(ctx context.Context, c v1.RunnerServiceClient, id string) error {
		_, err := c.GetSandboxEvents(ctx, &v1.GetSandboxEventsRequest{SandboxId: id})
		return err
	}},
	"GetSandboxExecs": {allows, func(ctx context.Context, c v1.RunnerServiceClient, id string) error {
		_, err := c.GetSandboxExecs(ctx, &v1.GetSandboxExecsRequest{SandboxId: id})
		return err
	}},
	"GetSandboxEgress": {allows, func(ctx context.Context, c v1.RunnerServiceClient, id string) error {
		_, err := c.GetSandboxEgress(ctx, &v1.GetSandboxEgressRequest{SandboxId: id})
		return err
	}},

	"StartSandbox": {rejects, func(ctx context.Context, c v1.RunnerServiceClient, id string) error {
		_, err := c.StartSandbox(ctx, &v1.StartSandboxRequest{SandboxId: id})
		return err
	}},
	"ExecSandbox": {rejects, func(ctx context.Context, c v1.RunnerServiceClient, id string) error {
		stream, err := c.ExecSandbox(ctx, &v1.ExecSandboxRequest{SandboxId: id, Exec: &v1.ExecRequest{Command: "true"}})
		if err != nil {
			return err
		}
		_, err = stream.Recv()
		return err
	}},
	"SignalSandbox": {rejects, func(ctx context.Context, c v1.RunnerServiceClient, id string) error {
		_, err := c.SignalSandbox(ctx, &v1.SignalSandboxRequest{SandboxId: id, Signal: 9})
		return err
	}},
	"GetSandboxFile": {rejects, func(ctx context.Context, c v1.RunnerServiceClient, id string) error {
		_, err := c.GetSandboxFile(ctx, &v1.GetSandboxFileRequest{SandboxId: id, Path: "/workspace/x"})
		return err
	}},
	"PutSandboxFile": {rejects, func(ctx context.Context, c v1.RunnerServiceClient, id string) error {
		_, err := c.PutSandboxFile(ctx, &v1.PutSandboxFileRequest{SandboxId: id, Path: "/workspace/x", Content: []byte("x")})
		return err
	}},
	"DestroySandbox": {rejects, func(ctx context.Context, c v1.RunnerServiceClient, id string) error {
		_, err := c.DestroySandbox(ctx, &v1.DestroySandboxRequest{SandboxId: id})
		return err
	}},
	"SnapshotSandbox": {rejects, func(ctx context.Context, c v1.RunnerServiceClient, id string) error {
		_, err := c.SnapshotSandbox(ctx, &v1.SnapshotSandboxRequest{SandboxId: id})
		return err
	}},
}

// TestPooledSandboxRPCs is the default-deny gate on pool members: every RPC
// on the service descriptor must say what it does with one, an RPC whose
// request carries a sandbox_id cannot claim it addresses none, and each
// "rejects" row is proven against a live member.
func TestPooledSandboxRPCs(t *testing.T) {
	methods := v1.File_api_proto_v1_runner_proto.Services().ByName("RunnerService").Methods()
	for i := 0; i < methods.Len(); i++ {
		m := methods.Get(i)
		name := string(m.Name())
		row, ok := pooledSandboxRPCs[name]
		if !ok {
			t.Errorf("%s is not classified: say what it does with a pool member", name)
			continue
		}
		if row.policy == noSandbox && m.Input().Fields().ByName(protoreflect.Name("sandbox_id")) != nil {
			t.Errorf("%s takes a sandbox_id but is classified as addressing no sandbox", name)
		}
		if row.policy != noSandbox && row.call == nil {
			t.Errorf("%s is classified but has no call to prove it", name)
		}
	}
	if t.Failed() {
		return
	}

	h := newWarmHarness(t)
	p := h.create(poolSpec(1, 1))
	h.waitIdle(p.GetId(), 1)
	recs, _ := h.s.store.List()
	member := recs[0].ID
	for name, row := range pooledSandboxRPCs {
		if row.policy == noSandbox {
			continue
		}
		err := row.call(h.ctx(), h.client, member)
		code := status.Code(err)
		switch row.policy {
		case rejects:
			if code != codes.FailedPrecondition {
				t.Errorf("%s on a pool member: %v, want FailedPrecondition", name, err)
			}
		case allows:
			if err != nil {
				t.Errorf("%s on a pool member: %v, want allowed", name, err)
			}
		}
	}
	// The member is untouched: still the pool's, still idle.
	if st := h.get(p.GetId()).GetStats(); st.GetIdle() != 1 || st.GetDestroyedTotal() != 0 {
		t.Fatalf("pool after the rejected RPCs = %v, want its member intact", st)
	}
}
