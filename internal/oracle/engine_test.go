package oracle

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/auth"
	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/config"
)

const sec = int64(1_000_000_000)

func cfgV1() *config.Config {
	return &config.Config{
		Version: 1, Pair: "BTC/USD",
		Sources: map[string]config.Source{
			"src-a": {ID: "src-a", Secret: "sa", Weight: 1},
			"src-b": {ID: "src-b", Secret: "sb", Weight: 1},
			"src-c": {ID: "src-c", Secret: "sc", Weight: 1},
		},
		Rule: config.AggregationRule{
			MinSources: 2, MaxStalenessNS: 30 * sec, MaxDeviationBPS: 200,
			InsufficientPolicy: config.PolicyFallback, DeviationPolicy: config.PolicyReject,
			HardMinPrice: 90, HardMaxPrice: 110,
		},
	}
}

func cfgV2() *config.Config {
	c := cfgV1()
	c.Version = 2
	c.Sources["src-d"] = config.Source{ID: "src-d", Secret: "sd", Weight: 1}
	c.Rule.MinSources = 3
	c.Rule.MaxDeviationBPS = 100
	return c
}

func newTestEngine(t *testing.T, recovery RecoveryPolicy, cfgs ...*config.Config) *Engine {
	t.Helper()
	h, err := config.NewHistory(cfgs...)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(Options{DataDir: t.TempDir(), History: h, Recovery: recovery, FutureToleranceNS: sec})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	return eng
}

func signSub(c *config.Config, round int64, id string, price float64, ts int64, nonce string) auth.Submission {
	s := auth.Submission{SourceID: id, RoundID: round, Price: price, Timestamp: ts, Nonce: nonce}
	s.Sig = auth.Sign(c.Sources[id].Secret, s)
	return s
}

