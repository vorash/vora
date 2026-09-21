//go:build security

package security

import (
	"context"
	"fmt"
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
//
// Set VORA_QUOTA_TEST_DIR to the real, prjquota-mounted state directory (e.g.
// /var/lib/vora/sandboxes) when validating a host; otherwise it runs under the
// test temp dir, which usually has no project quotas and SKIPs.
func TestDiskQuotaBoundsHostWrites(t *testing.T) {
	root := quotaTestRoot(t)
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

// quotaTestRoot returns the directory the quota test runs against: the real
// mounted state dir when VORA_QUOTA_TEST_DIR is set, otherwise a temp dir.
func quotaTestRoot(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("VORA_QUOTA_TEST_DIR")
	if dir == "" {
		return t.TempDir()
	}
	root := filepath.Join(dir, fmt.Sprintf("vora-quota-test-%d", os.Getpid()))
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create VORA_QUOTA_TEST_DIR %s: %v", root, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}
