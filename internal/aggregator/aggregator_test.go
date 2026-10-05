package aggregator

import (
	"math"
	"testing"
)

func q(id string, price, weight float64, ts int64) Quote {
	return Quote{SourceID: id, Price: price, Weight: weight, Timestamp: ts, Nonce: "n-" + id}
}

func defaultRule() Rule {
	return Rule{MinSources: 3, MaxStalenessNS: 30, MaxDeviationBPS: 200, HardMinPrice: 90, HardMaxPrice: 110}
}

func TestWeightedMedianDeterminism(t *testing.T) {
	// 业务：加权中位聚合
	t.Log("业务：加权中位聚合 —— 乱序输入结果确定，权重过半决定中位价")
	in := []Quote{q("d", 101, 2, 100), q("a", 100, 1, 100), q("c", 100.5, 1, 100), q("b", 99.5, 1, 100)}
	opt := Options{Rule: defaultRule(), CloseTimeNS: 100}
	r1 := Aggregate(in, opt)
	// 打乱输入顺序再算一次
	r2 := Aggregate([]Quote{in[2], in[0], in[3], in[1]}, opt)
	if r1.Outcome != OutcomePublish || r2.Outcome != OutcomePublish {
		t.Fatalf("期望发布, 得到 %s / %s", r1.Outcome, r2.Outcome)
	}
	if r1.Price != r2.Price {
		t.Fatalf("聚合结果依赖输入顺序: %.6f vs %.6f", r1.Price, r2.Price)
	}
	// 权重 d=2 在 5 总权重中占 2，排序后累计: b1->a2->c3>2.5 => 中位应为 c=100.5
	if math.Abs(r1.Price-100.5) > 1e-9 {
		t.Fatalf("加权中位价期望 100.5, 得到 %.6f", r1.Price)
	}
	t.Logf("用例通过: 两次乱序聚合价均为 %.4f", r1.Price)
}

func TestInsufficientAndStalenessBoundaries(t *testing.T) {
	// 业务：来源不足与陈旧报价降级边界
	t.Log("业务：降级边界 —— min_sources 临界、陈旧时间戳剔除")
	rl := defaultRule()

	// 恰好达到 min_sources：发布
	r := Aggregate([]Quote{q("a", 100, 1, 100), q("b", 100, 1, 100), q("c", 100, 1, 100)},
		Options{Rule: rl, CloseTimeNS: 100})
	if r.Outcome != OutcomePublish {
		t.Fatalf("3/3 应发布, 得到 %s (%s)", r.Outcome, r.Reason)
	}
	t.Log("用例通过: 恰好 3 个新鲜来源 -> publish")

	// 少一个：拒绝
	r = Aggregate([]Quote{q("a", 100, 1, 100), q("b", 100, 1, 100)},
		Options{Rule: rl, CloseTimeNS: 100})
	if r.Outcome != OutcomeReject || r.Reason != "insufficient_sources" {
		t.Fatalf("2 来源应拒绝, 得到 %s/%s", r.Outcome, r.Reason)
	}
	t.Log("用例通过: 仅 2 个新鲜来源 -> reject(insufficient_sources)")

	// 3 个提交但 1 个陈旧（ts+30 < close=131）：新鲜仅 2 -> 拒绝
	r = Aggregate([]Quote{q("a", 100, 1, 100), q("b", 100, 1, 100), q("c", 100, 1, 100)},
		Options{Rule: rl, CloseTimeNS: 131})
	if r.Outcome != OutcomeReject || len(r.DroppedStale) != 3 {
		t.Fatalf("陈旧报价应被剔除并拒绝, outcome=%s dropped=%d", r.Outcome, len(r.DroppedStale))
	}
	t.Log("用例通过: 超出新鲜窗口的报价被剔除 -> insufficient_sources")

	// 边界：ts+30 == close 视为新鲜（条件为严格小于）
	r = Aggregate([]Quote{q("a", 100, 1, 100), q("b", 100, 1, 100), q("c", 100, 1, 100)},
		Options{Rule: rl, CloseTimeNS: 130})
	if r.Outcome != OutcomePublish {
		t.Fatalf("边界时刻 ts+maxStaleness==close 应视为新鲜, 得到 %s", r.Outcome)
	}
	t.Log("用例通过: ts+staleness==close 边界判为新鲜 -> publish")
}

