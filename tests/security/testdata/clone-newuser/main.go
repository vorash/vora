package main

import (
	"os"
	"syscall"
)

func main() {
	pid, _, errno := syscall.RawSyscall(syscall.SYS_CLONE, uintptr(syscall.CLONE_NEWUSER)|uintptr(syscall.SIGCHLD), 0, 0)
	if errno == 0 {
		if pid == 0 {
			syscall.RawSyscall(syscall.SYS_EXIT, 0, 0, 0)
		}
		os.Exit(1)
	}
	if errno != syscall.EPERM && errno != syscall.ENOSYS {
		os.Exit(2)
	}
}
