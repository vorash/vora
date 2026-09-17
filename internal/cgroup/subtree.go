package cgroup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/vorash/vora/internal/runtime"
)

const (
	runnerLeaf   = "vora-init"
	cpuPeriod    = 100000
	subtreeCtrls = "+memory +pids +cpu"
)

// Subtree is a writable cgroup v2 directory under the runner’s delegated parent.
type Subtree interface {
	Create(ctx context.Context, id string, lim runtime.ResourceLimits) (*Group, error)
	Attach(ctx context.Context, id string) (*Group, error)
	List(ctx context.Context) ([]string, error)
}

// Group is one sandbox cgroup.
type Group struct {
	id   string
	rel  string // path relative to the cgroup mount
	root string
}

// Path is the absolute filesystem path of the cgroup directory.
func (g *Group) Path() string { return absCgroup(g.root, g.rel) }

// fsSubtree implements Subtree against a cgroup v2 mount.
type fsSubtree struct {
	root   string
	parent string // relative to root, no leading slash
}

// NewSubtree prepares a delegated parent under the current process cgroup.
func NewSubtree() (Subtree, error) {
	parent, err := currentCgroup()
	if err != nil {
		return nil, err
	}
	s := &fsSubtree{root: cgroupRoot, parent: parent}
	if err := s.prepare(); err != nil {
		return nil, err
	}
	return s, nil
}

// OpenSubtree returns a Subtree rooted at root with parent relative path.
// Used by tests; it does not perform the process-move dance.
func OpenSubtree(root, parent string) Subtree {
	return &fsSubtree{root: root, parent: strings.TrimPrefix(parent, "/")}
}

func currentCgroup() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("cgroup: read /proc/self/cgroup: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			p := strings.TrimPrefix(line, "0::")
			p = strings.TrimPrefix(p, "/")
			if strings.HasSuffix(p, "/"+runnerLeaf) {
				p = strings.TrimSuffix(p, "/"+runnerLeaf)
			}
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: no cgroup v2 path in /proc/self/cgroup", ErrUnsupported)
}

func absCgroup(root, rel string) string {
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return root
	}
	return filepath.Join(root, rel)
}

// prepare makes the parent an inner node: processes live in vora-init, and
// memory/pids/cpu are enabled on subtree_control so sibling sandbox cgroups
// can apply limits.
func (s *fsSubtree) prepare() error {
	parent := absCgroup(s.root, s.parent)
	if err := enableControllers(parent); err == nil {
		return nil
	}
	leaf := filepath.Join(parent, runnerLeaf)
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		return fmt.Errorf("cgroup: mkdir %s: %w", leaf, err)
	}
	if err := moveProcs(parent, leaf); err != nil {
		return err
	}
	if err := enableControllers(parent); err != nil {
		return fmt.Errorf("cgroup: enable controllers on %s: %w", parent, err)
	}
	return nil
}

func enableControllers(dir string) error {
	return os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(subtreeCtrls+"\n"), 0o644)
}

func moveProcs(from, to string) error {
	data, err := os.ReadFile(filepath.Join(from, "cgroup.procs"))
	if err != nil {
		return fmt.Errorf("cgroup: read procs: %w", err)
	}
	dst := filepath.Join(to, "cgroup.procs")
	for _, line := range strings.Split(string(data), "\n") {
		pid := strings.TrimSpace(line)
		if pid == "" {
			continue
		}
		if err := os.WriteFile(dst, []byte(pid+"\n"), 0o644); err != nil {
			// Threads may refuse until every thread is moved; keep going.
			_ = os.WriteFile(filepath.Join(to, "cgroup.threads"), []byte(pid+"\n"), 0o644)
		}
	}
	return nil
}

func (s *fsSubtree) groupRel(id string) string {
	name := UnitName(id)
	if s.parent == "" {
		return name
	}
	return s.parent + "/" + name
}

func (s *fsSubtree) Create(_ context.Context, id string, lim runtime.ResourceLimits) (*Group, error) {
	if id == "" {
		return nil, fmt.Errorf("cgroup: empty sandbox id")
	}
	g := &Group{id: id, rel: s.groupRel(id), root: s.root}
	dir := g.Path()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cgroup: mkdir %s: %w", dir, err)
	}
	if err := writeLimits(dir, lim); err != nil {
		_ = os.Remove(dir)
		return nil, err
	}
	return g, nil
}

func (s *fsSubtree) Attach(_ context.Context, id string) (*Group, error) {
	g := &Group{id: id, rel: s.groupRel(id), root: s.root}
	if _, err := os.Stat(g.Path()); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil, err
	}
	return g, nil
}

func (s *fsSubtree) List(_ context.Context) ([]string, error) {
	parent := absCgroup(s.root, s.parent)
	entries, err := os.ReadDir(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !IsSandboxUnit(name) {
			continue
		}
		ids = append(ids, strings.TrimPrefix(name, UnitPrefix))
	}
	return ids, nil
}

func writeLimits(dir string, lim runtime.ResourceLimits) error {
	if err := writeCgroupFile(dir, "memory.max", strconv.FormatInt(lim.MemoryBytes, 10)); err != nil {
		return fmt.Errorf("cgroup: memory.max: %w", err)
	}
	_ = writeCgroupFile(dir, "memory.swap.max", "0")
	if err := writeCgroupFile(dir, "pids.max", strconv.FormatInt(lim.Pids, 10)); err != nil {
		return fmt.Errorf("cgroup: pids.max: %w", err)
	}
	if err := writeCgroupFile(dir, "cpu.max", cpuMax(lim.CPUMillis)); err != nil {
		return fmt.Errorf("cgroup: cpu.max: %w", err)
	}
	return nil
}

func writeCgroupFile(dir, name, value string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644)
}

func cpuMax(millis int64) string {
	if millis <= 0 {
		return "max 100000"
	}
	return strconv.FormatInt(millis*100, 10) + " " + strconv.Itoa(cpuPeriod)
}

// Adopt writes pid into this cgroup.
func (g *Group) Adopt(pid int) error {
	if err := os.WriteFile(filepath.Join(g.Path(), "cgroup.procs"), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return fmt.Errorf("cgroup: adopt %d: %w", pid, err)
	}
	return nil
}

// ApplyTo starts cmd in this cgroup via CLONE_INTO_CGROUP.
func (g *Group) ApplyTo(cmd *exec.Cmd) (func(), error) {
	f, err := os.Open(g.Path())
	if err != nil {
		return nil, fmt.Errorf("cgroup: open %s: %w", g.Path(), err)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(f.Fd())
	return func() { _ = f.Close() }, nil
}

// Stats reads resource usage from this cgroup.
func (g *Group) Stats() (*runtime.Stats, error) {
	old := cgroupRoot
	cgroupRoot = g.root
	defer func() { cgroupRoot = old }()
	return readStats(g.rel)
}

// Destroy moves leftover processes to the parent and removes the directory.
func (g *Group) Destroy() error {
	dir := g.Path()
	parent := filepath.Dir(dir)
	_ = moveProcs(dir, parent)
	err := os.Remove(dir)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	// Real cgroup dirs rmdir even with controller files. A fake directory
	// used in tests has regular files, so fall back to RemoveAll.
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("cgroup: rmdir %s: %w", dir, err)
	}
	return nil
}
