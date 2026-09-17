// Package containerd provides a minimal client for the Vora image store.
//
// containerd is used only as an OCI content + snapshot store (see ADR 009); it
// is never the container runtime. Images are pulled and unpacked through it and
// the resulting snapshot rootfs is bound read-only into a sandbox.
package containerd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	ctr "github.com/containerd/containerd/v2/client"
)

const (
	// DefaultNamespace is the containerd namespace that holds Vora images.
	DefaultNamespace = "vora"
	// DefaultSnapshotter is the snapshotter Vora unpacks images into. The
	// native snapshotter yields a plain user-readable directory, which both
	// backends can bind without an extra mount.
	DefaultSnapshotter = "native"
)

// SocketPath resolves the containerd socket: the explicit address, then
// VORA_CONTAINERD_ADDRESS, then the rootless socket under $XDG_RUNTIME_DIR,
// then the system socket.
func SocketPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("VORA_CONTAINERD_ADDRESS"); v != "" {
		return v
	}
	if rd := os.Getenv("XDG_RUNTIME_DIR"); rd != "" {
		p := filepath.Join(rd, "containerd", "containerd.sock")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "/run/containerd/containerd.sock"
}

// Available reports whether a containerd socket exists.
func Available(explicit string) bool {
	p := SocketPath(explicit)
	_, err := os.Stat(p)
	return err == nil
}

// Connect dials containerd. namespace defaults to DefaultNamespace.
func Connect(address, namespace string) (*ctr.Client, error) {
	if address == "" {
		address = SocketPath("")
	}
	if namespace == "" {
		namespace = DefaultNamespace
	}
	cli, err := ctr.New(address,
		ctr.WithDefaultNamespace(namespace),
		ctr.WithTimeout(5*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("containerd: connect %s: %w", address, err)
	}
	return cli, nil
}

// Close releases the client connection.
func Close(cli *ctr.Client) error {
	if cli == nil {
		return nil
	}
	return cli.Close()
}
