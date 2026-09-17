//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"
)

// TestSandboxBrokerFlagsFailClosed asserts the CLI refuses --broker without the
// control plane (secrets live there) or outside restricted networking, before
// any sandbox is created and before it even dials.
func TestSandboxBrokerFlagsFailClosed(t *testing.T) {
	bin := buildBinary(t, "vora")

	cmd := exec.Command(bin, "sandbox", "create", "--broker", "github", "--network", "restricted")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("--broker without --api succeeded: %s", out)
	}
	if !strings.Contains(string(out), "--api") {
		t.Fatalf("error does not mention --api: %s", out)
	}

	cmd = exec.Command(bin, "--api", "http://127.0.0.1:1", "--api-key", "vora_test", "sandbox", "create",
		"--broker", "github", "--network", "none")
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("--broker with --network none succeeded: %s", out)
	}
	if !strings.Contains(string(out), "restricted") {
		t.Fatalf("error does not mention restricted: %s", out)
	}
}
