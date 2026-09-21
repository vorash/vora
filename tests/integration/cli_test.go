//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCLIWorkflow exercises the vora CLI against a real runner, covering the
// argument-passing behaviour (the `--` separator) and exit-code propagation.
func TestCLIWorkflow(t *testing.T) {
	rootfs := imageRootfs(t, "base")
	realImages := filepath.Dir(filepath.Dir(filepath.Dir(rootfs))) // .../images

	binDir := t.TempDir()
	for _, pkg := range []string{"vora", "vora-runner", "vora-init"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, pkg), "./cmd/"+pkg)
		cmd.Dir = filepath.Join("..", "..")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, out)
		}
	}

	stateDir := t.TempDir()
	if err := os.Symlink(realImages, filepath.Join(stateDir, "images")); err != nil {
		t.Fatalf("symlink images: %v", err)
	}
	socket := filepath.Join(t.TempDir(), "runner.sock")

	runner := exec.Command(filepath.Join(binDir, "vora-runner"))
	runner.Env = append(os.Environ(), "VORA_STATE_DIR="+stateDir, "VORA_SOCKET="+socket)
	runner.Stderr = os.Stderr
	if err := runner.Start(); err != nil {
		t.Fatalf("start runner: %v", err)
	}
	t.Cleanup(func() {
		_ = runner.Process.Signal(syscall.SIGTERM)
		_, _ = runner.Process.Wait()
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	vora := func(args ...string) (string, int) {
		full := append([]string{"--socket", socket}, args...)
		cmd := exec.Command(filepath.Join(binDir, "vora"), full...)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run vora %v: %v", args, err)
		}
		return string(out), code
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run not installed")
	}

	out, code := vora("sandbox", "create", "--image", "vora/base", "--memory", "256m", "--cpu", "1", "--pids", "64")
	if code != 0 {
		t.Fatalf("create failed (%d): %s", code, out)
	}
	id := ""
	if fields := strings.Fields(out); len(fields) >= 3 {
		id = fields[2]
	}
	if !strings.HasPrefix(id, "sbx_") {
		t.Fatalf("could not parse sandbox id from %q", out)
	}
	t.Cleanup(func() { vora("sandbox", "destroy", id) })

	out, code = vora("exec", id, "--", "/bin/echo", "hello", "vora")
	if code != 0 || strings.TrimSpace(out) != "hello vora" {
		t.Fatalf("exec = %q (code %d)", out, code)
	}

	// Exit codes propagate through the CLI.
	if _, code = vora("exec", id, "--", "/bin/sh", "-c", "exit 7"); code != 7 {
		t.Fatalf("exit propagation = %d, want 7", code)
	}

	// cp round-trips a file, including parents created by cp.
	local := filepath.Join(t.TempDir(), "local.txt")
	if err := os.WriteFile(local, []byte("round trip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code = vora("cp", local, id+":/workspace/nested/local.txt"); code != 0 {
		t.Fatalf("cp in failed (%d): %s", code, out)
	}
	out, code = vora("exec", id, "--", "cat", "/workspace/nested/local.txt")
	if code != 0 || strings.TrimSpace(out) != "round trip" {
		t.Fatalf("cp readback = %q (code %d)", out, code)
	}
	outPath := filepath.Join(t.TempDir(), "out.txt")
	if out, code = vora("cp", id+":/workspace/nested/local.txt", outPath); code != 0 {
		t.Fatalf("cp out failed (%d): %s", code, out)
	}
	if data, err := os.ReadFile(outPath); err != nil || string(data) != "round trip\n" {
		t.Fatalf("cp out = %q, %v", data, err)
	}

	out, code = vora("logs", id)
	if code != 0 || !strings.Contains(out, "RUNNING") {
		t.Fatalf("logs = %q (code %d)", out, code)
	}

	if out, code = vora("sandbox", "destroy", id); code != 0 {
		t.Fatalf("destroy failed (%d): %s", code, out)
	}
}
