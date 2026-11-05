package bwrap

import (
	"reflect"
	"strings"
	"testing"
)

func TestArgsRequiresInputs(t *testing.T) {
	cases := map[string]BuildOptions{
		"no rootfs":    {WorkspaceDir: "/w", IPCDir: "/i"},
		"no workspace": {RootfsDir: "/r", IPCDir: "/i"},
		"no ipc":       {RootfsDir: "/r", WorkspaceDir: "/w"},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Args(opts); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestArgsBuildsIsolationFlags(t *testing.T) {
	argv, err := Args(BuildOptions{
		RootfsDir:    "/cache/base/rootfs",
		WorkspaceDir: "/state/sbx/workspace",
		HomeDir:      "/state/sbx/home",
		IPCDir:       "/state/sbx/ipc",
		Env:          map[string]string{"B": "2", "A": "1"},
	})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	if argv[0] != "bwrap" {
		t.Fatalf("argv[0] = %q", argv[0])
	}
	joined := strings.Join(argv, " ")

	// Every isolation guarantee the security tests rely on.
	for _, want := range []string{
		"--unshare-all",
		"--die-with-parent",
		"--new-session",
		"--ro-bind /cache/base/rootfs /",
		"--bind /state/sbx/workspace /workspace",
		"--bind /state/sbx/home /root",
		"--bind /state/sbx/ipc /vora",
		"--ro-bind /state/sbx/ipc/vora-init /vora/vora-init",
		"--tmpfs /tmp",
		"--proc /proc",
		"--dev /dev",
		"--chdir /workspace",
		"--setenv HOME /root",
		"-- /vora/vora-init --socket /vora/control.sock",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q\nargv: %s", want, joined)
		}
	}

	// Environment is emitted sorted for deterministic argv.
	aIdx := indexOf(argv, "--setenv", "A")
	bIdx := indexOf(argv, "--setenv", "B")
	if aIdx < 0 || bIdx < 0 || aIdx > bIdx {
		t.Errorf("env not sorted: A@%d B@%d", aIdx, bIdx)
	}

	// The rootfs must be bound read-only and before writable mounts.
	roIdx := strings.Index(joined, "/cache/base/rootfs")
	wsIdx := strings.Index(joined, "/state/sbx/workspace")
	if roIdx < 0 || wsIdx < 0 || roIdx > wsIdx {
		t.Errorf("rootfs bind must precede workspace bind")
	}

	// bwrap applies mounts in argv order, and the file bind's target only exists
	// once /vora is bound. The other way round, every sandbox fails to start.
	dirIdx := indexOf(argv, "--bind", "/state/sbx/ipc")
	fileIdx := indexOf(argv, "--ro-bind", "/state/sbx/ipc/vora-init")
	if dirIdx < 0 || fileIdx < dirIdx {
		t.Errorf("the vora-init file bind (@%d) must follow the /vora bind (@%d)", fileIdx, dirIdx)
	}
}

func indexOf(argv []string, flag, value string) int {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return i
		}
	}
	return -1
}

// TestArgsBindsResolver asserts a restricted sandbox gets its resolver from the
// gateway, and that no resolv.conf bind appears when none is configured.
func TestArgsBindsResolver(t *testing.T) {
	base := BuildOptions{
		RootfsDir:    "/cache/base/rootfs",
		WorkspaceDir: "/state/sbx/workspace",
		IPCDir:       "/state/sbx/ipc",
	}
	without, err := Args(base)
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	if strings.Contains(strings.Join(without, " "), "/etc/resolv.conf") {
		t.Fatal("resolv.conf bound without a resolver configured")
	}

	base.ResolvConf = "/state/sbx/resolv.conf"
	with, err := Args(base)
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	if !strings.Contains(strings.Join(with, " "), "--ro-bind /state/sbx/resolv.conf /etc/resolv.conf") {
		t.Fatalf("argv missing the resolv.conf bind: %v", with)
	}
}

// The system unit gives the runner ambient CAP_SYS_ADMIN/CAP_NET_ADMIN so
// nsenter can join a sandbox's netns, and bwrap refuses to run holding
// capabilities when it is not setuid. The launch must clear them in between.
func TestNsenterBwrapArgsDropCapabilitiesBeforeBwrap(t *testing.T) {
	got := nsenterBwrapArgs(4242, "/usr/bin/bwrap", []string{"--unshare-user", "--", "/vora/vora-init"})
	want := []string{"-t", "4242", "-n", "setpriv", "--ambient-caps=-all", "--inh-caps=-all", "--", "/usr/bin/bwrap", "--unshare-user", "--", "/vora/vora-init"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nsenter argv = %v\nwant           %v", got, want)
	}
}
