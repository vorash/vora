// Package quota enforces per-sandbox disk limits.
//
// A sandbox's writable state lives in per-sandbox directories on the host
// (/workspace and /root). cgroup v2 has no byte-quota controller, so the limit
// is applied by the filesystem instead, in two layers:
//
//   - Project quotas (hard): when the sandboxes filesystem is mounted with
//     prjquota and the runner can run chattr/setquota, each sandbox's writable
//     directories are assigned a project ID with a byte limit. Writes past the
//     limit fail with EDQUOT at the kernel boundary, so the host is never
//     overrun even if the sandbox ignores the error.
//   - Accounting (soft): the runner periodically sums the writable bytes and
//     stops a sandbox that is over its limit. This runs on every host and
//     supplies the semantic termination reason when hard quotas are enabled.
package quota

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Mode describes how strictly disk limits are enforced on this host.
type Mode string

const (
	// ModeProject means the filesystem enforces the limit with a project quota.
	ModeProject Mode = "project"
	// ModeAccounting means there is no kernel enforcement; only the periodic
	// scan stops an over-quota sandbox.
	ModeAccounting Mode = "accounting"
)

// ErrUnsupported is returned when project quotas cannot be enabled.
var ErrUnsupported = errors.New("quota: project quotas are unavailable")

// Runner runs external commands. It exists so tests can observe and stub the
// chattr/setquota calls.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) error
	LookPath(name string) (string, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

func (execRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

// Manager applies and clears per-sandbox disk limits.
type Manager struct {
	root    string
	run     Runner
	mode    Mode
	mounted string
}

// New detects the strongest enforcement available for the sandboxes root.
func New(root string) *Manager {
	m := &Manager{root: root, run: execRunner{}}
	m.mode = detect(root, m.run, os.Geteuid() == 0)
	return m
}

// Mode reports whether limits are kernel-enforced on this host.
func (m *Manager) Mode() Mode { return m.mode }

// detect returns ModeProject only when the runner is root, both tools exist,
// the filesystem holding root is mounted with prjquota, and a probe shows the
// kernel actually refuses writes past a project-quota limit. The probe matters:
// some kernels account project quotas without enforcing them (observed on
// ext4), and a mount option alone cannot tell the two apart.
func detect(root string, r Runner, rootUser bool) Mode {
	if !rootUser {
		return ModeAccounting
	}
	if _, err := r.LookPath("chattr"); err != nil {
		return ModeAccounting
	}
	if _, err := r.LookPath("setquota"); err != nil {
		return ModeAccounting
	}
	mp := mountPoint(root)
	if mp == "" || !hasProjectQuota(mp) {
		return ModeAccounting
	}
	if !probeHardLimit(root, r) {
		return ModeAccounting
	}
	return ModeProject
}

// probeProjectID is a fixed project id used only for the startup enforcement
// probe, well away from the derived per-sandbox range.
const probeProjectID = uint32(0x7fffffff)

// probeHardLimit writes past a tiny project quota in a throwaway directory and
// reports whether the kernel refused it. It cleans up after itself and is
// safe to run once at startup. Any failure to set up the probe is treated as
// "not enforced" so the runner falls back to the accounting scan.
func probeHardLimit(root string, r Runner) bool {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return false
	}
	dir, err := os.MkdirTemp(root, ".vora-quota-probe-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)

	mp := mountPoint(root)
	if mp == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	proj := strconv.FormatUint(uint64(probeProjectID), 10)
	if err := r.Run(ctx, "chattr", "-R", "-p", proj, dir); err != nil {
		return false
	}
	if err := r.Run(ctx, "chattr", "+P", "-p", proj, dir); err != nil {
		return false
	}
	// One 1 KiB block hard limit; a 64 KiB write must fail when enforced.
	if err := r.Run(ctx, "setquota", "-P", proj, "1", "1", "0", "0", mp); err != nil {
		return false
	}
	defer func() { _ = r.Run(ctx, "setquota", "-P", proj, "0", "0", "0", "0", mp) }()

	f, err := os.Create(filepath.Join(dir, "probe"))
	if err != nil {
		return false
	}
	_, werr := f.Write(make([]byte, 64<<10))
	_ = f.Close()
	return werr != nil
}

// Enforce applies a hard limit of bytes to paths. In accounting mode it is a
// no-op; the caller's periodic scan is responsible for enforcement.
func (m *Manager) Enforce(ctx context.Context, id string, paths []string, bytes int64) error {
	if bytes <= 0 || m.mode != ModeProject {
		return nil
	}
	mp := m.mountPoint()
	if mp == "" {
		return ErrUnsupported
	}
	proj := strconv.FormatUint(uint64(projectID(id)), 10)
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := m.run.Run(ctx, "chattr", "-R", "-p", proj, p); err != nil {
			return fmt.Errorf("quota: assign project %s to %s: %w", proj, p, err)
		}
		// Best effort: mark the directory as a project root so entries created
		// after this point inherit the project id. `+P` sets the project-hierarchy
		// flag; `-P` would clear it and let new files escape the quota (as they do
		// on XFS).
		_ = m.run.Run(ctx, "chattr", "+P", "-p", proj, p)
	}
	blocks := (bytes + 1023) / 1024 // setquota counts 1 KiB blocks
	limit := strconv.FormatInt(blocks, 10)
	if err := m.run.Run(ctx, "setquota", "-P", proj, limit, limit, "0", "0", mp); err != nil {
		return fmt.Errorf("quota: set limit for project %s: %w", proj, err)
	}
	return nil
}

// Release clears the project id and limit for a sandbox. It is best-effort:
// a sandbox whose directories are already gone is not an error.
func (m *Manager) Release(ctx context.Context, id string, paths []string) error {
	if m.mode != ModeProject {
		return nil
	}
	proj := strconv.FormatUint(uint64(projectID(id)), 10)
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		_ = m.run.Run(ctx, "chattr", "-R", "-p", "0", p)
	}
	if mp := m.mountPoint(); mp != "" {
		_ = m.run.Run(ctx, "setquota", "-P", proj, "0", "0", "0", "0", mp)
	}
	return nil
}

