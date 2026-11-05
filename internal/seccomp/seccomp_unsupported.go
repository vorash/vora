//go:build !linux

package seccomp

import "fmt"

// Install fails on non-Linux platforms: seccomp is a Linux facility.
func Install(bool) error {
	return fmt.Errorf("seccomp: not supported on this platform")
}
