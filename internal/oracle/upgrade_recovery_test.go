package oracle_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/oracle"
)

// TestConfigUpgradeOnlyAffectsNewRounds: 升级只对新轮次生效。
func TestConfigUpgradeOnlyAffectsNewRounds(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	eng := newTestEngine(t, clk, oracle.NewMemoryStore())
	cfg1 := eng.ActiveConfig()

	r1, _ := eng.OpenRound()
	submitAll(t, eng, healthyQuotes(cfg1, "BTCUSD", r1, clk.t, 100*oracle.MicrosPerUnit))
	rep1, _, err := eng.Advance()
	if err != nil {
		t.Fatal(err)
	}

	cfg2 := testConfig(2)
	rule := cfg2.Assets["BTCUSD"]
	rule.MinSources = 4
	cfg2.Assets["BTCUSD"] = rule
	if err := eng.InstallConfig(cfg2); err != nil {
		t.Fatal(err)
	}

	clk.t = clk.t.Add(30 * time.Second)
	// Upgrade lands while round 2 is open, so round 2 still binds v1. Close
	// it without submitting; it must not rewrite round 1.
	r2open, _ := eng.CurrentRound()
	rep2old, _, err := eng.Advance()
	if err != nil {
		t.Fatal(err)
	}
	if rep2old.ConfigVersion != 1 {
		t.Fatalf("open round should keep v1, got v%d", rep2old.ConfigVersion)
	}

	// The next round is the first one opened under v2.
	r2, _ := eng.CurrentRound()
	if r2 != r2open+1 {
		t.Fatalf("new round number=%d want %d", r2, r2open+1)
	}
	for _, id := range []string{"src-a", "src-b", "src-c"} {
		s := signedSub(cfg1.Sources[id].Key, id, "BTCUSD", r2, 100*oracle.MicrosPerUnit, clk.t, "up-"+id)
		if _, err := eng.Submit(s); err != nil {
			t.Fatal(err)
		}
	}
	rep2, _, err := eng.Advance()
	if err != nil {
		t.Fatal(err)
	}
	if rep2.ConfigVersion != 2 {
		t.Fatalf("round 2 should bind config v2, got v%d", rep2.ConfigVersion)
	}
	if rep2.Decisions["BTCUSD"].Status != oracle.RoundFallback {
		t.Fatalf("expected FALLBACK under v2 min=4, got %s", rep2.Decisions["BTCUSD"].Status)
	}
	if rep1.ConfigVersion != 1 {
		t.Fatalf("historical round config rewritten to v%d", rep1.ConfigVersion)
	}
	if got, ok := eng.Report(r1); !ok || got.Decisions["BTCUSD"].Price == nil {
		t.Fatal("historical round 1 result lost after upgrade")
	}
	if err := eng.InstallConfig(testConfig(1)); err == nil {
		t.Fatal("reinstalling an older config version must fail")
	}
	t.Log("业务[规则版本升级] 用例[v2仅约束新轮次且历史轮次不变] 通过")
}

func voidConfig(version int64) oracle.Config {
	c := testConfig(version)
	c.OnRestartOpenRound = "void"
	return c
}

