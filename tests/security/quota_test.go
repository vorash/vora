//go:build security

package security

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/vorash/vora/internal/quota"
)

// TestDiskQuotaBoundsHostWrites asserts the hard guarantee: on a host that
// enforces project quotas, a process that ignores the limit cannot push the
// writable directories past DiskBytes, so the host filesystem is never
// overrun. When the host cannot enforce project quotas the test is SKIPped
// with the reason — the soft enforcer is covered by the integration suite.
func TestDiskQuotaBoundsHostWrites(t *testing.T) {
	root := t.TempDir()
	m := quota.New(root)
	if m.Mode() != quota.ModeProject {
		t.Skipf("host does not enforce project quotas (mode=%s): needs root, chattr/setquota and a prjquota mount", m.Mode())
	}

	ws := filepath.Join(root, "sandboxes", "sbx_quota", "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	const limit = int64(1 << 20)
	ctx := context.Background()
	if err := m.Enforce(ctx, "sbx_quota", []string{ws}, limit); err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	t.Cleanup(func() { _ = m.Release(context.Background(), "sbx_quota", []string{ws}) })

	f, err := os.Create(filepath.Join(ws, "fill"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := io.Copy(f, io.LimitReader(neverReader{}, 8<<20)); err == nil {
		t.Fatal("wrote past the quota without an error")
	}

	used, err := m.Usage([]string{ws})
	if err != nil {
		t.Fatal(err)
	}
	if used > limit {
		t.Fatalf("workspace holds %d bytes, over the %d limit", used, limit)
	}
}

// neverReader yields zero bytes forever.
type neverReader struct{}

func (neverReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
