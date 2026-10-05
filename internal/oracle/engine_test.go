package oracle_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/oracle"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestEngine(t *testing.T, clk *clock, store oracle.Store) *oracle.Engine {
	t.Helper()
	eng, err := oracle.NewEngine(store, clk.now)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if err := eng.InstallConfig(testConfig(1)); err != nil {
		t.Fatalf("install config: %v", err)
	}
	return eng
}

func testConfig(version int64) oracle.Config {
	sources := map[string]oracle.SourceConfig{
		"src-a": {ID: "src-a", Name: "A", Key: "key-a", Active: true},
		"src-b": {ID: "src-b", Name: "B", Key: "key-b", Active: true},
		"src-c": {ID: "src-c", Name: "C", Key: "key-c", Active: true},
		"src-d": {ID: "src-d", Name: "D", Key: "key-d", Active: true},
	}
	rule := oracle.DefaultAssetRule()
	assets := map[string]oracle.AssetRule{"BTCUSD": rule, "ETHUSD": rule}
	return oracle.DefaultConfig(version, sources, assets)
}

func sourceIDs() []string { return []string{"src-a", "src-b", "src-c", "src-d"} }

func signedSub(key, source, asset string, round uint64, p oracle.Price, at time.Time, nonce string) oracle.Submission {
	return oracle.Submission{
		SourceID:  source,
		Asset:     asset,
		Round:     round,
		Price:     p,
		QuoteTime: at,
		Nonce:     nonce,
		Sig:       oracle.Sign(key, source, asset, round, p, at, nonce),
	}
}

func healthyQuotes(cfg oracle.Config, asset string, round uint64, at time.Time, base oracle.Price) []oracle.Submission {
	subs := []oracle.Submission{}
	offsets := map[string]oracle.Price{"src-a": 0, "src-b": 10_000, "src-c": -10_000, "src-d": 5_000}
	for _, id := range sourceIDs() {
		subs = append(subs, signedSub(cfg.Sources[id].Key, id, asset, round, base+offsets[id], at, fmt.Sprintf("n-%d-%s-%s", round, asset, id)))
	}
	return subs
}

func submitAll(t *testing.T, eng *oracle.Engine, subs []oracle.Submission) {
	t.Helper()
	for _, s := range subs {
		if _, err := eng.Submit(s); err != nil {
			t.Fatalf("accept %s %s: %v", s.SourceID, s.Asset, err)
		}
	}
}

func rejectReason(t *testing.T, eng *oracle.Engine, s oracle.Submission, want string) {
	t.Helper()
	_, err := eng.Submit(s)
	if err == nil {
		t.Fatalf("expected rejection %s, got accept", want)
	}
	re, ok := err.(*oracle.RejectError)
	if !ok {
		t.Fatalf("expected RejectError, got %T: %v", err, err)
	}
	if re.Reason != want {
		t.Fatalf("reason=%s, want %s (%s)", re.Reason, want, re.Detail)
	}
}

// TestHealthyAggregation: 正常业务路径，四来源去极值求均值。
func TestHealthyAggregation(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	eng := newTestEngine(t, clk, oracle.NewMemoryStore())
	r, err := eng.OpenRound()
	if err != nil {
		t.Fatal(err)
	}
	submitAll(t, eng, healthyQuotes(eng.ActiveConfig(), "BTCUSD", r, clk.t, 100*oracle.MicrosPerUnit))
	report, err := eng.CloseRound()
	if err != nil {
		t.Fatal(err)
	}
	d := report.Decisions["BTCUSD"]
	if d.Status != oracle.RoundPublished {
		t.Fatalf("status=%s want PUBLISHED (%s)", d.Status, d.RejectReason)
	}
	// 99.99, 100.00, 100.005, 100.01 去掉最低/最高后取中两项整数均值
	if d.Price == nil || *d.Price != 100_002_500 {
		t.Fatalf("price=%v want 100002500", d.Price)
	}
	t.Logf("业务[正常聚合] 用例[4来源去极值求均值] 通过: price=%d", *d.Price)
}

