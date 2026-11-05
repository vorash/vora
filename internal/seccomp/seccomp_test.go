//go:build linux

package seccomp

import (
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFilterStructure(t *testing.T) {
	f := Filter(false)
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
	// Default allow; clone3 returns ENOSYS so libc can fall back to clone.
	if last := f[len(f)-1]; last.K != unix.SECCOMP_RET_ALLOW {
		t.Fatalf("final instruction must allow: %+v", last)
	}
	deny := unix.SECCOMP_RET_ERRNO | (uint32(unix.EPERM) & unix.SECCOMP_RET_DATA)
	for _, instr := range f[4 : len(f)-1] {
		if instr.Code == unix.BPF_RET|unix.BPF_K && instr.K != deny && instr.K != unix.SECCOMP_RET_ERRNO|uint32(unix.ENOSYS) {
			t.Fatalf("deny instruction returns %#x, want EPERM %#x", instr.K, deny)
		}
	}
}

// TestInstallBlocksDeniedSyscall re-executes the test binary as a child (the
// filter is process-wide and would poison the test runner), installs the
// filter there, and checks a denied syscall fails while an allowed one works.
// It fails if the filter cannot be installed, so a silent unfiltered sandbox
// is caught.
func TestFilterBlocksNamespaceCloneAndX32(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nr    uint32
		flags uint32
		want  uint32
	}{
		{"plain clone", unix.SYS_CLONE, 0, unix.SECCOMP_RET_ALLOW},
		{"user namespace clone", unix.SYS_CLONE, unix.CLONE_NEWUSER, unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		{"mount namespace clone", unix.SYS_CLONE, unix.CLONE_NEWNS, unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		{"clone3 fallback", unix.SYS_CLONE3, 0, unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)},
		{"io_uring", unix.SYS_IO_URING_SETUP, 0, unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		{"userfaultfd", unix.SYS_USERFAULTFD, 0, unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		{"personality query", unix.SYS_PERSONALITY, 0xffffffff, unix.SECCOMP_RET_ALLOW},
		{"personality change", unix.SYS_PERSONALITY, 0, unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := evalFilter(Filter(false), auditArch(), tc.nr, tc.flags); got != tc.want {
				t.Fatalf("result=%#x, want %#x", got, tc.want)
			}
		})
	}
	if auditArch() == unix.AUDIT_ARCH_X86_64 {
		if got := evalFilter(Filter(false), auditArch(), uint32(unix.SYS_GETPID)|0x40000000, 0); got != unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM) {
			t.Fatalf("x32 syscall accepted: %#x", got)
		}
	}
}

func evalFilter(prog []unix.SockFilter, arch, nr, flags uint32) uint32 {
	var acc uint32
	for pc := 0; pc < len(prog); {
		in := prog[pc]
		switch in.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			switch in.K {
			case 0:
				acc = nr
			case 4:
				acc = arch
			case 16:
				acc = flags
			}
			pc++
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			if acc == in.K {
				pc += int(in.Jt) + 1
			} else {
				pc += int(in.Jf) + 1
			}
		case unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K:
			if acc >= in.K {
				pc += int(in.Jt) + 1
			} else {
				pc += int(in.Jf) + 1
			}
		case unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K:
			if acc&in.K != 0 {
				pc += int(in.Jt) + 1
			} else {
				pc += int(in.Jf) + 1
			}
		case unix.BPF_RET | unix.BPF_K:
			return in.K
		default:
			panic("unexpected seccomp instruction")
		}
	}
	panic("seccomp program did not return")
}

func TestInstallBlocksDeniedSyscall(t *testing.T) {
	if os.Getenv("VORA_SECCOMP_TEST_CHILD") == "1" {
		if err := Install(false); err != nil {
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

// The names go to runsc and the numbers into the agent's filter, so a call missing from
// either one leaves a backend open while the other looks fine.
func TestFilterDeniesIPCOnlyWhenAsked(t *testing.T) {
	numbers := map[string]uint32{
		"shmget": unix.SYS_SHMGET, "semget": unix.SYS_SEMGET, "msgget": unix.SYS_MSGGET,
		"mq_open": unix.SYS_MQ_OPEN, "mq_unlink": unix.SYS_MQ_UNLINK,
		"mq_timedsend": unix.SYS_MQ_TIMEDSEND, "mq_timedreceive": unix.SYS_MQ_TIMEDRECEIVE,
		"mq_notify": unix.SYS_MQ_NOTIFY, "mq_getsetattr": unix.SYS_MQ_GETSETATTR,
	}
	if len(IPCSyscalls) != len(numbers) {
		t.Fatalf("IPCSyscalls = %v, want exactly %d calls", IPCSyscalls, len(numbers))
	}
	eperm := unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)
	for _, name := range IPCSyscalls {
		nr, ok := numbers[name]
		if !ok {
			t.Fatalf("IPCSyscalls names %q, which this test does not know", name)
		}
		if got := evalFilter(Filter(true), auditArch(), nr, 0); got != eperm {
			t.Errorf("%s with denyIPC: result %#x, want EPERM", name, got)
		}
		if got := evalFilter(Filter(false), auditArch(), nr, 0); got != unix.SECCOMP_RET_ALLOW {
			t.Errorf("%s without denyIPC: result %#x, want allowed", name, got)
		}
	}
	// A sandbox that denies IPC must still deny everything else the base list does.
	if got := evalFilter(Filter(true), auditArch(), unix.SYS_KEYCTL, 0); got != eperm {
		t.Errorf("keyctl with denyIPC: result %#x, want EPERM", got)
	}
	if got := evalFilter(Filter(true), auditArch(), unix.SYS_GETPID, 0); got != unix.SECCOMP_RET_ALLOW {
		t.Errorf("getpid with denyIPC: result %#x, want allowed", got)
	}
}

// The real kernel, on a key that does not exist, so nothing is ever created here: the
// filter answers EPERM, and without it the kernel answers ENOENT.
func TestInstallDeniesIPCOnlyWhenAsked(t *testing.T) {
	if mode := os.Getenv("VORA_SECCOMP_IPC_CHILD"); mode != "" {
		deny := mode == "deny"
		if err := Install(deny); err != nil {
			t.Fatalf("Install: %v", err)
		}
		want := unix.ENOENT
		if deny {
			want = unix.EPERM
		}
		const noSuchKey = 0x766f7261
		if _, _, errno := unix.Syscall(unix.SYS_SHMGET, noSuchKey, 0, 0); errno != want {
			t.Fatalf("shmget errno = %v, want %v", errno, want)
		}
		return
	}
	for _, mode := range []string{"deny", "allow"} {
		cmd := exec.Command(os.Args[0], "-test.run=TestInstallDeniesIPCOnlyWhenAsked")
		cmd.Env = append(os.Environ(), "VORA_SECCOMP_IPC_CHILD="+mode)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s child failed: %v\n%s", mode, err, out)
		}
	}
}
