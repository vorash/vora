package api

import (
	"context"
	"io"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	v1 "github.com/vorash/vora/api/proto/v1"
)

// fakePools is the part of fakeRunner that answers pool calls the way the runner
// documents them: CreatePool idempotent by (tenant, name), and a pool of another
// tenant NotFound.
type fakePools struct {
	mu       sync.Mutex
	pools    map[string]*v1.Pool // by id
	creates  []*v1.CreatePoolRequest
	gets     []*v1.GetPoolRequest
	deletes  []*v1.DeletePoolRequest
	runs     []*v1.RunInPoolRequest
	execs    []*v1.GetSandboxExecsRequest
	members  map[string]*v1.Sandbox
	events   []*v1.SandboxEvent
	calls    int
	getBoxes int

	poolCreateErr error
	// createdThenErr makes CreatePool create the pool and lose the answer.
	createdThenErr error
	getErr         error
	deleteErr      error
	deleteBusy     bool
	runErr         error
	runChunks      []*v1.RunChunk
}

func (f *fakePools) CreatePool(_ context.Context, req *v1.CreatePoolRequest) (*v1.Pool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.creates = append(f.creates, proto.Clone(req).(*v1.CreatePoolRequest))
	if f.poolCreateErr != nil {
		return nil, f.poolCreateErr
	}
	spec := proto.Clone(req.GetSpec()).(*v1.PoolSpec)
	if spec.Size == nil {
		size := uint32(8)
		spec.Size = &size
	}
	if spec.MaxSize == nil {
		maxSize := max(spec.GetSize(), 1)
		spec.MaxSize = &maxSize
	}
	for _, p := range f.pools {
		if p.GetSpec().GetTenantId() == spec.GetTenantId() && p.GetSpec().GetName() == spec.GetName() {
			if p.GetState() == v1.PoolState_POOL_NOT_LOADED {
				return nil, status.Error(codes.FailedPrecondition, "pool "+p.GetId()+" is not loaded on this runner: backend_missing")
			}
			if !proto.Equal(p.GetSpec(), spec) {
				return nil, status.Error(codes.AlreadyExists, "pool exists with a different spec")
			}
			return proto.Clone(p).(*v1.Pool), nil
		}
	}
	if f.pools == nil {
		f.pools = map[string]*v1.Pool{}
	}
	p := &v1.Pool{Id: req.GetPoolId(), Spec: spec, State: v1.PoolState_POOL_ACTIVE, RunnerId: "run_test", Stats: &v1.PoolStats{}}
	f.pools[p.GetId()] = p
	if f.createdThenErr != nil {
		return nil, f.createdThenErr
	}
	return proto.Clone(p).(*v1.Pool), nil
}

func (f *fakePools) GetPool(_ context.Context, poolID, tenantID string) (*v1.Pool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gets = append(f.gets, &v1.GetPoolRequest{PoolId: poolID, TenantId: tenantID})
	if f.getErr != nil {
		return nil, f.getErr
	}
	p, ok := f.pools[poolID]
	if !ok || p.GetSpec().GetTenantId() != tenantID {
		return nil, status.Error(codes.NotFound, "pool "+poolID+" not found")
	}
	return proto.Clone(p).(*v1.Pool), nil
}

func (f *fakePools) DeletePool(_ context.Context, req *v1.DeletePoolRequest) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.deletes = append(f.deletes, proto.Clone(req).(*v1.DeletePoolRequest))
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	p, ok := f.pools[req.GetPoolId()]
	if !ok || p.GetSpec().GetTenantId() != req.GetTenantId() {
		return false, status.Error(codes.NotFound, "pool not found")
	}
	if f.deleteBusy {
		p.State = v1.PoolState_POOL_DRAINING
		return false, nil
	}
	delete(f.pools, req.GetPoolId())
	return true, nil
}

func (f *fakePools) RunInPool(_ context.Context, req *v1.RunInPoolRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[v1.RunChunk], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.runs = append(f.runs, proto.Clone(req).(*v1.RunInPoolRequest))
	// As over gRPC, what the runner refuses arrives on the first Recv.
	if err := f.runErr; err != nil {
		return &fakeRunStream{err: err}, nil
	}
	p, ok := f.pools[req.GetPoolId()]
	switch {
	case !ok || p.GetSpec().GetTenantId() != req.GetTenantId():
		return &fakeRunStream{err: status.Error(codes.NotFound, "pool not found")}, nil
	case p.GetState() == v1.PoolState_POOL_NOT_LOADED:
		return &fakeRunStream{err: status.Error(codes.FailedPrecondition, "pool is not loaded on this runner: backend_missing")}, nil
	case p.GetState() == v1.PoolState_POOL_DRAINING:
		return &fakeRunStream{err: status.Error(codes.FailedPrecondition, "pool: draining")}, nil
	}
	return &fakeRunStream{chunks: f.runChunks}, nil
}

func (f *fakePools) PoolExecs(_ context.Context, sandboxID, poolID string) ([]*v1.SandboxExec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.execs = append(f.execs, &v1.GetSandboxExecsRequest{SandboxId: sandboxID, PoolId: poolID})
	sb, ok := f.members[sandboxID]
	if !ok || sb.GetPoolId() != poolID {
		return nil, status.Error(codes.NotFound, "sandbox not found in pool")
	}
	return []*v1.SandboxExec{{ExecutionId: "exe_member", PoolId: poolID, SandboxRun: 1}}, nil
}

// member reports a sandbox of a pool, as GetSandbox would.
func (f *fakePools) member(id string) (*v1.Sandbox, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getBoxes++
	sb, ok := f.members[id]
	if !ok {
		return nil, false
	}
	return proto.Clone(sb).(*v1.Sandbox), true
}

func (f *fakePools) pool(id string) *v1.Pool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pools[id]
}

func (f *fakePools) poolCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pools)
}

func (f *fakePools) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeRunStream delivers its error, if any, on the first Recv.
type fakeRunStream struct {
	grpc.ClientStream
	chunks []*v1.RunChunk
	err    error
	i      int
}

func (s *fakeRunStream) Recv() (*v1.RunChunk, error) {
	if s.i < len(s.chunks) {
		s.i++
		return s.chunks[s.i-1], nil
	}
	if s.err != nil {
		return nil, s.err
	}
	return nil, io.EOF
}
