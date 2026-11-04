// Package config defines the runner configuration and its defaults.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SystemdMode selects which systemd instance owns sandbox cgroups.
type SystemdMode string

const (
	// SystemdUser uses `systemd-run --user --scope` (no root required).
	SystemdUser SystemdMode = "user"
	// SystemdSystem uses `systemd-run --system --scope` (root required).
	SystemdSystem SystemdMode = "system"
)

// Config is the runner configuration. Paths default to system-wide locations
// when running as root and to user directories otherwise, so the runner works
// rootlessly during local development.
type Config struct {
	// StateDir holds sandbox metadata, images and logs.
	StateDir string
	// SocketPath is the runner control socket.
	SocketPath string
	// SystemdMode selects user or system scopes.
	SystemdMode SystemdMode
	// DefaultTTL is applied when a sandbox spec does not set one.
	DefaultTTL            time.Duration
	MaxMemoryBytes        int64
	MaxCPUMillis          int64
	MaxPids               int64
	MaxSandboxes          int32
	MaxSandboxesPerTenant int32
	MaxSnapshotsPerTenant int32
	MaxTTL                time.Duration
	// AuditRetention is how long a finished sandbox's local audit trail is
	// kept. Zero keeps it forever.
	AuditRetention time.Duration
	// Backends limits which isolation backends the runner registers.
	Backends         BackendSet
	DefaultDiskBytes int64
	MaxDiskBytes     int64
	// MaxSnapshotBytes caps one on-disk snapshot archive.
	MaxSnapshotBytes int64
	// MetricsAddr is the listen address for the Prometheus /metrics endpoint.
	MetricsAddr string
	// GvisorPlatform selects the gVisor platform ("kvm" or "systrap").
	GvisorPlatform string
	// GvisorRunRoot is the runsc --root state directory (default under
	// $XDG_RUNTIME_DIR).
	GvisorRunRoot string
	// RunscPath overrides the runsc binary location.
	RunscPath string
	// ContainerdAddress is the containerd gRPC socket used as the OCI image
	// store. Empty selects the rootless socket under $XDG_RUNTIME_DIR, then the
	// system socket.
	ContainerdAddress string
	// ContainerdNamespace is the containerd namespace for Vora images.
	ContainerdNamespace string
	// ContainerdSnapshotter is the snapshotter used to unpack images.
	ContainerdSnapshotter string
	// ImagesMode selects how an image ref resolves to a rootfs:
	// "auto" (containerd, then the local cache), "containerd" or "cache".
	ImagesMode string
	// ImagePolicy selects image signature verification: "off" (default;
	// images are trusted) or "enforce" (unsigned/unverifiable images are
	// refused). See docs/adr/020-image-signing.md.
	ImagePolicy string
	// CosignKey is the path to a cosign public key used for verification when
	// ImagePolicy is "enforce". Required in enforce mode.
	CosignKey string
	// CosignPath overrides the cosign binary location.
	CosignPath string
	// EgressMaxConnections bounds concurrent proxy connections per sandbox.
	EgressMaxConnections int
	// EgressIdleTimeout closes connections with no traffic in either direction.
	EgressIdleTimeout time.Duration
	// EgressBrokerPort is the gateway port the credential broker listens on.
	EgressBrokerPort string
}

// Image mode values for Config.ImagesMode.
const (
	ImagesAuto       = "auto"
	ImagesContainerd = "containerd"
	ImagesCache      = "cache"
)

// Image signature policy values for Config.ImagePolicy.
const (
	// ImagePolicyOff trusts every image. It is the default and is logged
	// loudly at startup.
	ImagePolicyOff = "off"
	// ImagePolicyEnforce refuses images whose signature cannot be verified.
	ImagePolicyEnforce = "enforce"
)

// ImagesDir is the read-only rootfs cache.
func (c Config) ImagesDir() string { return filepath.Join(c.StateDir, "images") }

// SandboxesDir is the per-sandbox state root.
func (c Config) SandboxesDir() string { return filepath.Join(c.StateDir, "sandboxes") }

// SnapshotsDir is the snapshot store root.
func (c Config) SnapshotsDir() string { return filepath.Join(c.StateDir, "snapshots") }

