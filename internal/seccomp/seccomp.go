//go:build linux

// Package seccomp installs a deny-list seccomp filter for Bubblewrap
// sandboxes. The in-sandbox init installs it before serving, so every process
// it starts inherits it. Denied syscalls fail with EPERM rather than killing
// the process, so a workload gets a clear error instead of a silent death.
//
// The filter is deliberately a deny-list, not an allow-list: an allow-list has
// to enumerate every syscall every image's runtime needs, which breaks
// workloads in ways that are hard to diagnose. The deny-list removes the
// syscalls a sandboxed workload has no legitimate need for and that are common
// kernel attack surface (ptrace, module loading, the mount and keyring APIs,
// bpf, kexec, ...).
//
// A sandbox that will be reset also denies IPCSyscalls. See its comment.
package seccomp

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// deniedSyscalls is the syscall deny-list. It uses x/sys/unix's SYS_* names so
// the numbers are correct for the architecture Vora was built for.
func deniedSyscalls() []uint32 {
	return []uint32{
		// Debugging another process.
		unix.SYS_PTRACE,
		unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_KCMP,
		// Mount namespace / filesystem topology changes (the sandbox is
		// already in its namespaces and must not reshuffle them).
		unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT,
		unix.SYS_OPEN_TREE, unix.SYS_MOVE_MOUNT, unix.SYS_FSOPEN,
		unix.SYS_FSCONFIG, unix.SYS_FSMOUNT, unix.SYS_MOUNT_SETATTR,
		unix.SYS_SETNS, unix.SYS_UNSHARE,
		// Kernel modules and kexec.
		unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE,
		unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD,
		// System-wide power, time and identity.
		unix.SYS_REBOOT, unix.SYS_SWAPON, unix.SYS_SWAPOFF,
		unix.SYS_SETHOSTNAME, unix.SYS_SETDOMAINNAME,
		unix.SYS_SETTIMEOFDAY, unix.SYS_CLOCK_SETTIME, unix.SYS_ADJTIMEX,
		unix.SYS_ACCT,
		// Kernel introspection and keyring.
		unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN,
		unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
		unix.SYS_USERFAULTFD, unix.SYS_FANOTIFY_INIT, unix.SYS_SYSLOG,
		unix.SYS_LOOKUP_DCOOKIE, unix.SYS_USELIB,
		unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY, unix.SYS_KEYCTL,
		// I/O privilege and file-handle bypasses.
		unix.SYS_IOPL, unix.SYS_IOPERM,
		unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_NAME_TO_HANDLE_AT,
		unix.SYS_QUOTACTL, unix.SYS_NFSSERVCTL,
	}
}

// ipcSyscalls is IPCSyscalls as this architecture's numbers.
func ipcSyscalls() []uint32 {
	return []uint32{
		unix.SYS_SHMGET, unix.SYS_SEMGET, unix.SYS_MSGGET,
		unix.SYS_MQ_OPEN, unix.SYS_MQ_UNLINK, unix.SYS_MQ_TIMEDSEND,
		unix.SYS_MQ_TIMEDRECEIVE, unix.SYS_MQ_NOTIFY, unix.SYS_MQ_GETSETATTR,
	}
}

// Filter returns the BPF program to install, with IPCSyscalls denied as well when
// denyIPC is set. It is exported so it can be written to a file and handed to a
// sandbox that applies its own filter.
func Filter(denyIPC bool) []unix.SockFilter {
	filter := []unix.SockFilter{
		// Load seccomp_data.arch.
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		// If it does not match the build architecture, kill the process: a
		// mismatch means the syscall numbers below are meaningless.
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, Jf: 0, K: auditArch()},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		// Load seccomp_data.nr.
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
	}
	deny := unix.SECCOMP_RET_ERRNO | (uint32(unix.EPERM) & unix.SECCOMP_RET_DATA)
	if runtime.GOARCH == "amd64" {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K, Jt: 0, Jf: 1, K: 0x40000000},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		)
	}
	filter = append(filter,
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 4, K: unix.SYS_PERSONALITY},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, Jf: 0, K: 0xffffffff},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: unix.SYS_CLONE3},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 4, K: unix.SYS_CLONE},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jt: 0, Jf: 1, K: unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWNET | unix.CLONE_NEWPID | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS | unix.CLONE_NEWCGROUP | unix.CLONE_NEWTIME},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
	)
	denied := deniedSyscalls()
	if denyIPC {
		denied = append(denied, ipcSyscalls()...)
	}
	for _, nr := range denied {
		filter = append(filter,
			// If nr == denied, fall to the EPERM return; otherwise skip it.
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: nr},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		)
	}
	return append(filter, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
}

// Install applies the filter to the current thread and every other thread in
// the process (and so to every process started afterwards). It fails loudly
// when seccomp is unavailable rather than leaving the sandbox unfiltered.
func Install(denyIPC bool) error {
	arch := auditArch()
	if arch == 0 {
		return fmt.Errorf("seccomp: unsupported architecture %s", runtime.GOARCH)
	}
	filter := Filter(denyIPC)
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	// no_new_privs is per thread until TSYNC spreads it, and seccomp refuses a thread
	// without it, so both calls must run on the same one.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("seccomp: set no_new_privs: %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&prog))); errno != 0 {
		return fmt.Errorf("seccomp: install filter: %w", errno)
	}
	return nil
}

// auditArch returns the AUDIT_ARCH_* value for the build architecture, or 0 if
// Vora does not have a syscall table for it.
func auditArch() uint32 {
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64
	default:
		return 0
	}
}
