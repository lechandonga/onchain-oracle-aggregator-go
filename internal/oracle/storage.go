package oracle

import (
	"bufio"
	"encoding/json"
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
	Type          string              `json:"type"`
	Config        *Config             `json:"config,omitempty"`
	Round         uint64              `json:"round,omitempty"`
	AtUnix        int64               `json:"at_unix_nano,omitempty"`
	ConfigVersion int64               `json:"config_version,omitempty"`
	Sub           *AcceptedSubmission `json:"submission,omitempty"`
	Report        *RoundReport        `json:"report,omitempty"`
	Reason        string              `json:"reason,omitempty"`
}

// Store is an append-only durable event log.
type Store interface {
	Append(ev Event) error
	Replay(apply func(Event) error) error
	Close() error
}

// FileStore durably appends one JSON object per line and fsyncs each event.
type FileStore struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

func OpenFileStore(path string) (*FileStore, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &FileStore{f: f, w: bufio.NewWriterSize(f, 4096)}, nil
}

func (s *FileStore) Append(ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	return s.f.Sync()
}

func (s *FileStore) Replay(apply func(Event) error) error {
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	sc := bufio.NewScanner(s.f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return err
		}
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
