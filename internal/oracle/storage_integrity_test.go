package oracle_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/oracle"
)

// writeSomeRounds drives an engine through two finalized rounds on a file
// store and closes it, returning the log path.
func writeSomeRounds(t *testing.T) (string, oracle.Price) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	store, err := oracle.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	eng := newTestEngine(t, clk, store)
	cfg := eng.ActiveConfig()
	var last oracle.Price
	for i := 0; i < 2; i++ {
		r, _ := eng.OpenRound()
		submitAll(t, eng, healthyQuotes(cfg, "BTCUSD", r, clk.t, 100*oracle.MicrosPerUnit))
		rep, err := eng.CloseRound()
		if err != nil {
			t.Fatal(err)
		}
		last = *rep.Decisions["BTCUSD"].Price
		clk.t = clk.t.Add(30 * time.Second)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path, last
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	return lines
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func expectIntegrityError(t *testing.T, path, wantSub string) {
	t.Helper()
	_, err := oracle.OpenFileStore(path)
	if err == nil {
		t.Fatalf("expected integrity error containing %q, got nil", wantSub)
	}
	var lie *oracle.LogIntegrityError
	if !errors.As(err, &lie) {
		t.Fatalf("expected LogIntegrityError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), wantSub) {
		t.Fatalf("error %q does not mention %q", err, wantSub)
	}
}

// TestLogSeqGapDetected: a missing record number fails the open loudly.
func TestLogSeqGapDetected(t *testing.T) {
	path, _ := writeSomeRounds(t)
	lines := readLines(t, path)
	if len(lines) < 4 {
		t.Fatalf("need at least 4 events, got %d", len(lines))
	}
	// Drop the second record: seq jumps 1 -> 3.
	writeLines(t, path, append(lines[:1], lines[2:]...))
	expectIntegrityError(t, path, "out of order")
	t.Log("业务[日志完整性] 用例[缺号被明确报错并指出位置] 通过")
}

// TestLogOutOfOrderDetected: swapped records fail the open loudly.
func TestLogOutOfOrderDetected(t *testing.T) {
	path, _ := writeSomeRounds(t)
	lines := readLines(t, path)
	lines[1], lines[2] = lines[2], lines[1]
	writeLines(t, path, lines)
	expectIntegrityError(t, path, "out of order")
	t.Log("业务[日志完整性] 用例[乱序记录被明确报错] 通过")
}

// TestLogCorruptRecordDetected: an unparseable newline-terminated record is
// corruption, not a torn tail.
func TestLogCorruptRecordDetected(t *testing.T) {
	path, _ := writeSomeRounds(t)
	lines := readLines(t, path)
	lines[2] = "{not-json"
	writeLines(t, path, lines)
	expectIntegrityError(t, path, "corrupt record")
	t.Log("业务[日志完整性] 用例[损坏记录被明确报错] 通过")
}

// TestLogTornTailTruncated: a partial final line (killed mid-write, never
// acknowledged) is truncated and the log keeps working.
func TestLogTornTailTruncated(t *testing.T) {
	path, want := writeSomeRounds(t)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":999,"type":"round_fin`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	store, err := oracle.OpenFileStore(path)
	if err != nil {
		t.Fatalf("torn tail must be safely ignored, got %v", err)
	}
	if n := store.TruncatedTailBytes(); n <= 0 {
		t.Fatal("expected truncated tail bytes to be reported")
	}
	clk := &clock{t: time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC)}
	eng, err := oracle.NewEngine(store, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _, ok := eng.LatestPrice("BTCUSD"); !ok || got != want {
		t.Fatalf("confirmed price lost after torn tail: %d/%v", got, ok)
	}
	// The log still accepts new rounds after truncation.
	r, err := eng.OpenRound()
	if err != nil {
		t.Fatal(err)
	}
	submitAll(t, eng, healthyQuotes(eng.ActiveConfig(), "BTCUSD", r, clk.t, 100*oracle.MicrosPerUnit))
	if _, err := eng.CloseRound(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// And the whole log still verifies.
	store2, err := oracle.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := oracle.Verify(store2)
	if err != nil || !res.OK() {
		t.Fatalf("verify after torn tail: %v %+v", err, res)
	}
	store2.Close()
	t.Log("业务[日志完整性] 用例[半条尾记录安全截断且日志可继续追加] 通过")
}

// flakyStore fails appends of chosen event types.
type flakyStore struct {
	inner  *oracle.MemoryStore
	failOn map[string]bool
}

func (f *flakyStore) Append(ev oracle.Event) error {
	if f.failOn[ev.Type] {
		return errors.New("injected write failure")
	}
	return f.inner.Append(ev)
}

func (f *flakyStore) Replay(apply func(oracle.Event) error) error { return f.inner.Replay(apply) }
func (f *flakyStore) Close() error                                { return nil }

// TestFinalizeWriteFailureAtomic: when the finalize append fails, the round
// stays undecided — in memory and on disk — and can be finalized later
// without loss or duplication.
func TestFinalizeWriteFailureAtomic(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	store := &flakyStore{inner: oracle.NewMemoryStore(), failOn: map[string]bool{}}
	eng := newTestEngine(t, clk, store)
	cfg := eng.ActiveConfig()

	r1, _ := eng.OpenRound()
	submitAll(t, eng, healthyQuotes(cfg, "BTCUSD", r1, clk.t, 100*oracle.MicrosPerUnit))
	rep1, r2, err := eng.Advance()
	if err != nil {
		t.Fatal(err)
	}
	good := *rep1.Decisions["BTCUSD"].Price

	clk.t = clk.t.Add(30 * time.Second)
	for _, id := range []string{"src-a", "src-b"} {
		s := signedSub(cfg.Sources[id].Key, id, "BTCUSD", r2, 100*oracle.MicrosPerUnit, clk.t, "af-"+id)
		if _, err := eng.Submit(s); err != nil {
			t.Fatal(err)
		}
	}

	store.failOn[oracle.EvRoundFinalized] = true
	if _, err := eng.CloseRound(); err == nil {
		t.Fatal("expected injected finalize failure")
	}

	// Memory must match disk: round still open, last confirmed unchanged.
	if cur, open := eng.CurrentRound(); !open || cur != r2 {
		t.Fatalf("round must stay open after failed finalize, got %d/%v", cur, open)
	}
	if p, lr, _, ok := eng.LatestPrice("BTCUSD"); !ok || p != good || lr != r1 {
		t.Fatalf("last confirmed mutated by failed finalize: %d round=%d", p, lr)
	}
	if len(eng.Reports()) != 1 {
		t.Fatalf("failed finalize must not record a report, got %d", len(eng.Reports()))
	}

	// Recovery from the same log agrees: round 2 unfinished, round 1 intact.
	eng2, err := oracle.NewEngine(store.inner, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(eng2.Reports()) != 1 {
		t.Fatalf("recovered reports=%d want 1", len(eng2.Reports()))
	}
	if cur, open := eng2.CurrentRound(); !open || cur != r2 {
		t.Fatalf("recovered round must resume as open %d, got %d/%v", r2, cur, open)
	}

	// Once the failure clears, the same round finalizes exactly once.
	store.failOn[oracle.EvRoundFinalized] = false
	rep2, err := eng.CloseRound()
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Round != r2 {
		t.Fatalf("finalized round=%d want %d", rep2.Round, r2)
	}
	finalized := 0
	_ = store.inner.Replay(func(ev oracle.Event) error {
		if ev.Type == oracle.EvRoundFinalized {
			finalized++
		}
		return nil
	})
	if finalized != 2 {
		t.Fatalf("finalize events=%d want 2 (no duplicate)", finalized)
	}
	t.Log("业务[定稿原子性] 用例[写入失败保持未定稿恢复后不丢不重] 通过")
}
