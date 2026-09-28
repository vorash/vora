package config

import (
	"testing"
	"time"
)

func TestEgressLimitsDefaultsAndOverrides(t *testing.T) {
	t.Setenv("VORA_EGRESS_MAX_CONNECTIONS", "")
	t.Setenv("VORA_EGRESS_IDLE_TIMEOUT", "")
	cfg, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EgressMaxConnections != 128 || cfg.EgressIdleTimeout != 5*time.Minute {
		t.Fatalf("defaults = %d, %s", cfg.EgressMaxConnections, cfg.EgressIdleTimeout)
	}

	t.Setenv("VORA_EGRESS_MAX_CONNECTIONS", "32")
	t.Setenv("VORA_EGRESS_IDLE_TIMEOUT", "45s")
	cfg, err = Default()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EgressMaxConnections != 32 || cfg.EgressIdleTimeout != 45*time.Second {
		t.Fatalf("overrides = %d, %s", cfg.EgressMaxConnections, cfg.EgressIdleTimeout)
	}
}

func TestEgressLimitsRejectInvalidValues(t *testing.T) {
	for _, tc := range []struct {
		key, value string
	}{
		{"VORA_EGRESS_MAX_CONNECTIONS", "0"},
		{"VORA_EGRESS_MAX_CONNECTIONS", "many"},
		{"VORA_EGRESS_IDLE_TIMEOUT", "0s"},
		{"VORA_EGRESS_IDLE_TIMEOUT", "later"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Setenv("VORA_EGRESS_MAX_CONNECTIONS", "")
			t.Setenv("VORA_EGRESS_IDLE_TIMEOUT", "")
			t.Setenv(tc.key, tc.value)
			if _, err := Default(); err == nil {
				t.Fatal("expected invalid value to fail")
			}
		})
	}
}

// The audit trail is kept forever unless the operator opts into an age limit:
// deleting evidence must be a decision, never a default.
func TestAuditRetentionDefaultsToKeepForever(t *testing.T) {
	t.Setenv("VORA_AUDIT_RETENTION", "")
	cfg, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuditRetention != 0 {
		t.Fatalf("AuditRetention = %s, want 0 (keep forever)", cfg.AuditRetention)
	}

	t.Setenv("VORA_AUDIT_RETENTION", "720h")
	cfg, err = Default()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuditRetention != 720*time.Hour {
		t.Fatalf("AuditRetention = %s, want 720h", cfg.AuditRetention)
	}
}

func TestAuditRetentionRejectsInvalidValues(t *testing.T) {
	for _, v := range []string{"0s", "-1h", "soon"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("VORA_AUDIT_RETENTION", v)
			if _, err := Default(); err == nil {
				t.Fatal("expected invalid value to fail")
			}
		})
	}
}