func TestConcurrentSubmissionsNoDoubleCount(t *testing.T) {
	// 业务：同轮并发提交的确定性
	t.Log("业务：同轮并发提交 —— 多 goroutine 对同一轮提交与重放，不重复计入、不漏计")
	c := cfgV1()
	eng := newTestEngine(t, RecoveryVoid, c)
	round := eng.CurrentRoundID()

	var wg sync.WaitGroup
	var mu sync.Mutex
	errs := make([]error, 0)

	// 3 个来源各提交 1 次合法报价 + 每个来源并发重放 4 次
	for _, id := range []string{"src-a", "src-b", "src-c"} {
		for k := 0; k < 5; k++ {
			wg.Add(1)
			go func(id string, k int) {
				defer wg.Done()
				err := eng.Submit(signSub(c, round, id, 100, 100*sec, fmt.Sprintf("%s-%d", id, k)), 100*sec)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}(id, k)
		}
	}
	wg.Wait()

	if got := len(eng.PendingSubmissions()); got != 3 {
		t.Fatalf("应恰好接受 3 份(每来源1份), 实际 %d; 错误数=%d", got, len(errs))
	}
	if len(errs) != 12 {
		t.Fatalf("应有 12 份重复提交被拒绝, 实际 %d", len(errs))
	}
	for _, e := range errs {
		var re *auth.RejectError
		if !errors.As(e, &re) || re.Reason != auth.ReasonDuplicate {
			t.Fatalf("重复提交应返回 duplicate, 得到 %v", e)
		}
	}
	r, err := eng.CloseRound(100*sec, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusPublished || len(r.UsedSources) != 3 {
		t.Fatalf("应发布且使用 3 来源, status=%s used=%v", r.Status, r.UsedSources)
	}
	t.Logf("用例通过: 15 份并发提交 -> 接受 3、拒绝重复 12，轮次发布，用源=%v", r.UsedSources)
}

func TestRoundSwitchNoSkip(t *testing.T) {
	// 业务：轮次切换确定
	t.Log("业务：轮次切换 —— 轮次号单调递增、旧轮拒收、禁止跳轮")
	c := cfgV1()
	eng := newTestEngine(t, RecoveryVoid, c)
	r1 := eng.CurrentRoundID()
	if r1 != 1 {
		t.Fatalf("首轮应为 1, 得到 %d", r1)
	}
	if _, err := eng.OpenRound(1); err == nil {
		t.Fatal("open 状态下开启新一轮必须报错")
	}
	err := eng.Submit(signSub(c, r1+5, "src-a", 100, 100*sec, "x"), 100*sec)
	var re *auth.RejectError
	if !errors.As(err, &re) || re.Reason != auth.ReasonClosedRound {
		t.Fatalf("向错误轮次提交应拒绝 closed_round, 得到 %v", err)
	}
	if _, err := eng.CloseRound(100*sec, false); err != nil {
		t.Fatal(err)
	}
	r2, err := eng.OpenRound(101 * sec)
	if err != nil {
		t.Fatal(err)
	}
	if r2 != 2 {
		t.Fatalf("第二轮应为 2, 得到 %d", r2)
	}
	t.Log("用例通过: 轮次 1->2 连续；禁止重复开轮；错误轮次提交被拒")
}

func TestUpgradeOnlyAffectsNewRounds(t *testing.T) {
	// 业务：配置版本演进
	t.Log("业务：配置升级 —— v2 只对新轮次生效，历史轮次仍按 v1 解释与复现")
	v1 := cfgV1()
	eng := newTestEngine(t, RecoveryVoid, v1)

	// round 1 用 v1 发布
	r1 := eng.CurrentRoundID()
	eng.Submit(signSub(v1, r1, "src-a", 100, 100*sec, "a1"), 100*sec)
	eng.Submit(signSub(v1, r1, "src-b", 100, 100*sec, "b1"), 100*sec)
	res1, err := eng.CloseRound(100*sec, true)
	if err != nil {
		t.Fatal(err)
	}
	if res1.ConfigVer != 1 || res1.Price != 100 {
		t.Fatalf("round1 应 v1@100, 得到 v%d@%.2f", res1.ConfigVer, res1.Price)
	}

	// round 2 开启后升级 v2：round 2 仍冻结在 v1
	r2 := eng.CurrentRoundID()
	if err := eng.Upgrade(cfgV2()); err != nil {
		t.Fatal(err)
	}
	if eng.CurrentConfigVersion() != 1 {
		t.Fatalf("进行中的 round %d 不应被升级影响", r2)
	}
	// v2 才登记的 src-d 不能参与仍按 v1 解释的 round 2
	err = eng.Submit(signSub(cfgV2(), r2, "src-d", 100, 101*sec, "d1"), 101*sec)
	var re *auth.RejectError
	if !errors.As(err, &re) || re.Reason != auth.ReasonUnauthorized {
		t.Fatalf("旧轮次应拒绝新来源, 得到 %v", err)
	}
	eng.Submit(signSub(v1, r2, "src-a", 100, 101*sec, "a2"), 101*sec)
	eng.Submit(signSub(v1, r2, "src-b", 100, 101*sec, "b2"), 101*sec)
	res2, _ := eng.CloseRound(101*sec, true)
	if res2.ConfigVer != 1 {
		t.Fatalf("round2 应仍为 v1, 得到 v%d", res2.ConfigVer)
	}

	// round 3 使用 v2：min_sources=3，只有 2 来源时按策略回退
	r3 := eng.CurrentRoundID()
	if eng.CurrentConfigVersion() != 2 {
		t.Fatalf("新轮次应使用 v2, 得到 v%d", eng.CurrentConfigVersion())
	}
	v2 := cfgV2()
	eng.Submit(signSub(v2, r3, "src-a", 100, 102*sec, "a3"), 102*sec)
	eng.Submit(signSub(v2, r3, "src-b", 100, 102*sec, "b3"), 102*sec)
	res3, _ := eng.CloseRound(102*sec, false)
	if res3.ConfigVer != 2 || res3.Status != StatusFallback || res3.DerivedFrom != 2 {
		t.Fatalf("round3 应 v2 下回退到 round1, 得到 v%d %s derived=%d", res3.ConfigVer, res3.Status, res3.DerivedFrom)
	}
	if eng.Result(1).Price != 100 || eng.Result(1).ConfigVer != 1 {
		t.Fatal("历史 round1 被升级改写")
	}
	t.Log("用例通过: 进行中轮次冻结旧版规则；新来源不能注入旧轮；新轮次采用 v2；历史不变")
}

func TestRestartVoidPolicy(t *testing.T) {
	// 业务：崩溃恢复 - void
	t.Log("业务：崩溃恢复(void) —— 已确认轮次不丢失、不重复发布，未完成轮次作废")
	dir := t.TempDir()
	v1 := cfgV1()
	h1, _ := config.NewHistory(v1)
	eng1, err := New(Options{DataDir: dir, History: h1, Recovery: RecoveryVoid})
	if err != nil {
		t.Fatal(err)
	}
	r1 := eng1.CurrentRoundID()
	eng1.Submit(signSub(v1, r1, "src-a", 100, 100*sec, "a"), 100*sec)
	eng1.Submit(signSub(v1, r1, "src-b", 100, 100*sec, "b"), 100*sec)
	res1, _ := eng1.CloseRound(100*sec, true)

	// round 2 提交一半后“崩溃”（不显式关闭，模拟进程消失）
	r2 := eng1.CurrentRoundID()
	eng1.Submit(signSub(v1, r2, "src-a", 999, 101*sec, "a2"), 101*sec)
	seq1, _ := eng1.ConfirmedSeq(r1)
	eng1.Close()

	// 重启
	h2, _ := config.NewHistory(cfgV1())
	eng2, err := New(Options{DataDir: dir, History: h2, Recovery: RecoveryVoid})
	if err != nil {
		t.Fatal(err)
	}
	recovered := eng2.Result(r1)
	if recovered == nil || recovered.Status != StatusPublished || recovered.Price != res1.Price {
		t.Fatalf("已确认轮次丢失: %+v", recovered)
	}
	seq1After, ok := eng2.ConfirmedSeq(r1)
	if !ok || seq1After != seq1 {
		t.Fatalf("确认事件被重复写入: seq %d -> %d", seq1, seq1After)
	}
	voided := eng2.Result(r2)
	if voided == nil || voided.Status != StatusVoided {
		t.Fatalf("未完成轮次应作废, 得到 %+v", voided)
	}
	if eng2.CurrentRoundID() != r2+1 {
		t.Fatalf("重启后应开启新轮 %d, 实际 %d", r2+1, eng2.CurrentRoundID())
	}
	t.Logf("用例通过: round1 价格 %.2f 保留且 seq=%d 不变; round2 作废; 新轮=%d",
		recovered.Price, seq1After, eng2.CurrentRoundID())
	eng2.Close()
}

func TestRestartResumePolicy(t *testing.T) {
	// 业务：崩溃恢复 - resume
	t.Log("业务：崩溃恢复(resume) —— 未完成轮次连同已接受提交一致恢复，可继续完成")
	dir := t.TempDir()
	v1 := cfgV1()
	h1, _ := config.NewHistory(v1)
	eng1, _ := New(Options{DataDir: dir, History: h1, Recovery: RecoveryResume})
	r1 := eng1.CurrentRoundID()
	eng1.Submit(signSub(v1, r1, "src-a", 100, 100*sec, "a"), 100*sec)
	eng1.Close()

	h2, _ := config.NewHistory(cfgV1())
	eng2, err := New(Options{DataDir: dir, History: h2, Recovery: RecoveryResume})
	if err != nil {
		t.Fatal(err)
	}
	if eng2.CurrentRoundID() != r1 || !eng2.IsOpen() {
		t.Fatalf("resume 应恢复 open 状态的 round %d", r1)
	}
	if got := len(eng2.PendingSubmissions()); got != 1 {
		t.Fatalf("应恢复 1 份已接受提交, 得到 %d", got)
	}
	err = eng2.Submit(signSub(v1, r1, "src-a", 101, 100*sec, "a-again"), 100*sec)
	var re *auth.RejectError
	if !errors.As(err, &re) || re.Reason != auth.ReasonDuplicate {
		t.Fatalf("恢复后重放检测应仍生效, 得到 %v", err)
	}
	eng2.Submit(signSub(v1, r1, "src-b", 100, 100*sec, "b"), 100*sec)
	res, err := eng2.CloseRound(100*sec, false)
	if err != nil || res.Status != StatusPublished || res.Price != 100 {
		t.Fatalf("恢复后轮次应正常发布 100, 得到 %+v err=%v", res, err)
	}
	t.Log("用例通过: 未完成轮次与提交恢复，重放检测生效，轮次正常发布")
	eng2.Close()
}

func TestReproduceRoundFromLog(t *testing.T) {
	// 业务：历史可复现
	t.Log("业务：历史可复现 —— 用事件日志中的提交与配置版本重算，结论必须逐字段一致")
	dir := t.TempDir()
	v1 := cfgV1()
	h1, _ := config.NewHistory(v1)
	eng1, _ := New(Options{DataDir: dir, History: h1, Recovery: RecoveryVoid})
	r1 := eng1.CurrentRoundID()
	eng1.Submit(signSub(v1, r1, "src-a", 100, 100*sec, "a"), 100*sec)
	eng1.Submit(signSub(v1, r1, "src-b", 100.5, 100*sec, "b"), 100*sec)
	stored, _ := eng1.CloseRound(100*sec, false)
	eng1.Close()

	// 新进程：从日志取提交，在空目录用记录的配置版本重算
	h2, _ := config.NewHistory(cfgV1())
	reEng, _ := New(Options{DataDir: t.TempDir(), History: h2, Recovery: RecoveryVoid})
	for _, s := range stored.Submissions {
		if err := reEng.Submit(s, s.Timestamp); err != nil {
			t.Fatal(err)
		}
	}
	got, err := reEng.CloseRound(stored.CloseNS, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != stored.Status || got.Price != stored.Price || got.MedianRaw != stored.MedianRaw {
		t.Fatalf("复现不一致: stored %s %.4f/%.4f vs got %s %.4f/%.4f",
			stored.Status, stored.Price, stored.MedianRaw, got.Status, got.Price, got.MedianRaw)
	}
	t.Logf("用例通过: 重算 status=%s price=%.4f median=%.4f 与日志记录完全一致", got.Status, got.Price, got.MedianRaw)
}
