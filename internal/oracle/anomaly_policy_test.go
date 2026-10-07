package oracle_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/oracle"
)

// policyConfig builds a single-asset config with the given anomaly policy.
func policyConfig(version int64, onAnomaly string) oracle.Config {
	c := testConfig(version)
	rule := c.Assets["BTCUSD"]
	rule.OnAnomaly = onAnomaly
	c.Assets = map[string]oracle.AssetRule{"BTCUSD": rule}
	return c
}

func newPolicyEngine(t *testing.T, clk *clock, onAnomaly string) *oracle.Engine {
	t.Helper()
	eng, err := oracle.NewEngine(oracle.NewMemoryStore(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.InstallConfig(policyConfig(1, onAnomaly)); err != nil {
		t.Fatal(err)
	}
	return eng
}

func publishHealthy(t *testing.T, eng *oracle.Engine, clk *clock) oracle.Price {
	t.Helper()
	r, _ := eng.OpenRound()
	submitAll(t, eng, healthyQuotes(eng.ActiveConfig(), "BTCUSD", r, clk.t, 100*oracle.MicrosPerUnit))
	rep, _, err := eng.Advance()
	if err != nil {
		t.Fatal(err)
	}
	d := rep.Decisions["BTCUSD"]
	if d.Status != oracle.RoundPublished {
		t.Fatalf("setup round status=%s", d.Status)
	}
	return *d.Price
}

// injectAnomaly makes the current open round anomalous in the requested way.
func injectAnomaly(t *testing.T, eng *oracle.Engine, clk *clock, kind string) {
	t.Helper()
	cfg := eng.ActiveConfig()
	r, _ := eng.CurrentRound()
	base := 100 * oracle.MicrosPerUnit
	switch kind {
	case "insufficient_sources":
		for _, id := range []string{"src-a", "src-b"} {
			s := signedSub(cfg.Sources[id].Key, id, "BTCUSD", r, base, clk.t, fmt.Sprintf("an-%d-%s", r, id))
			if _, err := eng.Submit(s); err != nil {
				t.Fatal(err)
			}
		}
	case "stale_quotes":
		submitAll(t, eng, healthyQuotes(cfg, "BTCUSD", r, clk.t, base))
		clk.t = clk.t.Add(cfg.MaxQuoteStaleness + time.Minute)
	case "deviation":
		for i, id := range sourceIDs() {
			p := base
			if i == 3 {
				p = base + 5*oracle.MicrosPerUnit
			}
			s := signedSub(cfg.Sources[id].Key, id, "BTCUSD", r, p, clk.t, fmt.Sprintf("an-%d-%s", r, id))
			if _, err := eng.Submit(s); err != nil {
				t.Fatal(err)
			}
		}
	default:
		t.Fatalf("unknown anomaly %q", kind)
	}
}

func hasCheck(d oracle.AssetDecision, prefix string) bool {
	for _, c := range d.Checks {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// TestAnomalyPolicyMatrix: every anomaly kind combined with both policies.
func TestAnomalyPolicyMatrix(t *testing.T) {
	anomalies := []struct {
		kind   string
		reason string
	}{
		{"insufficient_sources", "insufficient_sources"},
		{"stale_quotes", "insufficient_sources"},
		{"deviation", "deviation_exceeds_threshold"},
	}
	for _, a := range anomalies {
		for _, policy := range []string{oracle.AnomalyReject, oracle.AnomalyFallback} {
			name := a.kind + "/" + policy
			t.Run(name, func(t *testing.T) {
				clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
				eng := newPolicyEngine(t, clk, policy)
				good := publishHealthy(t, eng, clk)

				clk.t = clk.t.Add(30 * time.Second)
				injectAnomaly(t, eng, clk, a.kind)
				rep, err := eng.CloseRound()
				if err != nil {
					t.Fatal(err)
				}
				d := rep.Decisions["BTCUSD"]
				if d.RejectReason != a.reason {
					t.Fatalf("reject_reason=%s want %s", d.RejectReason, a.reason)
				}
				if d.AnomalyPolicy != policy {
					t.Fatalf("report anomaly_policy=%q want %q", d.AnomalyPolicy, policy)
				}
				if !hasCheck(d, "anomaly_policy="+policy) {
					t.Fatalf("checks must record the policy basis: %v", d.Checks)
				}
				switch policy {
				case oracle.AnomalyReject:
					if d.Status != oracle.RoundRejected || d.Price != nil {
						t.Fatalf("reject policy: status=%s price=%v", d.Status, d.Price)
					}
				case oracle.AnomalyFallback:
					if d.Status != oracle.RoundFallback || d.Price == nil || *d.Price != good {
						t.Fatalf("fallback policy: status=%s price=%v want %d", d.Status, d.Price, good)
					}
				}
				t.Logf("业务[按资产异常处置] 用例[%s 触发=%s 状态=%s] 通过", name, a.reason, d.Status)
			})
		}
	}
}

// TestFallbackMaxAgeBoundary: reuse is allowed at exactly fallback_staleness
// and refused one nanosecond later.
func TestFallbackMaxAgeBoundary(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	eng := newPolicyEngine(t, clk, oracle.AnomalyFallback)
	cfg := eng.ActiveConfig()
	good := publishHealthy(t, eng, clk)

	clk.t = clk.t.Add(cfg.FallbackStaleness)
	injectAnomaly(t, eng, clk, "insufficient_sources")
	rep, _, err := eng.Advance()
	if err != nil {
		t.Fatal(err)
	}
	if d := rep.Decisions["BTCUSD"]; d.Status != oracle.RoundFallback || *d.Price != good {
		t.Fatalf("at exactly fallback_staleness: status=%s want FALLBACK", d.Status)
	}
	t.Log("业务[按资产异常处置] 用例[沿用年龄恰等于上限仍允许] 通过")

	clk.t = clk.t.Add(time.Nanosecond)
	injectAnomaly(t, eng, clk, "insufficient_sources")
	rep2, err := eng.CloseRound()
	if err != nil {
		t.Fatal(err)
	}
	if d := rep2.Decisions["BTCUSD"]; d.Status != oracle.RoundRejected || d.Price != nil {
		t.Fatalf("1ns past fallback_staleness: status=%s want REJECTED", d.Status)
	}
	t.Log("业务[按资产异常处置] 用例[沿用年龄超上限1ns即拒绝] 通过")
}

// TestFallbackDoesNotExtendLife: FALLBACK never refreshes the last confirmed
// result, so prices cannot be kept alive by reuse alone.
func TestFallbackDoesNotExtendLife(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	eng := newPolicyEngine(t, clk, oracle.AnomalyFallback)
	cfg := eng.ActiveConfig()
	publishHealthy(t, eng, clk)

	clk.t = clk.t.Add(6 * time.Minute)
	injectAnomaly(t, eng, clk, "insufficient_sources")
	rep, _, err := eng.Advance()
	if err != nil {
		t.Fatal(err)
	}
	if d := rep.Decisions["BTCUSD"]; d.Status != oracle.RoundFallback {
		t.Fatalf("first fallback: status=%s", d.Status)
	}

	// 12 minutes after the last PUBLISH (6 after the fallback). If the
	// fallback had refreshed freshness this would be allowed; it must not.
	clk.t = clk.t.Add(6 * time.Minute)
	if clk.t.Sub(time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)) <= cfg.FallbackStaleness {
		t.Fatal("test setup: age should exceed staleness")
	}
	injectAnomaly(t, eng, clk, "insufficient_sources")
	rep2, err := eng.CloseRound()
	if err != nil {
		t.Fatal(err)
	}
	if d := rep2.Decisions["BTCUSD"]; d.Status != oracle.RoundRejected {
		t.Fatalf("fallback must not extend life: status=%s want REJECTED", d.Status)
	}
	t.Log("业务[按资产异常处置] 用例[沿用不刷新年龄无法续命] 通过")
}

// TestAnomalyPolicyBoundToConfigVersion: the policy evolves with the config
// version and only constrains new rounds.
func TestAnomalyPolicyBoundToConfigVersion(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	eng := newPolicyEngine(t, clk, oracle.AnomalyReject)
	publishHealthy(t, eng, clk)

	clk.t = clk.t.Add(30 * time.Second)
	injectAnomaly(t, eng, clk, "insufficient_sources")
	if err := eng.InstallConfig(policyConfig(2, oracle.AnomalyFallback)); err != nil {
		t.Fatal(err)
	}
	rep, _, err := eng.Advance()
	if err != nil {
		t.Fatal(err)
	}
	if d := rep.Decisions["BTCUSD"]; d.Status != oracle.RoundRejected {
		t.Fatalf("round opened under v1 must keep reject policy, got %s", d.Status)
	}

	clk.t = clk.t.Add(30 * time.Second)
	injectAnomaly(t, eng, clk, "insufficient_sources")
	rep2, err := eng.CloseRound()
	if err != nil {
		t.Fatal(err)
	}
	if d := rep2.Decisions["BTCUSD"]; d.Status != oracle.RoundFallback {
		t.Fatalf("round opened under v2 must use fallback policy, got %s", d.Status)
	}
	got, _ := eng.Report(rep.Round)
	if got.Decisions["BTCUSD"].AnomalyPolicy != oracle.AnomalyReject {
		t.Fatal("historical round policy rewritten after upgrade")
	}
	t.Log("业务[按资产异常处置] 用例[策略随版本演进历史轮次判定依据不变] 通过")
}

// TestMinPositivePriceEffective: min_positive_price is enforced at
// aggregation; quotes below it are dropped with a recorded reason.
func TestMinPositivePriceEffective(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	eng, err := oracle.NewEngine(oracle.NewMemoryStore(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	c := policyConfig(1, oracle.AnomalyReject)
	rule := c.Assets["BTCUSD"]
	rule.MinPositivePrice = 50 * oracle.MicrosPerUnit
	c.Assets["BTCUSD"] = rule
	if err := eng.InstallConfig(c); err != nil {
		t.Fatal(err)
	}
	r, _ := eng.OpenRound()
	cfg := eng.ActiveConfig()
	s := signedSub(cfg.Sources["src-a"].Key, "src-a", "BTCUSD", r, 1, clk.t, "mp-a")
	if _, err := eng.Submit(s); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"src-b", "src-c", "src-d"} {
		s := signedSub(cfg.Sources[id].Key, id, "BTCUSD", r, 100*oracle.MicrosPerUnit, clk.t, "mp-"+id)
		if _, err := eng.Submit(s); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := eng.CloseRound()
	if err != nil {
		t.Fatal(err)
	}
	d := rep.Decisions["BTCUSD"]
	if !hasCheck(d, "dropped_below_min_positive_price source=src-a") {
		t.Fatalf("expected min_positive_price drop check, got %v", d.Checks)
	}
	if len(d.AcceptedQuotes) != 3 {
		t.Fatalf("accepted=%d want 3 after dropping below-min quote", len(d.AcceptedQuotes))
	}
	t.Log("业务[配置项生效] 用例[min_positive_price在聚合阶段剔除低价报价] 通过")
}
