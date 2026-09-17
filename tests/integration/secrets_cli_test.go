//go:build integration

package integration

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/vorash/vora/internal/api"
	"github.com/vorash/vora/internal/secret"
)

// TestSecretsCLIWorkflow drives `vora secrets` against a real vora-api HTTP
// endpoint: a value goes in over stdin, only metadata comes back out, and no
// command ever echoes the value.
func TestSecretsCLIWorkflow(t *testing.T) {
	sealer, err := secret.NewSealer("integration-cli-master-key-long-enough")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	store := api.NewMemStore()
	plaintext, hash, err := api.NewAPIKey()
	if err != nil {
		t.Fatalf("api key: %v", err)
	}
	if err := store.CreateKey(context.Background(), api.APIKey{
		ID: "key_cli", Name: "cli", TenantID: "t1", KeyHash: hash,
		Scopes: []string{api.ScopeAdmin, api.ScopeSandbox},
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	secrets := secret.NewMemStore(sealer)
	apiSrv := api.New(api.Options{Store: store, Secrets: secrets})
	t.Cleanup(func() { _ = apiSrv.Close() })
	ts := httptest.NewServer(apiSrv.Handler())
	defer ts.Close()

	bin := buildBinary(t, "vora")
	run := func(stdin string, args ...string) (string, int) {
		t.Helper()
		full := append([]string{"--api", ts.URL, "--api-key", plaintext}, args...)
		cmd := exec.Command(bin, full...)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run vora %v: %v", args, err)
		}
		return out.String(), code
	}

	const value = "ghp_cli_secret_value"
	out, code := run(value+"\n", "secrets", "set", "--alias", "github",
		"--host", "api.github.com", "--scheme", "bearer", "--value-stdin")
	if code != 0 {
		t.Fatalf("set failed (%d): %s", code, out)
	}
	if !strings.Contains(out, "github") || !strings.Contains(out, "api.github.com") {
		t.Fatalf("set output = %q", out)
	}
	if strings.Contains(out, value) {
		t.Fatal("set echoed the credential value")
	}

	out, code = run("", "secrets", "list")
	if code != 0 {
		t.Fatalf("list failed (%d): %s", code, out)
	}
	if !strings.Contains(out, "github") || !strings.Contains(out, "api.github.com") {
		t.Fatalf("list output = %q", out)
	}
	if strings.Contains(out, value) {
		t.Fatal("list echoed the credential value")
	}

	// The stored value must be the one that went in, without the trailing
	// newline that stdin carried.
	if _, got, err := secrets.Reveal(context.Background(), "t1", "github"); err != nil || got != value {
		t.Fatalf("stored value = %q, %v", got, err)
	}

	out, code = run("", "secrets", "rm", "github")
	if code != 0 {
		t.Fatalf("rm failed (%d): %s", code, out)
	}
	out, code = run("", "secrets", "list")
	if code != 0 || !strings.Contains(out, "no secrets") {
		t.Fatalf("list after rm = %q (%d)", out, code)
	}

	// Without --api the command must refuse instead of silently storing
	// nothing.
	cmd := exec.Command(bin, "secrets", "list")
	outBytes, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("secrets list without --api succeeded: %s", outBytes)
	}
	if !strings.Contains(string(outBytes), "--api") {
		t.Fatalf("error does not mention --api: %s", outBytes)
	}
}
