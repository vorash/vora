package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	v1 "github.com/vorash/vora/api/proto/v1"
)

// parseMemory parses a human memory size into bytes.
// Accepts 512m, 512Mi, 512mb, 1g, 1GiB, or a bare byte count.
func parseMemory(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty memory value")
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"kib", 1 << 10}, {"ki", 1 << 10}, {"kb", 1 << 10}, {"k", 1 << 10},
		{"mib", 1 << 20}, {"mi", 1 << 20}, {"mb", 1 << 20}, {"m", 1 << 20},
		{"gib", 1 << 30}, {"gi", 1 << 30}, {"gb", 1 << 30}, {"g", 1 << 30},
		{"b", 1},
	}
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			mult = u.mult
			s = strings.TrimSuffix(s, u.suffix)
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory value %q", s)
	}
	if n <= 0 {
		return 0, fmt.Errorf("memory must be positive")
	}
	return n * mult, nil
}

// parseCPU parses a CPU amount into millis. Accepts "1" (one CPU) or "500m".
func parseCPU(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty cpu value")
	}
	if strings.HasSuffix(s, "m") {
		n, err := strconv.ParseInt(strings.TrimSuffix(s, "m"), 10, 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid cpu value %q", s)
		}
		return n, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("invalid cpu value %q", s)
	}
	return int64(f * 1000), nil
}

// parseNetwork maps a CLI network mode to the proto enum.
func parseNetwork(s string) (v1.NetworkMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none":
		return v1.NetworkMode_NETWORK_NONE, nil
	case "restricted":
		return v1.NetworkMode_NETWORK_RESTRICTED, nil
	case "full":
		return v1.NetworkMode_NETWORK_FULL, nil
	case "private":
		return v1.NetworkMode_NETWORK_PRIVATE, nil
	default:
		return v1.NetworkMode_NETWORK_MODE_UNSPECIFIED, fmt.Errorf("invalid network mode %q (want none, restricted, full or private)", s)
	}
}

// parseDuration parses a duration such as "30m" or "1h".
func parseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration must not be negative")
	}
	return d, nil
}
