// Package state is Strata's audit trail and project state.
//
// Every fact is an Event appended to a JSON-lines file. Events are hash
// chained (each stores the previous event's hash), so editing, deleting or
// reordering a line is detected when the log is opened. Nothing is ever
// rewritten: even a rollback is a new event. Current state is computed by
// replaying the events (see Replay).
package state

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Event is one entry in the audit log.
type Event struct {
	Seq      int             `json:"seq"`
	Time     time.Time       `json:"time"`
	Actor    string          `json:"actor"` // person id, or "system"
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
	PrevHash string          `json:"prev_hash"`
	Hash     string          `json:"hash"`
}

// Log is an append-only, hash-chained event file.
type Log struct {
	path   string
	events []Event
	Now    func() time.Time // replaceable in tests
}

// Create starts a new, empty log. It refuses to overwrite an existing one.
func Create(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%s already exists; refusing to overwrite an audit log", path)
		}
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return &Log{path: path, Now: time.Now}, nil
}

// Open reads a log and verifies its hash chain.
func Open(path string) (*Log, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for line := 1; sc.Scan(); line++ {
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(b, &e); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := Verify(events); err != nil {
		return nil, fmt.Errorf("%s: audit log failed verification: %w", path, err)
	}
	return &Log{path: path, events: events, Now: time.Now}, nil
}

// Verify checks sequence numbers and the hash chain.
func Verify(events []Event) error {
	prev := ""
	for i, e := range events {
		if e.Seq != i+1 {
			return fmt.Errorf("event at position %d has seq %d (missing or reordered event)", i+1, e.Seq)
		}
		if e.PrevHash != prev {
			return fmt.Errorf("event %d: prev_hash does not match event %d", e.Seq, e.Seq-1)
		}
		h, err := hashOf(e)
		if err != nil {
			return fmt.Errorf("event %d: %w", e.Seq, err)
		}
		if h != e.Hash {
			return fmt.Errorf("event %d: hash mismatch (event was modified)", e.Seq)
		}
		prev = e.Hash
	}
	return nil
}

// Events returns a copy of all events, including ones undone by rollbacks.
func (l *Log) Events() []Event { return append([]Event(nil), l.events...) }

// Append writes one event and fsyncs it before returning.
func (l *Log) Append(actor, typ string, data any) (Event, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, err
	}
	e := Event{Seq: len(l.events) + 1, Time: l.Now().UTC(), Actor: actor, Type: typ, Data: raw}
	if n := len(l.events); n > 0 {
		e.PrevHash = l.events[n-1].Hash
	}
	if e.Hash, err = hashOf(e); err != nil {
		return Event{}, err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return Event{}, err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return Event{}, err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return Event{}, err
	}
	if err := f.Sync(); err != nil {
		return Event{}, err
	}
	l.events = append(l.events, e)
	return e, nil
}

func hashOf(e Event) (string, error) {
	var data bytes.Buffer
	if len(e.Data) > 0 {
		if err := json.Compact(&data, e.Data); err != nil {
			return "", err
		}
	}
	h := sha256.New()
	fmt.Fprintf(h, "%d\n%s\n%s\n%s\n%s\n", e.Seq, e.Time.UTC().Format(time.RFC3339Nano), e.Actor, e.Type, e.PrevHash)
	h.Write(data.Bytes())
	return hex.EncodeToString(h.Sum(nil)), nil
}