// TestRestartRecoveryResume: resume 策略下未完成轮次一致恢复。
func TestRestartRecoveryResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}

	store1, err := oracle.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	eng1, err := oracle.NewEngine(store1, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng1.InstallConfig(testConfig(1)); err != nil {
		t.Fatal(err)
	}
	cfg := eng1.ActiveConfig()

	r1, _ := eng1.OpenRound()
	submitAll(t, eng1, healthyQuotes(cfg, "BTCUSD", r1, clk.t, 100*oracle.MicrosPerUnit))
	rep1, r2, err := eng1.Advance()
	if err != nil {
		t.Fatal(err)
	}
	wantPrice := *rep1.Decisions["BTCUSD"].Price

	for _, id := range []string{"src-a", "src-b"} {
		s := signedSub(cfg.Sources[id].Key, id, "BTCUSD", r2, 100*oracle.MicrosPerUnit, clk.t, "rc-"+id)
		if _, err := eng1.Submit(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := store1.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := oracle.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	eng2, err := oracle.NewEngine(store2, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	cur, ok := eng2.CurrentRound()
	if !ok || cur != r2 {
		t.Fatalf("open round should resume as %d, got %d/%v", r2, cur, ok)
	}
	got1, ok := eng2.Report(r1)
	if !ok || *got1.Decisions["BTCUSD"].Price != wantPrice {
		t.Fatal("confirmed round 1 lost or changed after restart")
	}
	dup := signedSub(cfg.Sources["src-a"].Key, "src-a", "BTCUSD", r2, 100*oracle.MicrosPerUnit, clk.t, "rc-src-a")
	rejectReason(t, eng2, dup, oracle.RejectDuplicate)

	for _, id := range []string{"src-c", "src-d"} {
		s := signedSub(cfg.Sources[id].Key, id, "BTCUSD", r2, 100*oracle.MicrosPerUnit, clk.t, "rc2-"+id)
		if _, err := eng2.Submit(s); err != nil {
			t.Fatal(err)
		}
	}
	rep2, err := eng2.CloseRound()
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Round != r2 {
		t.Fatalf("finalized round=%d want %d", rep2.Round, r2)
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}

	store3, err := oracle.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	eng3, err := oracle.NewEngine(store3, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(eng3.Reports()) != 2 {
		t.Fatalf("reports=%d want 2, no double publish on restart", len(eng3.Reports()))
	}
	if _, open := eng3.CurrentRound(); open {
		t.Fatal("no open round expected after clean close")
	}
	if err := store3.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("业务[崩溃恢复resume] 用例[确认轮次不丢不重、未完成轮次续算 price=%d] 通过", wantPrice)
}

// TestRestartRecoveryVoid: void 策略下崩溃时未完成轮次作废，不跳号。
func TestRestartRecoveryVoid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events-void.jsonl")
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}

	store1, err := oracle.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	eng1, err := oracle.NewEngine(store1, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng1.InstallConfig(voidConfig(1)); err != nil {
		t.Fatal(err)
	}
	cfg := eng1.ActiveConfig()

	r1, _ := eng1.OpenRound()
	submitAll(t, eng1, healthyQuotes(cfg, "BTCUSD", r1, clk.t, 100*oracle.MicrosPerUnit))
	_, r2, _ := eng1.Advance()
	s := signedSub(cfg.Sources["src-a"].Key, "src-a", "BTCUSD", r2, 100*oracle.MicrosPerUnit, clk.t, "v1")
	if _, err := eng1.Submit(s); err != nil {
		t.Fatal(err)
	}
	if err := store1.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := oracle.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	eng2, err := oracle.NewEngine(store2, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if !eng2.IsVoided(r2) {
		t.Fatalf("round %d should be voided on restart", r2)
	}
	if _, open := eng2.CurrentRound(); open {
		t.Fatal("voided round must not remain open")
	}
	if _, ok := eng2.Report(r1); !ok {
		t.Fatal("confirmed round 1 must survive")
	}

	r3, err := eng2.OpenRound()
	if err != nil {
		t.Fatal(err)
	}
	if r3 != r2+1 {
		t.Fatalf("next round=%d want %d (no skipped numbers)", r3, r2+1)
	}
	clk.t = clk.t.Add(time.Second)
	submitAll(t, eng2, healthyQuotes(cfg, "BTCUSD", r3, clk.t, 100*oracle.MicrosPerUnit))
	if _, err := eng2.CloseRound(); err != nil {
		t.Fatal(err)
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("业务[崩溃恢复void] 用例[未完成轮次作废且下一轮为%d不跳号] 通过", r3)
}

// TestDeterministicReplay: 同一条事件日志重放得到完全相同的报告。
func TestDeterministicReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events-det.jsonl")
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}

	store1, err := oracle.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	eng1, err := oracle.NewEngine(store1, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng1.InstallConfig(testConfig(1)); err != nil {
		t.Fatal(err)
	}
	cfg := eng1.ActiveConfig()
	for i := 0; i < 3; i++ {
		r, _ := eng1.OpenRound()
		submitAll(t, eng1, healthyQuotes(cfg, "BTCUSD", r, clk.t, 100*oracle.MicrosPerUnit))
		if _, err := eng1.CloseRound(); err != nil {
			t.Fatal(err)
		}
		clk.t = clk.t.Add(30 * time.Second)
	}
	first := eng1.Reports()
	if err := store1.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := oracle.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	eng2, err := oracle.NewEngine(store2, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	second := eng2.Reports()
	if len(first) != len(second) {
		t.Fatalf("report count %d vs %d", len(first), len(second))
	}
	for i := range first {
		if *first[i].Decisions["BTCUSD"].Price != *second[i].Decisions["BTCUSD"].Price {
			t.Fatalf("round %d price differs after replay", first[i].Round)
		}
		if first[i].Status != second[i].Status || first[i].ConfigVersion != second[i].ConfigVersion {
			t.Fatalf("round %d status/config differs after replay", first[i].Round)
		}
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}
	t.Log("业务[可复现] 用例[事件日志重放产出逐轮相同结果] 通过")
}