// Default returns the configuration for the current user, with environment
// overrides:
//
//	VORA_STATE_DIR   overrides the state directory
//	VORA_SOCKET      overrides the control socket path
//	VORA_SYSTEMD_MODE  "user" (default) or "system"
func Default() (Config, error) {
	c := Config{
		SystemdMode:           SystemdUser,
		DefaultTTL:            30 * time.Minute,
		MaxSnapshotBytes:      1 << 30,
		MaxMemoryBytes:        8 << 30,
		MaxCPUMillis:          8000,
		MaxPids:               512,
		MaxSandboxes:          64,
		MaxSandboxesPerTenant: 16,
		MaxSnapshotsPerTenant: 32,
		MaxTTL:                24 * time.Hour,
		DefaultDiskBytes:      1 << 30,
		MaxDiskBytes:          8 << 30,
		MetricsAddr:           "127.0.0.1:9101",
		GvisorPlatform:        "kvm",
		ContainerdNamespace:   "vora",
		ContainerdSnapshotter: "native",
		ImagesMode:            ImagesAuto,
		EgressMaxConnections:  128,
		EgressIdleTimeout:     5 * time.Minute,
		EgressBrokerPort:      "8080",
	}

	if os.Geteuid() == 0 {
		c.StateDir = "/var/lib/vora"
		c.SocketPath = "/var/run/vora/runner.sock"
		c.SystemdMode = SystemdSystem
	} else {
		c.StateDir = filepath.Join(userStateDir(), "vora")
		c.SocketPath = filepath.Join(userRuntimeDir(), "vora", "runner.sock")
	}

	if v := os.Getenv("VORA_STATE_DIR"); v != "" {
		c.StateDir = v
	}
	if v := os.Getenv("VORA_SOCKET"); v != "" {
		c.SocketPath = v
	}
	if v := os.Getenv("VORA_SYSTEMD_MODE"); v != "" {
		switch SystemdMode(v) {
		case SystemdUser, SystemdSystem:
			c.SystemdMode = SystemdMode(v)
		default:
			return Config{}, fmt.Errorf("invalid VORA_SYSTEMD_MODE %q (want %q or %q)",
				v, SystemdUser, SystemdSystem)
		}
	}

	for _, field := range []struct {
		name   string
		target *int64
	}{
		{"VORA_MAX_MEMORY", &c.MaxMemoryBytes},
		{"VORA_MAX_CPU", &c.MaxCPUMillis},
		{"VORA_MAX_PIDS", &c.MaxPids},
		{"VORA_DEFAULT_DISK", &c.DefaultDiskBytes},
		{"VORA_MAX_DISK", &c.MaxDiskBytes},
	} {
		if v := os.Getenv(field.name); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				return Config{}, fmt.Errorf("invalid %s %q (want positive integer)", field.name, v)
			}
			*field.target = n
		}
	}
	for _, field := range []struct {
		name   string
		target *int32
	}{
		{"VORA_MAX_SANDBOXES", &c.MaxSandboxes},
		{"VORA_MAX_SANDBOXES_PER_TENANT", &c.MaxSandboxesPerTenant},
		{"VORA_MAX_SNAPSHOTS_PER_TENANT", &c.MaxSnapshotsPerTenant},
	} {
		if v := os.Getenv(field.name); v != "" {
			n, err := strconv.ParseInt(v, 10, 32)
			if err != nil || n <= 0 {
				return Config{}, fmt.Errorf("invalid %s %q (want positive integer)", field.name, v)
			}
			*field.target = int32(n)
		}
	}
	if v := os.Getenv("VORA_MAX_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("invalid VORA_MAX_TTL %q (want positive duration)", v)
		}
		c.MaxTTL = d
	}
	if v := os.Getenv("VORA_MAX_SNAPSHOT_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("invalid VORA_MAX_SNAPSHOT_BYTES %q (want positive byte count)", v)
		}
		c.MaxSnapshotBytes = n
	}
	if v := os.Getenv("VORA_METRICS_ADDR"); v != "" {
		c.MetricsAddr = v
	}
	if v := os.Getenv("VORA_GVISOR_PLATFORM"); v != "" {
		c.GvisorPlatform = v
	}
	if v := os.Getenv("VORA_GVISOR_ROOT"); v != "" {
		c.GvisorRunRoot = v
	}
	if v := os.Getenv("VORA_RUNSC"); v != "" {
		c.RunscPath = v
	}
	if v := os.Getenv("VORA_CONTAINERD_ADDRESS"); v != "" {
		c.ContainerdAddress = v
	}
	if v := os.Getenv("VORA_CONTAINERD_NAMESPACE"); v != "" {
		c.ContainerdNamespace = v
	}
	if v := os.Getenv("VORA_CONTAINERD_SNAPSHOTTER"); v != "" {
		c.ContainerdSnapshotter = v
	}
	if v := os.Getenv("VORA_IMAGES_MODE"); v != "" {
		switch v {
		case ImagesAuto, ImagesContainerd, ImagesCache:
			c.ImagesMode = v
		default:
			return Config{}, fmt.Errorf("invalid VORA_IMAGES_MODE %q (want %q, %q or %q)",
				v, ImagesAuto, ImagesContainerd, ImagesCache)
		}
	}
	if v := os.Getenv("VORA_IMAGE_POLICY"); v != "" {
		switch v {
		case ImagePolicyOff, ImagePolicyEnforce:
			c.ImagePolicy = v
		default:
			return Config{}, fmt.Errorf("invalid VORA_IMAGE_POLICY %q (want %q or %q)",
				v, ImagePolicyOff, ImagePolicyEnforce)
		}
	}
	if v := os.Getenv("VORA_COSIGN_KEY"); v != "" {
		c.CosignKey = v
	}
	if v := os.Getenv("VORA_COSIGN_PATH"); v != "" {
		c.CosignPath = v
	}
	if v := os.Getenv("VORA_EGRESS_MAX_CONNECTIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("invalid VORA_EGRESS_MAX_CONNECTIONS %q (want a positive integer)", v)
		}
		c.EgressMaxConnections = n
	}
	if v := os.Getenv("VORA_EGRESS_IDLE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("invalid VORA_EGRESS_IDLE_TIMEOUT %q (want a positive duration)", v)
		}
		c.EgressIdleTimeout = d
	}
	if v := os.Getenv("VORA_EGRESS_BROKER_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil || port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("invalid VORA_EGRESS_BROKER_PORT %q (want a port number)", v)
		}
		c.EgressBrokerPort = v
	}

	if c.DefaultTTL <= 0 {
		c.DefaultTTL = 30 * time.Minute
	}
	return c, nil
}

