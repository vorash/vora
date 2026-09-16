package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vorash/vora/internal/runtime"
)

// ExecRecord is the audit record for a single execution inside a sandbox.
type ExecRecord struct {
	ExecutionID string             `json:"execution_id"`
	SandboxID   string             `json:"sandbox_id"`
	Command     string             `json:"command"`
	Args        []string           `json:"args,omitempty"`
	Cwd         string             `json:"cwd,omitempty"`
	StartedAt   time.Time          `json:"started_at"`
	FinishedAt  time.Time          `json:"finished_at"`
	ExitCode    int                `json:"exit_code"`
	Reason      runtime.ExitReason `json:"reason"`
	StdoutBytes int64              `json:"stdout_bytes"`
	StderrBytes int64              `json:"stderr_bytes"`
}

// WriteExecRecord writes an execution record to dir/<execution_id>.json
// atomically.
func WriteExecRecord(dir string, rec ExecRecord) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+rec.ExecutionID+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, rec.ExecutionID+".json"))
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