// Usage sums the regular-file bytes under paths. It is the accounting-mode
// measure and the source of the semantic reason under project quotas.
func (m *Manager) Usage(paths []string) (int64, error) {
	var total int64
	for _, p := range paths {
		if p == "" {
			continue
		}
		err := filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // a racing change is not fatal for accounting
			}
			if d.Type().IsRegular() {
				if info, err := d.Info(); err == nil {
					total += info.Size()
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return total, err
		}
	}
	return total, nil
}

// mountPoint returns the mount point of the filesystem containing the root.
func (m *Manager) mountPoint() string {
	if m.mounted == "" {
		m.mounted = mountPoint(m.root)
	}
	return m.mounted
}

// projectID derives a stable 31-bit project id from a sandbox id. Ids are not
// persisted: the mapping is a pure function, so a runner restart reapplies the
// same id. A collision between two sandboxes on one filesystem would share a
// quota; the space is large enough that this is acceptable for v0.x.
func projectID(id string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	v := h.Sum32() & 0x7fffffff
	if v == 0 {
		v = 1
	}
	return v
}

// mountPoint finds the longest mount point in /proc/self/mountinfo that is a
// prefix of path.
func mountPoint(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	best := ""
	for _, mp := range mountPoints() {
		if abs == mp || strings.HasPrefix(abs, strings.TrimSuffix(mp, "/")+"/") {
			if len(mp) > len(best) {
				best = mp
			}
		}
	}
	return best
}

// hasProjectQuota reports whether the mount at mp carries the prjquota option.
func hasProjectQuota(mp string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		// mountinfo: id parent major:minor root mountpoint options... - fstype source superoptions
		if len(fields) < 10 || fields[4] != mp {
			continue
		}
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+3 >= len(fields) {
			continue
		}
		for _, opt := range strings.Split(fields[sep+3], ",") {
			if opt == "prjquota" {
				return true
			}
		}
	}
	return false
}

// mountPoints returns every mount point in /proc/self/mountinfo.
func mountPoints() []string {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 {
			out = append(out, fields[4])
		}
	}
	return out
}
