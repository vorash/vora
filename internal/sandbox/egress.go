package sandbox

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// EgressFile is the network policy decision log inside a sandbox's log dir.
const EgressFile = "egress.jsonl"

// EgressRecord is one durable restricted-network policy decision. Alias is set
// when the decision was made by the credential broker and is never a secret
// value.
type EgressRecord struct {
	SandboxID string    `json:"sandbox_id"`
	Host      string    `json:"host,omitempty"`
	Remote    string    `json:"remote,omitempty"`
	Alias     string    `json:"alias,omitempty"`
	Allowed   bool      `json:"allowed"`
	Reason    string    `json:"reason,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// AppendEgress appends one policy decision to dir/egress.jsonl.
func AppendEgress(dir string, record EgressRecord) error {
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now().UTC()
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("sandbox: marshal egress record: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, EgressFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("sandbox: open egress log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("sandbox: write egress record: %w", err)
	}
	return nil
}

// ReadEgress returns policy decisions oldest first. Malformed lines are
// skipped so one damaged record does not hide the rest of the audit trail.
func ReadEgress(dir string) ([]EgressRecord, error) {
	f, err := os.Open(filepath.Join(dir, EgressFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var records []EgressRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var record EgressRecord
		if json.Unmarshal(scanner.Bytes(), &record) == nil {
			records = append(records, record)
		}
	}
	return records, scanner.Err()
}
