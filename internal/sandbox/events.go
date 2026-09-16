package sandbox

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// EventsFile is the lifecycle event log inside a sandbox's logs directory.
const EventsFile = "events.jsonl"

// Event is one lifecycle transition, persisted for audit.
type Event struct {
	SandboxID string    `json:"sandbox_id"`
	From      State     `json:"from"`
	To        State     `json:"to"`
	Reason    string    `json:"reason,omitempty"`
	RunnerID  string    `json:"runner_id,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// AppendEvent appends an event to dir/events.jsonl, creating the file if
// needed. dir must already exist.
func AppendEvent(dir string, e Event) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("sandbox: marshal event: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, EventsFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("sandbox: open events: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("sandbox: write event: %w", err)
	}
	return nil
}

// ReadEvents reads all events from dir/events.jsonl. A missing file yields no
// events and no error.
func ReadEvents(dir string) ([]Event, error) {
	f, err := os.Open(filepath.Join(dir, EventsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var events []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue // skip malformed lines rather than failing the whole log
		}
		events = append(events, e)
	}
	return events, scanner.Err()
}
