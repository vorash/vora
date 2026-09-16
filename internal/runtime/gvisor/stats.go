package gvisor

import (
	"encoding/json"

	"github.com/vorash/vora/internal/runtime"
)

// parseStats best-effort parses `runsc events --stats` output. The exact shape
// varies across gVisor releases, so unknown fields are ignored and zero stats
// are returned when nothing matches.
func parseStats(out string) *runtime.Stats {
	st := &runtime.Stats{}
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return st
	}
	if v, ok := findInt(raw, "memory_usage", "memoryUsage", "usage", "resident"); ok {
		st.MemoryBytes = v
	}
	if v, ok := findInt(raw, "peak_memory", "memory_peak", "peakMemory"); ok {
		st.MemoryPeak = v
	}
	return st
}

// findInt recursively searches for the first matching key with an integer value.
func findInt(v any, keys ...string) (int64, bool) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range keys {
			if raw, ok := t[k]; ok {
				if n, ok := toInt(raw); ok {
					return n, true
				}
			}
		}
		for _, raw := range t {
			if n, ok := findInt(raw, keys...); ok {
				return n, true
			}
		}
	case []any:
		for _, raw := range t {
			if n, ok := findInt(raw, keys...); ok {
				return n, true
			}
		}
	}
	return 0, false
}

func toInt(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}
