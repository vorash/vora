package gvisor

import "testing"

// Under the non-root system unit the runner is the vora user. runsc supports
// neither that nor Method 2 there, so the backend must not be registered: a
// tenant asking for gVisor gets "unknown backend" instead of a create that
// fails after the fact.
func TestGvisorIsOfferedOnlyAtUID0(t *testing.T) {
	if err := requireUID0(0); err != nil {
		t.Fatalf("uid 0 refused: %v", err)
	}
	if err := requireUID0(997); err == nil {
		t.Fatal("non-root runner accepted for gVisor")
	}
}
