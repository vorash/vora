package sandbox

import (
	"testing"
	"time"
)

func TestEgressJSONLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := []EgressRecord{
		{SandboxID: "sbx_test", Host: "example.com", Remote: "93.184.216.34:443", Allowed: true, Timestamp: time.Now().UTC().Truncate(time.Nanosecond)},
		{SandboxID: "sbx_test", Host: "evil.test", Allowed: false, Reason: "domain_not_allowed", Timestamp: time.Now().UTC().Add(time.Millisecond).Truncate(time.Nanosecond)},
	}
	for _, record := range want {
		if err := AppendEgress(dir, record); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadEgress(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("records = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
