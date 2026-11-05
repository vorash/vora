// Package cgroup manages per-sandbox resource limits through systemd.
//
// v0.1 uses transient systemd services: the runner asks systemd to start the
// sandbox as `vora-<sandbox-id>.service` with MemoryMax/TasksMax/CPUQuota
// properties, which gives a detached process tree with its own cgroup v2
// subtree and no root requirement when running in --user mode.
package cgroup

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runtime"
)

// Errors returned by managers and units.
var (
	// ErrUnsupported means systemd/cgroup v2 is unavailable on this host.
	ErrUnsupported = errors.New("cgroup: systemd user manager unavailable")
	// ErrNotFound means the unit does not exist.
	ErrNotFound = errors.New("cgroup: unit not found")
	// ErrAlreadyExists means a unit with the requested name already exists.
	ErrAlreadyExists = errors.New("cgroup: unit already exists")
)

// UnitPrefix is prepended to sandbox ids to form systemd unit names.
const UnitPrefix = "vora-"

// SandboxUnitPrefix matches runtime sandbox units (vora-sbx_*), distinguishing
// them from the runner's own unit (vora-runner.service).
const SandboxUnitPrefix = UnitPrefix + "sbx_"

// UnitName returns the systemd unit base name for a sandbox id.
func UnitName(sandboxID string) string { return UnitPrefix + sandboxID }

// IsSandboxUnit reports whether a unit base name belongs to a sandbox.
func IsSandboxUnit(name string) bool { return strings.HasPrefix(name, SandboxUnitPrefix) }

// StartRequest describes a sandbox process to launch under a cgroup.
type StartRequest struct {
	// Name is the systemd unit base name (use UnitName).
	Name string
	// Limits are the resource limits to enforce.
	Limits runtime.ResourceLimits
	// Argv is the command to run (argv[0] is the executable).
	Argv []string
	// Env are extra environment variables ("KEY=VALUE").
	Env []string
	// WorkingDirectory, if set, is the unit's working directory on the host.
	WorkingDirectory string
}

// Manager launches and controls cgroup-backed units.
type Manager interface {
	// Start launches a detached transient unit with the given limits.
	Start(ctx context.Context, req StartRequest) (*Unit, error)
	// Attach returns a handle to an existing unit (used by reconciliation).
	Attach(ctx context.Context, name string) (*Unit, error)
	// List returns the base names of vora-managed units.
	List(ctx context.Context) ([]string, error)
	// Mode reports whether user or system systemd is in use.
	Mode() config.SystemdMode
}

// New returns a Manager for the configured systemd mode.
func New(cfg config.Config) (Manager, error) {
	return newSystemd(cfg.SystemdMode)
}

// Unit is a handle to a running (or stopped) systemd unit.
type Unit struct {
	name       string
	cgroupPath string
	ctl        *systemd
}

// Name returns the unit base name.
func (u *Unit) Name() string { return u.name }

// CgroupPath returns the cgroup v2 filesystem path, if known.
func (u *Unit) CgroupPath() string { return u.cgroupPath }

// Active reports whether the unit is currently active.
func (u *Unit) Active(ctx context.Context) (bool, error) { return u.ctl.isActive(ctx, u.name) }

// Stats reads current resource usage from the unit's cgroup.
func (u *Unit) Stats(ctx context.Context) (*runtime.Stats, error) {
	if err := u.resolveCgroup(ctx); err != nil {
		return nil, err
	}
	return readStats(u.cgroupPath)
}

// OpenWindow starts a usage window on the unit's cgroup.
func (u *Unit) OpenWindow(ctx context.Context) (*Window, error) {
	dir, err := u.Dir(ctx)
	if err != nil {
		return nil, err
	}
	return OpenWindow(dir)
}

// Dir is the absolute filesystem path of the unit's cgroup directory.
func (u *Unit) Dir(ctx context.Context) (string, error) {
	if err := u.resolveCgroup(ctx); err != nil {
		return "", err
	}
	return filepath.Join(cgroupRoot, u.cgroupPath), nil
}

// resolveCgroup asks systemd for the unit's cgroup when Start could not.
func (u *Unit) resolveCgroup(ctx context.Context) error {
	if u.cgroupPath != "" {
		return nil
	}
	path, err := u.ctl.controlGroup(ctx, u.name)
	if err != nil {
		return err
	}
	u.cgroupPath = path
	return nil
}

// Stop stops the unit and its process tree.
func (u *Unit) Stop(ctx context.Context) error { return u.ctl.stop(ctx, u.name) }

// Result returns the systemd Result property for the unit, which is retained
// for failed units until Remove is called (e.g. "oom-kill", "timeout").
func (u *Unit) Result(ctx context.Context) (string, error) { return u.ctl.result(ctx, u.name) }

// Remove clears any leftover unit state so the name can be reused.
func (u *Unit) Remove(ctx context.Context) error { return u.ctl.resetFailed(ctx, u.name) }
