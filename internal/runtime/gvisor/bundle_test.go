package gvisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/vorash/vora/internal/runtime"
)

// fakeRunsc stands in for the real `runsc spec`: it records its argv and writes
// a baseline OCI spec into the current directory, which is all writeBundle
// consumes before overriding the fields Vora cares about.
const fakeRunsc = `#!/bin/sh
printf '%s\n' "$@" > argv.txt
cat > config.json <<'JSON'
{"ociVersion":"1.0.2-dev","process":{"args":["/bin/sh"],"cwd":"/","env":["PATH=/usr/bin"]},"root":{"path":"rootfs","readonly":false}}
JSON
`

func TestWriteBundleUsesVoraInit(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	if err := os.WriteFile(runsc, []byte(fakeRunsc), 0o755); err != nil {
		t.Fatalf("write fake runsc: %v", err)
	}

	paths := runtime.WorkspaceSpec{
		RootDir:      filepath.Join(dir, "sbx"),
		RootfsDir:    filepath.Join(dir, "rootfs"),
		WorkspaceDir: filepath.Join(dir, "workspace"),
		HomeDir:      filepath.Join(dir, "home"),
		IPCDir:       filepath.Join(dir, "ipc"),
	}
	for _, d := range []string{paths.RootfsDir, paths.WorkspaceDir, paths.HomeDir, paths.IPCDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	bundle := filepath.Join(paths.RootDir, "bundle")

	spec := runtime.SandboxSpec{ID: "sbx_test", Workspace: paths, Env: map[string]string{"FOO": "bar"}}
	if err := writeBundle(runsc, bundle, spec, "", false); err != nil {
		t.Fatalf("writeBundle: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var s specs.Spec
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The init is Vora's static binary in pause mode, never the image's shell.
	if s.Process == nil {
		t.Fatal("spec has no process")
	}
	want := []string{initInside, initMode}
	if !reflect.DeepEqual(s.Process.Args, want) {
		t.Errorf("init args = %v, want %v", s.Process.Args, want)
	}
	for _, a := range s.Process.Args {
		if strings.Contains(a, "/bin/sh") {
			t.Errorf("init still depends on the image shell: %v", s.Process.Args)
		}
	}
	if s.Process.Cwd != "/workspace" {
		t.Errorf("cwd = %q, want /workspace", s.Process.Cwd)
	}

	// The ipc directory is bound at /vora, which is how the init binary
	// reaches the sandbox without the image shipping it. It must be read-only:
	// a workload that could write there could replace the init of a reused sandbox.
	var bound bool
	for _, m := range s.Mounts {
		if m.Destination == "/vora" && m.Source == paths.IPCDir {
			bound = true
			if !slices.Contains(m.Options, "ro") || slices.Contains(m.Options, "rw") {
				t.Errorf("/vora options = %v, want read-only", m.Options)
			}
		}
	}
	if !bound {
		t.Errorf("ipc dir not bound at /vora: %+v", s.Mounts)
	}

	// The image rootfs is bound read-only.
	if s.Root == nil || s.Root.Path != paths.RootfsDir || !s.Root.Readonly {
		t.Errorf("root = %+v, want %s read-only", s.Root, paths.RootfsDir)
	}

	// `runsc spec` is also told to seed the same init, so the template and the
	// override agree.
	argv, err := os.ReadFile(filepath.Join(bundle, "argv.txt"))
	if err != nil {
		t.Fatalf("read argv.txt: %v", err)
	}
	if got := string(argv); !strings.Contains(got, initInside) || !strings.Contains(got, initMode) {
		t.Errorf("runsc spec argv = %q, want it to seed %s %s", got, initInside, initMode)
	}
	if strings.Contains(string(argv), "/bin/sh") {
		t.Errorf("runsc spec argv still names /bin/sh: %q", argv)
	}
}
