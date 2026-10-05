// Package store 提供基于本地追加写事件日志的持久化。
//
// 只使用标准库：每个事件一行 JSON，写入后 fsync，模拟链上事件的
// 顺序性与不可篡改性。进程重启时从头重放日志即可还原全部已确认轮次；
// 已确认轮次只追加、从不重写，因此不会丢失，也不会被重复发布。
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const logFileName = "events.log"

// EventType 是日志事件类型。
type EventType string

const (
	EvtRoundOpened    EventType = "round_opened"
	EvtSubmission     EventType = "submission_accepted"
	EvtRoundRejected  EventType = "round_rejected"
	EvtRoundConfirmed EventType = "round_confirmed"
	EvtRoundVoided    EventType = "round_voided"
)

// Event 是日志中的一条记录。Payload 按类型携带不同结构（原始 JSON 延后解码）。
type Event struct {
	Seq       int64           `json:"seq"`
	Type      EventType       `json:"type"`
	RoundID   int64           `json:"round_id"`
	ConfigVer int64           `json:"config_version"`
	TimeNS    int64           `json:"time_ns"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Store 是已打开的事件日志。
type Store struct {
	dir string
	f   *os.File
	w   *bufio.Writer
	seq int64
}

// Open 打开（必要时创建）数据目录并重放日志。
// onEvent 对每条历史事件按序回调，调用方据此重建内存状态；
// 回调返回错误会中止打开，避免在损坏状态下继续运行。
func Open(dir string, onEvent func(Event) error) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dir, logFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	s := &Store{dir: dir, f: f}
	if err := s.replay(onEvent); err != nil {
		f.Close()
		return nil, err
	}
	s.w = bufio.NewWriter(f)
	return s, nil
}

func (s *Store) replay(onEvent func(Event) error) error {
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	sc := bufio.NewScanner(s.f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	var lastSeq int64
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("event log corrupted at line %d: %w", lineNo, err)
		}
		if e.Seq != lastSeq+1 {
			return fmt.Errorf("event log non-contiguous seq at line %d: want %d got %d", lineNo, lastSeq+1, e.Seq)
		}
		lastSeq = e.Seq
		if onEvent != nil {
			if err := onEvent(e); err != nil {
				return fmt.Errorf("replay event seq %d: %w", e.Seq, err)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read event log: %w", err)
	}
	s.seq = lastSeq
	return nil
}

// Append 持久化一条事件：编码、刷缓冲、fsync 成功后才算写入完成。
func (s *Store) Append(roundID, configVer, timeNS int64, typ EventType, payload any) (Event, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return Event{}, fmt.Errorf("encode event payload: %w", err)
		}
		raw = b
	}
	s.seq++
	e := Event{Seq: s.seq, Type: typ, RoundID: roundID, ConfigVer: configVer, TimeNS: timeNS, Payload: raw}
	line, err := json.Marshal(e)
	if err != nil {
		s.seq--
		return Event{}, err
	}
	if _, err := s.w.Write(append(line, '\n')); err != nil {
		s.seq--
		return Event{}, err
	}
	if err := s.w.Flush(); err != nil {
		s.seq--
		return Event{}, err
	}
	if err := s.f.Sync(); err != nil {
		s.seq--
		return Event{}, fmt.Errorf("fsync event log: %w", err)
	}
	return e, nil
}

// Close 关闭日志文件。
func (s *Store) Close() error {
	if s.w != nil {
		if err := s.w.Flush(); err != nil {
			return err
		}
	}
	return s.f.Close()
}

// Path 返回日志文件绝对路径，便于人工核验。
func (s *Store) Path() string { return filepath.Join(s.dir, logFileName) }

// ReadAll 以只读方式重放任意目录下的日志（离线审计/复现用）。
func ReadAll(dir string) ([]Event, error) {
	f, err := os.Open(filepath.Join(dir, logFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, sc.Err()
}
