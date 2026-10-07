package oracle_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/oracle"
)

var (
	windowStart = time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC)
	windowEnd   = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
)

// windowConfig puts src-a inside [windowStart, windowEnd); the other sources
// have no window and stay long-term valid.
func windowConfig(version int64) oracle.Config {
	c := testConfig(version)
	src := c.Sources["src-a"]
	nb, na := windowStart, windowEnd
	src.NotBefore = &nb
	src.NotAfter = &na
	c.Sources["src-a"] = src
	return c
}

func newWindowEngine(t *testing.T, clk *clock) *oracle.Engine {
	t.Helper()
	eng, err := oracle.NewEngine(oracle.NewMemoryStore(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.InstallConfig(windowConfig(1)); err != nil {
		t.Fatal(err)
	}
	return eng
}

func submitSrcA(eng *oracle.Engine, round uint64, at time.Time, nonce string) error {
	s := signedSub("key-a", "src-a", "BTCUSD", round, 100*oracle.MicrosPerUnit, at, nonce)
	_, err := eng.Submit(s)
	return err
}

// TestAuthWindowEndpoints covers both window endpoints: start inclusive,
// end exclusive.
func TestAuthWindowEndpoints(t *testing.T) {
	base := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"before_start_rejected", windowStart.Add(-time.Nanosecond), oracle.RejectAuthWindow},
		{"at_start_accepted", windowStart, ""},
		{"inside_accepted", windowStart.Add(time.Minute), ""},
		{"before_end_accepted", windowEnd.Add(-time.Nanosecond), ""},
		{"at_end_rejected", windowEnd, oracle.RejectAuthWindow},
		{"after_end_rejected", windowEnd.Add(time.Hour), oracle.RejectAuthWindow},
		{"far_before_rejected", base, oracle.RejectAuthWindow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := &clock{t: tc.at}
			eng := newWindowEngine(t, clk)
			r, _ := eng.OpenRound()
			err := submitSrcA(eng, r, tc.at, "n-"+tc.name)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("expected accept at %s, got %v", tc.at, err)
				}
				t.Logf("业务[授权时间窗] 用例[%s 被接受] 通过", tc.name)
				return
			}
			re, ok := err.(*oracle.RejectError)
			if !ok || re.Reason != tc.want {
				t.Fatalf("reason=%v want %s", err, tc.want)
			}
			if re.Reason == oracle.RejectUnauthorized || re.Reason == oracle.RejectExpired {
				t.Fatalf("window rejection must be distinguishable, got %s", re.Reason)
			}
			t.Logf("业务[授权时间窗] 用例[%s 拒绝原因=%s 可区分] 通过", tc.name, re.Reason)
		})
	}
}

// TestAuthWindowDistinctReasons: window rejection differs from unknown
// source and from stale quote.
func TestAuthWindowDistinctReasons(t *testing.T) {
	clk := &clock{t: windowEnd.Add(time.Hour)}
	eng := newWindowEngine(t, clk)
	r, _ := eng.OpenRound()

	err := submitSrcA(eng, r, clk.t, "d1")
	re := err.(*oracle.RejectError)
	if re.Reason != oracle.RejectAuthWindow {
		t.Fatalf("reason=%s want %s", re.Reason, oracle.RejectAuthWindow)
	}

	unknown := signedSub("key-x", "src-unknown", "BTCUSD", r, 100*oracle.MicrosPerUnit, clk.t, "d2")
	if got := mustReject(t, eng, unknown); got != oracle.RejectUnauthorized {
		t.Fatalf("unknown source reason=%s", got)
	}

	cfg := eng.ActiveConfig()
	stale := signedSub("key-b", "src-b", "BTCUSD", r, 100*oracle.MicrosPerUnit, clk.t.Add(-cfg.SubmissionTTL-time.Second), "d3")
	if got := mustReject(t, eng, stale); got != oracle.RejectExpired {
		t.Fatalf("stale quote reason=%s", got)
	}
	t.Log("业务[授权时间窗] 用例[窗口外/未授权/报价过期三种原因可区分] 通过")
}

func mustReject(t *testing.T, eng *oracle.Engine, s oracle.Submission) string {
	t.Helper()
	_, err := eng.Submit(s)
	if err == nil {
		t.Fatal("expected rejection")
	}
	return err.(*oracle.RejectError).Reason
}

