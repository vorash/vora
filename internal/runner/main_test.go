package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// A resolver that finds a containerd socket tries to pull every image containerd
// does not have, and a pull's resolve runs in this process. Unit tests would then
// call a public registry for their made-up images, so no server built in this
// binary may find a socket, whichever config it was built from.
func TestMain(m *testing.M) {
	if err := os.Setenv("VORA_CONTAINERD_ADDRESS", filepath.Join(os.TempDir(), "vora-unit-tests-have-no-containerd.sock")); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestUnitTestServersCannotReachARegistry(t *testing.T) {
	if testServer(t, testRegistry(t)).images.UsesContainerd() {
		t.Fatal("a unit test server connected to containerd, so its image lookups can pull from a real registry")
	}
}
