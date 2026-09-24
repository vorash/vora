package gvisor

import (
	"reflect"
	"testing"

	"github.com/vorash/vora/internal/runtime"
)

func TestExecArgsPreserveCwdEnvAndSeparateIdentifiers(t *testing.T) {
	got := execArgs("sbx_test", runtime.ExecRequest{Command: "-command", Args: []string{"-flag"}, Cwd: "/workspace", Env: map[string]string{"FOO": "bar", "A": "b"}})
	want := []string{"exec", "-cwd", "/workspace", "-env", "A=b", "-env", "FOO=bar", "--", "sbx_test", "-command", "-flag"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exec argv = %v, want %v", got, want)
	}
}
