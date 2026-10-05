package oracle_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/oracle"
)

// TestInsufficientSourcesAndFallback: 来源不足边界与回退语义。
func TestInsufficientSourcesAndFallback(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	eng := newTestEngine(t, clk, oracle.NewMemoryStore())
	cfg := eng.ActiveConfig()

	r1, _ := eng.OpenRound()
	submitAll(t, eng, healthyQuotes(cfg, "BTCUSD", r1, clk.t, 100*oracle.MicrosPerUnit))
	rep1, _, err := eng.Advance()
	if err != nil {
		t.Fatal(err)
	}
	good := *rep1.Decisions["BTCUSD"].Price

	clk.t = clk.t.Add(30 * time.Second)
	r2, _ := eng.CurrentRound()
	for _, id := range []string{"src-a", "src-b"} {
		s := signedSub(cfg.Sources[id].Key, id, "BTCUSD", r2, 100*oracle.MicrosPerUnit, clk.t, fmt.Sprintf("f2-%s", id))
		if _, err := eng.Submit(s); err != nil {
			t.Fatal(err)
		}
	}
	rep2, _, err := eng.Advance()
	if err != nil {
		t.Fatal(err)
	}
	d2 := rep2.Decisions["BTCUSD"]
	if d2.Status != oracle.RoundFallback || d2.Price == nil || *d2.Price != good {
		t.Fatalf("expected fresh fallback to %d, got %s %v", good, d2.Status, d2.Price)
	}
	if d2.BasedOnRound == nil || *d2.BasedOnRound != r1 {
		t.Fatalf("fallback should reference round %d", r1)
	}
	t.Logf("业务[来源不足降级] 用例[2来源回退最近有效结果=%d] 通过", *d2.Price)

	clk.t = clk.t.Add(cfg.FallbackStaleness + time.Second)
	r3, _ := eng.CurrentRound()
	for _, id := range []string{"src-a", "src-b"} {
		s := signedSub(cfg.Sources[id].Key, id, "BTCUSD", r3, 100*oracle.MicrosPerUnit, clk.t, fmt.Sprintf("f3-%s", id))
		if _, err := eng.Submit(s); err != nil {
			t.Fatal(err)
		}
	}
	rep3, _, err := eng.Advance()
	if err != nil {
		t.Fatal(err)
	}
	d3 := rep3.Decisions["BTCUSD"]
	if d3.Status != oracle.RoundRejected || d3.Price != nil {
		t.Fatalf("expected REJECTED with no price, got %s %v", d3.Status, d3.Price)
	}
	t.Log("业务[来源不足降级] 用例[回退超期则拒绝发布] 通过")
}

// TestDeviationThreshold: 偏差超过阈值不得静默产出。
func TestDeviationThreshold(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	eng := newTestEngine(t, clk, oracle.NewMemoryStore())
	cfg := eng.ActiveConfig()
	r, _ := eng.OpenRound()
	base := 100 * oracle.MicrosPerUnit
	prices := map[string]oracle.Price{
		"src-a": base,
		"src-b": base,
		"src-c": base,
		"src-d": base + 5*oracle.MicrosPerUnit,
	}
	for _, id := range sourceIDs() {
		s := signedSub(cfg.Sources[id].Key, id, "BTCUSD", r, prices[id], clk.t, "dev-"+id)
		if _, err := eng.Submit(s); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := eng.CloseRound()
	if err != nil {
		t.Fatal(err)
	}
	d := rep.Decisions["BTCUSD"]
	if d.Status != oracle.RoundRejected {
		t.Fatalf("status=%s want REJECTED", d.Status)
	}
	if d.RejectReason != "deviation_exceeds_threshold" {
		t.Fatalf("reason=%s", d.RejectReason)
	}
	if d.DeviationBp == nil || *d.DeviationBp <= int64(cfg.Assets["BTCUSD"].MaxDeviationBasisPoints) {
		t.Fatalf("devBp=%v", d.DeviationBp)
	}
	if d.Price != nil {
		t.Fatal("rejected round must not publish a price")
	}
	t.Logf("业务[偏差降级边界] 用例[偏差=%dbp超阈值拒绝发布] 通过", *d.DeviationBp)
}

// TestStaleQuotesDropped: 轮次长期不结算时报价整体陈旧，必须拒绝。
func TestStaleQuotesDropped(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	eng := newTestEngine(t, clk, oracle.NewMemoryStore())
	cfg := eng.ActiveConfig()
	r, _ := eng.OpenRound()
	base := 100 * oracle.MicrosPerUnit
	for _, id := range sourceIDs() {
		s := signedSub(cfg.Sources[id].Key, id, "BTCUSD", r, base, clk.t, "stale-"+id)
		if _, err := eng.Submit(s); err != nil {
			t.Fatal(err)
		}
	}
	clk.t = clk.t.Add(cfg.MaxQuoteStaleness + time.Minute)
	rep, err := eng.CloseRound()
	if err != nil {
		t.Fatal(err)
	}
	d := rep.Decisions["BTCUSD"]
	if d.Status != oracle.RoundRejected {
		t.Fatalf("status=%s want REJECTED after all quotes went stale", d.Status)
	}
	t.Log("业务[陈旧报价] 用例[结算时报价超期剔除并拒绝] 通过")
}

// TestHardBandAntiCollusion: 全体来源串谋也无法越过硬区间。
func TestHardBandAntiCollusion(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	eng := newTestEngine(t, clk, oracle.NewMemoryStore())
	cfg := eng.ActiveConfig()

	r1, _ := eng.OpenRound()
	submitAll(t, eng, healthyQuotes(cfg, "BTCUSD", r1, clk.t, 100*oracle.MicrosPerUnit))
	rep1, _, _ := eng.Advance()
	last := *rep1.Decisions["BTCUSD"].Price

	clk.t = clk.t.Add(30 * time.Second)
	r2, _ := eng.CurrentRound()
	for _, id := range sourceIDs() {
		p := last + last*8/100 // 全体 +8%
		s := signedSub(cfg.Sources[id].Key, id, "BTCUSD", r2, p, clk.t, "band-"+id)
		if _, err := eng.Submit(s); err != nil {
			t.Fatal(err)
		}
	}
	rep2, _, err := eng.Advance()
	if err != nil {
		t.Fatal(err)
	}
	d := rep2.Decisions["BTCUSD"]
	wantCeil := last + last*500/10000
	if !d.Clamped || d.Price == nil || *d.Price != wantCeil {
		t.Fatalf("clamped=%v price=%v want %d", d.Clamped, d.Price, wantCeil)
	}
	t.Logf("业务[反串通硬护栏] 用例[+8%%报价被钳制到上沿=%d] 通过", *d.Price)
}
