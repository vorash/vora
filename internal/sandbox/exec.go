package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/ulid"
)

// ExecRecord is the audit record for a single execution inside a sandbox.
type ExecRecord struct {
	ExecutionID string             `json:"execution_id"`
	SandboxID   string             `json:"sandbox_id"`
	Actor       Actor              `json:"actor"`
	Command     string             `json:"command"`
	Args        []string           `json:"args,omitempty"`
	Cwd         string             `json:"cwd,omitempty"`
	StartedAt   time.Time          `json:"started_at"`
	FinishedAt  time.Time          `json:"finished_at"`
	ExitCode    int                `json:"exit_code"`
	Reason      runtime.ExitReason `json:"reason"`
	StdoutBytes int64              `json:"stdout_bytes"`
	StderrBytes int64              `json:"stderr_bytes"`
	// CPUMillis is the CPU the sandbox consumed during this execution (the
	// cgroup cpu.stat usage delta across its window).
	CPUMillis int64 `json:"cpu_millis,omitempty"`
	// MemoryPeakBytes is the sandbox's memory peak during this execution's
	// window. MemoryPeakScope says whether the kernel could scope it to the
	// window or it is the lifetime peak. Records written before usage windows
	// hold the lifetime peak at exit with no scope.
	MemoryPeakBytes int64             `json:"memory_peak_bytes,omitempty"`
	MemoryPeakScope runtime.PeakScope `json:"memory_peak_scope,omitempty"`
	// OOMKills and PidsLimitHits are the cgroup's counter deltas across the
	// window. Windows are per sandbox, so concurrent executions in one sandbox
	// count each other's usage.
	OOMKills      int64 `json:"oom_kills,omitempty"`
	PidsLimitHits int64 `json:"pids_limit_hits,omitempty"`
	// EgressAllowed and EgressDenied count the restricted-network policy
	// decisions that fell inside this execution's window.
	EgressAllowed int64 `json:"egress_allowed,omitempty"`
	EgressDenied  int64 `json:"egress_denied,omitempty"`
}

// WriteExecRecord writes an execution record to dir/<execution_id>.json
// atomically. Records are append-only: it fails if one already exists for the
// ID, so a repeated ID can never replace the record of an earlier command.
func WriteExecRecord(dir string, rec ExecRecord) error {
	if !ulid.ValidPrefix(rec.ExecutionID, ulid.PrefixExec) {
		return fmt.Errorf("sandbox: invalid execution id %q", rec.ExecutionID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+rec.ExecutionID+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Unlike rename, link refuses to replace an existing record.
	return os.Link(tmp.Name(), filepath.Join(dir, rec.ExecutionID+".json"))
}

// ReadExecRecords returns execution records from dir, oldest first.
func ReadExecRecords(dir string) ([]ExecRecord, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var recs []ExecRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var rec ExecRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].StartedAt.Before(recs[j].StartedAt) })
	return recs, nil
}