func TestHardBandDefendsCollusion(t *testing.T) {
	// 业务：反串通硬价格带
	t.Log("业务：反串通 —— 多数来源报价 500 也无法越过硬上限 110")
	rl := defaultRule()
	r := Aggregate(
		[]Quote{q("a", 500, 1, 100), q("b", 500, 1, 100), q("c", 500, 1, 100)},
		Options{Rule: rl, CloseTimeNS: 100, LastValidPrice: 100, DeviationPolicy: "reject"})
	if r.MedianRaw != 500 {
		t.Fatalf("原始中位应为 500, 得到 %.2f", r.MedianRaw)
	}
	// 偏差策略 reject：整体拒绝；即便策略放行，价格也已被钳到 110
	if r.Outcome != OutcomeReject {
		t.Fatalf("期望拒绝, 得到 %s", r.Outcome)
	}
	t.Log("用例通过: 串通报价 500 -> 原始中位 500 被识别并拒绝（钳制值=110）")

	// 无偏差参照（首轮）时，钳制后直接发布 110
	r = Aggregate(
		[]Quote{q("a", 500, 1, 100), q("b", 500, 1, 100), q("c", 500, 1, 100)},
		Options{Rule: rl, CloseTimeNS: 100})
	if r.Outcome != OutcomePublish || r.Price != 110 {
		t.Fatalf("首轮串通报价应被钳制为 110 后发布, 得到 %s %.2f", r.Outcome, r.Price)
	}
	t.Log("用例通过: 首轮无参照时串通报价被钳制到硬上限 110 发布")
}

func TestDeviationRejectAndFallback(t *testing.T) {
	// 业务：偏差超限两种处置
	t.Log("业务：偏差降级 —— reject 与 fallback_last_confirmed 两种策略")
	rl := defaultRule()
	// 101.5 相对 100 偏离 150bps，未超 200bps -> 发布
	r := Aggregate([]Quote{q("a", 101.5, 1, 100), q("b", 101.5, 1, 100), q("c", 101.5, 1, 100)},
		Options{Rule: rl, CloseTimeNS: 100, LastValidPrice: 100, DeviationPolicy: "reject"})
	if r.Outcome != OutcomePublish {
		t.Fatalf("150bps 未超 200bps 应发布, 得到 %s", r.Outcome)
	}
	t.Log("用例通过: 偏差 150bps < 200bps -> publish")

	// 103 => 300bps > 200bps，reject
	r = Aggregate([]Quote{q("a", 103, 1, 100), q("b", 103, 1, 100), q("c", 103, 1, 100)},
		Options{Rule: rl, CloseTimeNS: 100, LastValidPrice: 100, DeviationPolicy: "reject"})
	if r.Outcome != OutcomeReject || r.Price != 0 {
		t.Fatalf("期望 reject 且不产出价格, 得到 %s %.2f", r.Outcome, r.Price)
	}
	t.Log("用例通过: 偏差 300bps + reject -> 不产出价格")

	// fallback 策略：回退价 100
	r = Aggregate([]Quote{q("a", 103, 1, 100), q("b", 103, 1, 100), q("c", 103, 1, 100)},
		Options{Rule: rl, CloseTimeNS: 100, LastValidPrice: 100, DeviationPolicy: "fallback_last_confirmed"})
	if r.Outcome != OutcomeFallback || r.Price != 100 {
		t.Fatalf("期望 fallback 到 100, 得到 %s %.2f", r.Outcome, r.Price)
	}
	t.Log("用例通过: 偏差超限 + fallback -> 回退价 100")
}
