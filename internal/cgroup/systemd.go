package cgroup

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/vorash/vora/internal/config"
)

// systemd implements Manager using systemd-run / systemctl.
type systemd struct {
	mode config.SystemdMode
}

func newSystemd(mode config.SystemdMode) (*systemd, error) {
	s := &systemd{mode: mode}
	if err := s.available(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *systemd) Mode() config.SystemdMode { return s.mode }

// sysctlArgs returns the common flags for systemctl.
func (s *systemd) sysctlArgs(args ...string) []string {
	if s.mode == config.SystemdUser {
		return append([]string{"--user"}, args...)
	}
	return args
}

func (s *systemd) available() error {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return fmt.Errorf("%w: systemd-run not found", ErrUnsupported)
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("%w: systemctl not found", ErrUnsupported)
	}
	out, err := exec.Command("systemctl", s.sysctlArgs("show-environment")...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", ErrUnsupported, strings.TrimSpace(string(out)))
	}
	return nil
}

// Start launches a detached transient service with the requested limits.
func (s *systemd) Start(ctx context.Context, req StartRequest) (*Unit, error) {
	if len(req.Argv) == 0 {
		return nil, fmt.Errorf("cgroup: empty argv")
	}
	args := []string{}
	if s.mode == config.SystemdUser {
		args = append(args, "--user")
	}
	args = append(args,
		"--unit="+req.Name,
		"--property=MemoryMax="+strconv.FormatInt(req.Limits.MemoryBytes, 10),
		"--property=MemorySwapMax=0",
		"--property=TasksMax="+strconv.FormatInt(req.Limits.Pids, 10),
		"--property=CPUQuota="+cpuQuota(req.Limits.CPUMillis),
		"--property=KillMode=control-group",
	)
	if req.WorkingDirectory != "" {
		args = append(args, "--property=WorkingDirectory="+req.WorkingDirectory)
	}
	for _, e := range req.Env {
		args = append(args, "--setenv="+e)
	}
	args = append(args, "--")
	args = append(args, req.Argv...)

	out, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "already loaded") || strings.Contains(msg, "already exists") {
			return nil, fmt.Errorf("%w: %s", ErrAlreadyExists, req.Name)
		}
		return nil, fmt.Errorf("cgroup: systemd-run: %w: %s", err, msg)
	}

	unit := &Unit{name: req.Name, ctl: s}
	// The cgroup path can lag slightly behind systemd-run returning.
	deadline := time.Now().Add(2 * time.Second)
	for {
		path, err := s.controlGroup(ctx, req.Name)
		if err == nil && path != "" {
			unit.cgroupPath = path
			return unit, nil
		}
		if time.Now().After(deadline) {
			return unit, nil
		}
		select {
		case <-ctx.Done():
			return unit, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// Attach resolves an existing unit by name.
func (s *systemd) Attach(ctx context.Context, name string) (*Unit, error) {
	path, err := s.controlGroup(ctx, name)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return &Unit{name: name, cgroupPath: path, ctl: s}, nil
}

func (s *systemd) controlGroup(ctx context.Context, name string) (string, error) {
	out, err := exec.CommandContext(ctx, "systemctl",
		s.sysctlArgs("show", name, "--property=ControlGroup", "--value")...).Output()
	if err != nil {
		return "", fmt.Errorf("cgroup: systemctl show %s: %w", name, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (s *systemd) isActive(ctx context.Context, name string) (bool, error) {
	out, err := exec.CommandContext(ctx, "systemctl", s.sysctlArgs("is-active", name)...).Output()
	state := strings.TrimSpace(string(out))
	// is-active exits non-zero for inactive/failed; the textual state is
	// still meaningful, so do not treat a non-zero exit as an error.
	_ = err
	return state == "active" || state == "activating", nil
}

// result returns the systemd Result property (e.g. "success", "oom-kill",
// "timeout", "exit-code"). It is available for failed units, which systemd
// keeps loaded until reset-failed.
func (s *systemd) result(ctx context.Context, name string) (string, error) {
	out, err := exec.CommandContext(ctx, "systemctl",
		s.sysctlArgs("show", name, "--property=Result", "--value")...).Output()
	if err != nil {
		return "", fmt.Errorf("cgroup: systemctl show %s: %w", name, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (s *systemd) stop(ctx context.Context, name string) error {
	out, err := exec.CommandContext(ctx, "systemctl", s.sysctlArgs("stop", name)...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "not loaded") || strings.Contains(msg, "not found") {
			return nil
		}
		return fmt.Errorf("cgroup: stop %s: %w: %s", name, err, msg)
	}
	return nil
}

func (s *systemd) resetFailed(ctx context.Context, name string) error {
	// Best effort: failed/absent units are fine.
	_ = exec.CommandContext(ctx, "systemctl", s.sysctlArgs("reset-failed", name)...).Run()
	return nil
}

// List returns vora-managed unit base names, including failed ones.
func (s *systemd) List(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "systemctl",
		s.sysctlArgs("list-units", "--type=service", "--all", "--no-legend", "--plain")...).Output()
	if err != nil {
		return nil, fmt.Errorf("cgroup: list-units: %w", err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := strings.TrimSuffix(fields[0], ".service")
		if strings.HasPrefix(name, UnitPrefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

// cpuQuota converts CPU millis (1000 = one CPU) to a systemd CPUQuota value.
func cpuQuota(millis int64) string {
	return strconv.FormatFloat(float64(millis)/10, 'f', -1, 64) + "%"
}
