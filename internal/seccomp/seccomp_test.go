//go:build linux

package seccomp

import (
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFilterStructure(t *testing.T) {
	f := Filter()
	if len(f) < 6 {
		t.Fatalf("filter too short: %d instructions", len(f))
	}
	// Prologue: load arch, compare, kill on mismatch, load syscall number.
	if f[0].Code != unix.BPF_LD|unix.BPF_W|unix.BPF_ABS || f[0].K != 4 {
		t.Fatalf("first instruction must load arch: %+v", f[0])
	}
	if f[2].K != unix.SECCOMP_RET_KILL_PROCESS {
		t.Fatalf("arch mismatch must kill: %+v", f[2])
	}
	if f[3].Code != unix.BPF_LD|unix.BPF_W|unix.BPF_ABS || f[3].K != 0 {
		t.Fatalf("fourth instruction must load nr: %+v", f[3])
	}
	// Default allow, and every denied syscall returns EPERM, not kill.
	if last := f[len(f)-1]; last.K != unix.SECCOMP_RET_ALLOW {
		t.Fatalf("final instruction must allow: %+v", last)
	}
	deny := unix.SECCOMP_RET_ERRNO | (uint32(unix.EPERM) & unix.SECCOMP_RET_DATA)
	for _, instr := range f[4 : len(f)-1] {
		if instr.Code == unix.BPF_RET|unix.BPF_K && instr.K != deny {
			t.Fatalf("deny instruction returns %#x, want EPERM %#x", instr.K, deny)
		}
	}
}

// TestInstallBlocksDeniedSyscall re-executes the test binary as a child (the
// filter is process-wide and would poison the test runner), installs the
// filter there, and checks a denied syscall fails while an allowed one works.
// It fails if the filter cannot be installed, so a silent unfiltered sandbox
// is caught.
func TestInstallBlocksDeniedSyscall(t *testing.T) {
	if os.Getenv("VORA_SECCOMP_TEST_CHILD") == "1" {
		if err := Install(); err != nil {
			t.Fatalf("Install: %v", err)
		}
		if _, _, errno := unix.Syscall6(unix.SYS_KEYCTL, 0, 0, 0, 0, 0, 0); errno != unix.EPERM {
			t.Fatalf("keyctl errno = %v, want EPERM", errno)
		}
		if _, _, errno := unix.Syscall(unix.SYS_GETPID, 0, 0, 0); errno != 0 {
			t.Fatalf("getpid errno = %v, want 0", errno)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestInstallBlocksDeniedSyscall")
	cmd.Env = append(os.Environ(), "VORA_SECCOMP_TEST_CHILD=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("filtered child failed: %v\n%s", err, out)
	}
}
