package main

import (
	"path/filepath"
	"testing"
)

// The agent socket accepts exec, signal and shutdown from any client that can
// open it. It lives in a directory the runner's UID owns, so no other user
// should ever be able to connect: 0666 handed that to every local user
// whenever the state directory happened to be traversable.
func TestAgentSocketIsOwnerOnly(t *testing.T) {
	lis, err := listenAgent(filepath.Join(t.TempDir(), "ipc", "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	assertMode(t, lis.Addr().String(), 0o600)
}