// AuditRetentionFromEnv parses VORA_AUDIT_RETENTION. It is separate from
// Default because only the runner uses it: the CLI also calls Default, and a
// runner-only typo must not break every CLI command.
func AuditRetentionFromEnv() (time.Duration, error) {
	v := os.Getenv("VORA_AUDIT_RETENTION")
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid VORA_AUDIT_RETENTION %q (want positive duration)", v)
	}
	return d, nil
}

// Isolation backend names accepted by VORA_BACKENDS. They match the backends'
// runtime.Backend Name().
const (
	BackendBwrap  = "bwrap"
	BackendGvisor = "gvisor"
)

// BackendSet is the VORA_BACKENDS allowlist. The zero value allows every
// backend, and a backend that fails to start is skipped. A parsed list allows
// only its members, and each of them must start.
type BackendSet struct {
	names map[string]bool
}

// Explicit reports whether the operator listed backends.
func (s BackendSet) Explicit() bool { return s.names != nil }

// Allows reports whether the runner may register the named backend.
func (s BackendSet) Allows(name string) bool { return s.names == nil || s.names[name] }

// Names returns the listed backends in sorted order, or nil for the zero value.
func (s BackendSet) Names() []string {
	if s.names == nil {
		return nil
	}
	out := make([]string, 0, len(s.names))
	for name := range s.names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ParseBackends parses a comma-separated VORA_BACKENDS value. An empty value
// is the zero set. Empty entries, duplicates and unknown names are errors:
// each is more likely a typo than a deliberate choice.
func ParseBackends(v string) (BackendSet, error) {
	if strings.TrimSpace(v) == "" {
		return BackendSet{}, nil
	}
	names := map[string]bool{}
	for _, raw := range strings.Split(v, ",") {
		name := strings.TrimSpace(raw)
		switch {
		case name == "":
			return BackendSet{}, fmt.Errorf("invalid VORA_BACKENDS %q: empty entry", v)
		case name != BackendBwrap && name != BackendGvisor:
			return BackendSet{}, fmt.Errorf("invalid VORA_BACKENDS %q: unknown backend %q (want %q or %q)",
				v, name, BackendBwrap, BackendGvisor)
		case names[name]:
			return BackendSet{}, fmt.Errorf("invalid VORA_BACKENDS %q: %q listed twice", v, name)
		}
		names[name] = true
	}
	return BackendSet{names: names}, nil
}

// BackendsFromEnv parses VORA_BACKENDS. Like AuditRetentionFromEnv it is
// separate from Default, so a runner-only typo cannot break the CLI.
func BackendsFromEnv() (BackendSet, error) {
	return ParseBackends(os.Getenv("VORA_BACKENDS"))
}

func userStateDir() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return os.TempDir()
	}
	return filepath.Join(home, ".local", "state")
}

func userRuntimeDir() string {
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return v
	}
	return os.TempDir()
}