// TestAuthWindowEdgeConcurrency: submissions racing exactly on the window
// edge are serialized under the engine mutex and always decided the same
// way: at NotBefore exactly one quote lands, at NotAfter all are rejected.
func TestAuthWindowEdgeConcurrency(t *testing.T) {
	const goroutines = 16
	for iter := 0; iter < 20; iter++ {
		clk := &clock{t: windowStart}
		eng := newWindowEngine(t, clk)
		r, _ := eng.OpenRound()
		var accepted, windowRejected int
		var wg sync.WaitGroup
		var mu sync.Mutex
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				err := submitSrcA(eng, r, windowStart, fmt.Sprintf("edge-%d-%d", iter, i))
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					accepted++
				} else if re := err.(*oracle.RejectError); re.Reason == oracle.RejectAuthWindow {
					windowRejected++
				}
			}(i)
		}
		wg.Wait()
		if accepted != 1 || windowRejected != 0 {
			t.Fatalf("iter %d at NotBefore: accepted=%d windowRejected=%d, want 1/0", iter, accepted, windowRejected)
		}

		clk2 := &clock{t: windowEnd}
		eng2 := newWindowEngine(t, clk2)
		r2, _ := eng2.OpenRound()
		accepted, windowRejected = 0, 0
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				err := submitSrcA(eng2, r2, windowEnd, fmt.Sprintf("edge2-%d-%d", iter, i))
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					accepted++
				} else if re := err.(*oracle.RejectError); re.Reason == oracle.RejectAuthWindow {
					windowRejected++
				}
			}(i)
		}
		wg.Wait()
		if accepted != 0 || windowRejected != goroutines {
			t.Fatalf("iter %d at NotAfter: accepted=%d windowRejected=%d, want 0/%d", iter, accepted, windowRejected, goroutines)
		}
	}
	t.Log("业务[授权时间窗] 用例[边缘并发20轮x16协程结果确定:起点恰收1次/终点全拒] 通过")
}

// TestAuthWindowBoundToConfigVersion: the window follows the config version
// and only constrains rounds opened after the upgrade.
func TestAuthWindowBoundToConfigVersion(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	eng, err := oracle.NewEngine(oracle.NewMemoryStore(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.InstallConfig(testConfig(1)); err != nil {
		t.Fatal(err)
	}
	r1, _ := eng.OpenRound()

	c2 := windowConfig(2)
	if err := eng.InstallConfig(c2); err != nil {
		t.Fatal(err)
	}

	if err := submitSrcA(eng, r1, clk.t, "v1-ok"); err != nil {
		t.Fatalf("round under v1 must not be affected by v2 window: %v", err)
	}
	if _, _, err := eng.Advance(); err != nil {
		t.Fatal(err)
	}
	r2, _ := eng.CurrentRound()
	s := signedSub("key-a", "src-a", "BTCUSD", r2, 100*oracle.MicrosPerUnit, clk.t, "v2-no")
	if got := mustReject(t, eng, s); got != oracle.RejectAuthWindow {
		t.Fatalf("round under v2 reason=%s want %s", got, oracle.RejectAuthWindow)
	}
	t.Log("业务[授权时间窗] 用例[窗口随配置版本升级仅约束新轮次] 通过")
}

// TestAuthWindowValidation rejects invalid windows and unknown policies.
func TestAuthWindowValidation(t *testing.T) {
	c := windowConfig(1)
	src := c.Sources["src-a"]
	src.NotBefore = &windowEnd
	src.NotAfter = &windowStart
	c.Sources["src-a"] = src
	if err := c.Validate(); err == nil {
		t.Fatal("not_before >= not_after must be rejected")
	}

	c2 := testConfig(1)
	rule := c2.Assets["BTCUSD"]
	rule.OnAnomaly = "explode"
	c2.Assets["BTCUSD"] = rule
	if err := c2.Validate(); err == nil {
		t.Fatal("unknown on_anomaly must be rejected")
	}
	t.Log("业务[授权时间窗] 用例[非法窗口与非法策略配置校验拒绝] 通过")
}

// TestNoWindowLongTermValid: a source without a window stays valid forever,
// exactly as before this change.
func TestNoWindowLongTermValid(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	eng := newTestEngine(t, clk, oracle.NewMemoryStore())
	r, _ := eng.OpenRound()
	clk.t = clk.t.Add(365 * 24 * time.Hour)
	s := signedSub("key-b", "src-b", "BTCUSD", r, 100*oracle.MicrosPerUnit, clk.t, "nw1")
	if _, err := eng.Submit(s); err != nil {
		t.Fatalf("source without window must stay valid: %v", err)
	}
	t.Log("业务[授权时间窗] 用例[无窗口来源长期有效兼容旧配置] 通过")
}
