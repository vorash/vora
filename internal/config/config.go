// Package config defines the runner configuration and its defaults.
package config

import (
	"fmt"
	"os"
	"path/filepath"
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
	DefaultTTL time.Duration
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
}

// Image mode values for Config.ImagesMode.
const (
	ImagesAuto       = "auto"
	ImagesContainerd = "containerd"
	ImagesCache      = "cache"
)

// ImagesDir is the read-only rootfs cache.
func (c Config) ImagesDir() string { return filepath.Join(c.StateDir, "images") }

// SandboxesDir is the per-sandbox state root.
func (c Config) SandboxesDir() string { return filepath.Join(c.StateDir, "sandboxes") }

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
		MetricsAddr:           "127.0.0.1:9101",
		GvisorPlatform:        "kvm",
		ContainerdNamespace:   "vora",
		ContainerdSnapshotter: "native",
		ImagesMode:            ImagesAuto,
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

	if c.DefaultTTL <= 0 {
		c.DefaultTTL = 30 * time.Minute
	}
	return c, nil
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
