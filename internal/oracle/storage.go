package oracle

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// EventType tags the durable records written to the event log.
const (
	EvConfigInstalled    = "config_installed"
	EvRoundOpened        = "round_opened"
	EvSubmissionAccepted = "submission_accepted"
	EvRoundFinalized     = "round_finalized"
	EvRoundVoided        = "round_voided"
)

type Event struct {
	// Seq is a strictly consecutive record number assigned by the store on
	// Append, starting at 1. Replay rejects gaps and out-of-order records.
	Seq           uint64              `json:"seq"`
	Type          string              `json:"type"`
	Config        *Config             `json:"config,omitempty"`
	Round         uint64              `json:"round,omitempty"`
	AtUnix        int64               `json:"at_unix_nano,omitempty"`
	ConfigVersion int64               `json:"config_version,omitempty"`
	Sub           *AcceptedSubmission `json:"submission,omitempty"`
	Report        *RoundReport        `json:"report,omitempty"`
	Reason        string              `json:"reason,omitempty"`
}

// LogIntegrityError pinpoints a broken record in the event log: a gap or
// out-of-order sequence number, or an unparseable record.
type LogIntegrityError struct {
	Line   int64
	Offset int64
	Detail string
}

func (e *LogIntegrityError) Error() string {
	return fmt.Sprintf("event log integrity error at line %d (offset %d): %s", e.Line, e.Offset, e.Detail)
}

// Store is an append-only durable event log.
type Store interface {
	Append(ev Event) error
	Replay(apply func(Event) error) error
	Close() error
}

// FileStore durably appends one JSON object per line and fsyncs each event.
//
// Integrity rules: every record carries a strictly consecutive Seq. A final
// line without a terminating newline is a torn tail left by a process killed
// mid-write; since Append only acknowledges a record after it is fully
// written and fsynced, a torn tail was never confirmed and is truncated at
// open (TruncatedTailBytes reports how many bytes were dropped). Any other
// corruption — an unparseable newline-terminated record, a gap, or an
// out-of-order Seq — fails the open with a LogIntegrityError instead of
// running on top of a broken history.
type FileStore struct {
	mu                 sync.Mutex
	f                  *os.File
	w                  *bufio.Writer
	nextSeq            uint64
	truncatedTailBytes int64
}

func OpenFileStore(path string) (*FileStore, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	nextSeq, tornAt, tornBytes, err := scanLog(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if tornBytes > 0 {
		if err := f.Truncate(tornAt); err != nil {
			f.Close()
			return nil, fmt.Errorf("truncate torn tail: %w", err)
		}
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			f.Close()
			return nil, err
		}
	}
	return &FileStore{f: f, w: bufio.NewWriterSize(f, 4096), nextSeq: nextSeq, truncatedTailBytes: tornBytes}, nil
}

// TruncatedTailBytes reports how many bytes of unconfirmed torn tail were
// dropped when the store was opened (0 = clean log).
func (s *FileStore) TruncatedTailBytes() int64 { return s.truncatedTailBytes }

// scanLog validates every complete record and returns the next sequence
// number. A trailing partial line is reported via tornAt/tornBytes instead
// of failing.
func scanLog(f *os.File) (nextSeq uint64, tornAt int64, tornBytes int64, err error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, 0, 0, err
	}
	nextSeq = 1
	var offset, line int64
	r := bufio.NewReader(f)
	for {
		chunk, readErr := r.ReadBytes('\n')
		if readErr == io.EOF {
			if len(chunk) > 0 {
				return nextSeq, offset, int64(len(chunk)), nil
			}
			return nextSeq, 0, 0, nil
		}
		if readErr != nil {
			return 0, 0, 0, readErr
		}
		line++
		if err := checkRecord(chunk[:len(chunk)-1], nextSeq, line, offset); err != nil {
			return 0, 0, 0, err
		}
		offset += int64(len(chunk))
		nextSeq++
	}
}

func checkRecord(data []byte, wantSeq uint64, line, offset int64) error {
	if len(data) == 0 {
		return &LogIntegrityError{Line: line, Offset: offset, Detail: "empty record"}
	}
	var ev Event
	if err := json.Unmarshal(data, &ev); err != nil {
		return &LogIntegrityError{Line: line, Offset: offset, Detail: fmt.Sprintf("corrupt record: %v", err)}
	}
	if ev.Seq != wantSeq {
		return &LogIntegrityError{Line: line, Offset: offset, Detail: fmt.Sprintf("seq %d out of order, expected %d (gap or duplicate)", ev.Seq, wantSeq)}
	}
	return nil
}

func (s *FileStore) Append(ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev.Seq = s.nextSeq
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := s.w.Write(data); err != nil {
		return err
	}
	if err := s.w.Flush(); err != nil {
		return err
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	s.nextSeq++
	return nil
}

func (s *FileStore) Replay(apply func(Event) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	sc := bufio.NewScanner(s.f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	var line, offset int64
	wantSeq := uint64(1)
	for sc.Scan() {
		data := sc.Bytes()
		offset += int64(len(data)) + 1
		line++
		if len(data) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(data, &ev); err != nil {
			return &LogIntegrityError{Line: line, Offset: offset, Detail: fmt.Sprintf("corrupt record: %v", err)}
		}
		if ev.Seq != wantSeq {
			return &LogIntegrityError{Line: line, Offset: offset, Detail: fmt.Sprintf("seq %d out of order, expected %d", ev.Seq, wantSeq)}
		}
		wantSeq++
		if err := apply(ev); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	_, err := s.f.Seek(0, io.SeekEnd)
	return err
}

func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.w.Flush(); err != nil {
		return err
	}
	return s.f.Close()
}

// MemoryStore keeps events in memory; useful for tests and ephemeral runs.
type MemoryStore struct {
	mu     sync.Mutex
	events []Event
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

func (m *MemoryStore) Append(ev Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev.Seq = uint64(len(m.events)) + 1
	m.events = append(m.events, ev)
	return nil
}

func (m *MemoryStore) Replay(apply func(Event) error) error {
	m.mu.Lock()
	snapshot := append([]Event(nil), m.events...)
	m.mu.Unlock()
	for _, ev := range snapshot {
		if err := apply(ev); err != nil {
			return err
		}
	}
	return nil
}

func (m *MemoryStore) Close() error { return nil }

func (m *MemoryStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events)
}