// TestRejectionReasons 覆盖各类可区分的拒绝原因。
func TestRejectionReasons(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	eng := newTestEngine(t, clk, oracle.NewMemoryStore())
	r, _ := eng.OpenRound()
	cfg := eng.ActiveConfig()
	at := clk.t

	t.Run("unauthorized_source", func(t *testing.T) {
		s := signedSub("key-a", "src-unknown", "BTCUSD", r, 100*oracle.MicrosPerUnit, at, "u1")
		rejectReason(t, eng, s, oracle.RejectUnauthorized)
		t.Log("业务[提交鉴权] 用例[未授权来源被拒绝] 通过")
	})

	t.Run("inactive_source_config_gate", func(t *testing.T) {
		eng2 := newTestEngine(t, clk, oracle.NewMemoryStore())
		c2 := testConfig(2)
		src := c2.Sources["src-d"]
		src.Active = false
		c2.Sources["src-d"] = src
		if _, active := c2.Source("src-d"); active {
			t.Fatal("src-d should be inactive")
		}
		if err := eng.InstallConfig(c2); err != nil {
			t.Fatalf("upgrade install: %v", err)
		}
		if err := eng2.InstallConfig(c2); err != nil {
			t.Fatal(err)
		}
		r2, _ := eng2.OpenRound()
		s := signedSub("key-d", "src-d", "BTCUSD", r2, 100*oracle.MicrosPerUnit, at, "u1b")
		rejectReason(t, eng2, s, oracle.RejectUnauthorized)
		t.Log("业务[提交鉴权] 用例[已停用来源被拒绝] 通过")
	})

	t.Run("expired_quote", func(t *testing.T) {
		old := clk.t.Add(-cfg.SubmissionTTL - time.Second)
		s := signedSub("key-a", "src-a", "BTCUSD", r, 100*oracle.MicrosPerUnit, old, "u2")
		rejectReason(t, eng, s, oracle.RejectExpired)
		t.Log("业务[提交鉴权] 用例[超过有效期的报价被拒绝] 通过")
	})

	t.Run("future_quote", func(t *testing.T) {
		future := clk.t.Add(cfg.ClockSkew + time.Second)
		s := signedSub("key-a", "src-a", "BTCUSD", r, 100*oracle.MicrosPerUnit, future, "u2b")
		rejectReason(t, eng, s, oracle.RejectExpired)
		t.Log("业务[提交鉴权] 用例[时间戳超前过多被拒绝] 通过")
	})

	t.Run("bad_signature", func(t *testing.T) {
		s := signedSub("wrong-key", "src-a", "BTCUSD", r, 100*oracle.MicrosPerUnit, at, "u3")
		rejectReason(t, eng, s, oracle.RejectBadSignature)
		t.Log("业务[提交鉴权] 用例[签名错误被拒绝] 通过")
	})

	t.Run("bad_format", func(t *testing.T) {
		s := signedSub("key-a", "src-a", "BTCUSD", r, -5, at, "u4")
		rejectReason(t, eng, s, oracle.RejectBadFormat)
		t.Log("业务[提交鉴权] 用例[非法价格被拒绝] 通过")
	})

	t.Run("unknown_asset", func(t *testing.T) {
		s := signedSub("key-a", "src-a", "DOGEUSD", r, 100*oracle.MicrosPerUnit, at, "u5")
		rejectReason(t, eng, s, oracle.RejectAssetUnknown)
		t.Log("业务[提交鉴权] 用例[未配置资产被拒绝] 通过")
	})

	t.Run("wrong_round", func(t *testing.T) {
		s := signedSub("key-a", "src-a", "BTCUSD", r+99, 100*oracle.MicrosPerUnit, at, "u6")
		rejectReason(t, eng, s, oracle.RejectRoundClosed)
		t.Log("业务[轮次管理] 用例[非当前轮次报价被拒绝] 通过")
	})

	t.Run("duplicate_same_source", func(t *testing.T) {
		s1 := signedSub("key-a", "src-a", "BTCUSD", r, 100*oracle.MicrosPerUnit, at, "u7")
		if _, err := eng.Submit(s1); err != nil {
			t.Fatal(err)
		}
		s2 := signedSub("key-a", "src-a", "BTCUSD", r, 101*oracle.MicrosPerUnit, at, "u8")
		rejectReason(t, eng, s2, oracle.RejectDuplicate)
		t.Log("业务[提交鉴权] 用例[同一来源同轮重复报价被拒绝] 通过")
	})

	t.Run("duplicate_nonce", func(t *testing.T) {
		s1 := signedSub("key-b", "src-b", "BTCUSD", r, 100*oracle.MicrosPerUnit, at, "shared-nonce")
		if _, err := eng.Submit(s1); err != nil {
			t.Fatal(err)
		}
		s2 := signedSub("key-c", "src-c", "BTCUSD", r, 100*oracle.MicrosPerUnit, at, "shared-nonce")
		rejectReason(t, eng, s2, oracle.RejectDuplicate)
		t.Log("业务[提交鉴权] 用例[nonce重放被拒绝] 通过")
	})
}

// TestConcurrentSubmissions 并发提交必须每个来源恰好计入一次。
func TestConcurrentSubmissions(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	eng := newTestEngine(t, clk, oracle.NewMemoryStore())
	r, _ := eng.OpenRound()
	cfg := eng.ActiveConfig()

	var wg sync.WaitGroup
	for attempt := 0; attempt < 5; attempt++ {
		for _, id := range sourceIDs() {
			wg.Add(1)
			go func(id string, attempt int) {
				defer wg.Done()
				nonce := fmt.Sprintf("c-%s-%d", id, attempt)
				s := signedSub(cfg.Sources[id].Key, id, "BTCUSD", r, 100*oracle.MicrosPerUnit, clk.t, nonce)
				_, _ = eng.Submit(s)
			}(id, attempt)
		}
	}
	wg.Wait()

	report, err := eng.CloseRound()
	if err != nil {
		t.Fatal(err)
	}
	d := report.Decisions["BTCUSD"]
	if len(d.AcceptedQuotes) != 4 {
		t.Fatalf("accepted=%d want exactly 4", len(d.AcceptedQuotes))
	}
	seen := map[string]bool{}
	for _, q := range d.AcceptedQuotes {
		if seen[q.SourceID] {
			t.Fatalf("source %s counted twice", q.SourceID)
		}
		seen[q.SourceID] = true
	}

	// Every concurrent quote was exactly 100.0, so the only possible result
	// is 100.0 regardless of which attempt's quote won per source.
	if *d.Price != 100*oracle.MicrosPerUnit {
		t.Fatalf("price=%d want deterministic 100000000", *d.Price)
	}
	t.Logf("业务[并发确定性] 用例[20并发仅4来源各计1次且结果可复现 price=%d] 通过", *d.Price)
}
