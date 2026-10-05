// Package aggregator 实现纯函数化的聚合规则：
// 输入一组已通过鉴权的报价与一份不可变规则，输出成交价或明确的拒绝/回退判定，
// 并附带可核验的判定依据（basis），不依赖外部状态、不产生随机行为。
package aggregator

import (
	"fmt"
	"math"
	"sort"
)

// Quote 是参与聚合的单来源有效报价。
type Quote struct {
	SourceID  string
	Price     float64
	Weight    float64
	Timestamp int64
	Nonce     string
}

// Outcome 是聚合判定的类别。
type Outcome string

const (
	OutcomePublish  Outcome = "publish"  // 正常产出本轮聚合价
	OutcomeReject   Outcome = "reject"   // 拒绝发布
	OutcomeFallback Outcome = "fallback" // 回退到最近一次有效结果
)

// CheckTrace 记录单项规则检查的输入与结论，作为判定依据。
type CheckTrace struct {
	Check  string  `json:"check"`
	Passed bool    `json:"passed"`
	Detail string  `json:"detail"`
	Value  float64 `json:"value,omitempty"`
	Limit  float64 `json:"limit,omitempty"`
}

// Result 是一次聚合的完整结论。
type Result struct {
	Outcome      Outcome
	Price        float64 // OutcomePublish 时为本轮聚合价；Fallback 时为回退价（由引擎填充）
	MedianRaw    float64 // 硬价格带钳制之前的加权中位价
	UsedQuotes   []Quote
	DroppedStale []Quote
	Checks       []CheckTrace
	Reason       string // reject/fallback 的策略原因
	TotalWeight  float64
}

// Rule 是聚合所需规则的最小视图（由 config.AggregationRule 适配）。
type Rule struct {
	MinSources      int
	MaxStalenessNS  int64
	MaxDeviationBPS float64
	HardMinPrice    float64
	HardMaxPrice    float64
}

// Options 控制聚合行为。
type Options struct {
	Rule            Rule
	CloseTimeNS     int64   // 轮次关闭时刻，用于陈旧判定
	LastValidPrice  float64 // 最近一次 confirmed 有效价格，<=0 表示尚无参照
	DeviationPolicy string  // "reject" | "fallback_last_confirmed"
}

// Aggregate 执行确定性聚合。
//
// 顺序固定：去重(在引擎层完成) -> 陈旧剔除 -> 来源数检查 ->
// 加权中位 -> 硬价格带钳制 -> 偏差检查。每一步都记录 trace。
func Aggregate(quotes []Quote, opt Options) *Result {
	r := &Result{Outcome: OutcomeReject}

	// 1) 陈旧剔除：按来源报价时间戳与关轮时刻比较。
	fresh := make([]Quote, 0, len(quotes))
	for _, q := range quotes {
		if opt.Rule.MaxStalenessNS > 0 && q.Timestamp+opt.Rule.MaxStalenessNS < opt.CloseTimeNS {
			r.DroppedStale = append(r.DroppedStale, q)
			continue
		}
		fresh = append(fresh, q)
	}
	r.Checks = append(r.Checks, CheckTrace{
		Check:  "staleness",
		Passed: true,
		Detail: fmt.Sprintf("%d/%d quotes fresh at close %d (max staleness %d ns)", len(fresh), len(quotes), opt.CloseTimeNS, opt.Rule.MaxStalenessNS),
		Value:  float64(len(fresh)),
	})

	// 2) 来源数量检查（按去重后的来源计，权重不作为数量）。
	if len(fresh) < opt.Rule.MinSources {
		r.Checks = append(r.Checks, CheckTrace{
			Check: "min_sources", Passed: false,
			Detail: fmt.Sprintf("only %d fresh sources, need %d", len(fresh), opt.Rule.MinSources),
			Value:  float64(len(fresh)), Limit: float64(opt.Rule.MinSources),
		})
		r.Reason = "insufficient_sources"
		return r
	}
	r.Checks = append(r.Checks, CheckTrace{
		Check: "min_sources", Passed: true,
		Detail: fmt.Sprintf("%d fresh sources >= min %d", len(fresh), opt.Rule.MinSources),
		Value:  float64(len(fresh)), Limit: float64(opt.Rule.MinSources),
	})

	// 3) 加权中位数：按价格排序，累计权重过半的第一档即结果。
	// 并列时取价格较低者——价格序列确定，因此结果确定。
	sorted := append([]Quote(nil), fresh...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Price != sorted[j].Price {
			return sorted[i].Price < sorted[j].Price
		}
		return sorted[i].SourceID < sorted[j].SourceID
	})
	var total float64
	for _, q := range sorted {
		total += q.Weight
	}
	r.TotalWeight = total
	half := total / 2
	var acc float64
	var median float64
	for _, q := range sorted {
		acc += q.Weight
		if acc > half || math.Abs(acc-half) < 1e-12 {
			median = q.Price
			break
		}
	}
	r.MedianRaw = median
	r.Checks = append(r.Checks, CheckTrace{
		Check: "weighted_median", Passed: true,
		Detail: fmt.Sprintf("weighted median over %d quotes, total weight %.6f", len(sorted), total),
		Value:  median,
	})

	// 4) 硬价格带：少数来源串通也无法把价格推出允许范围。
	price := median
	clampedLow := opt.Rule.HardMinPrice > 0 && price < opt.Rule.HardMinPrice
	clampedHigh := opt.Rule.HardMaxPrice > 0 && price > opt.Rule.HardMaxPrice
	if clampedLow {
		price = opt.Rule.HardMinPrice
	}
	if clampedHigh {
		price = opt.Rule.HardMaxPrice
	}
	r.Checks = append(r.Checks, CheckTrace{
		Check:  "hard_band",
		Passed: !(clampedLow || clampedHigh),
		Detail: bandDetail(median, price, opt.Rule, clampedLow, clampedHigh),
		Value:  price,
	})

	// 5) 偏差检查（相对最近一次有效结果；首轮无参照时跳过）。
	if opt.LastValidPrice > 0 && opt.Rule.MaxDeviationBPS > 0 {
		devBPS := math.Abs(price-opt.LastValidPrice) / opt.LastValidPrice * 10000
		if devBPS > opt.Rule.MaxDeviationBPS {
			r.Checks = append(r.Checks, CheckTrace{
				Check: "deviation", Passed: false,
				Detail: fmt.Sprintf("deviation %.4fbps from last %.6f exceeds limit %.4fbps", devBPS, opt.LastValidPrice, opt.Rule.MaxDeviationBPS),
				Value:  devBPS, Limit: opt.Rule.MaxDeviationBPS,
			})
			r.Reason = "deviation_exceeded"
			if opt.DeviationPolicy == "fallback_last_confirmed" {
				r.Outcome = OutcomeFallback
				r.Price = opt.LastValidPrice
			}
			r.UsedQuotes = sorted
			return r
		}
		r.Checks = append(r.Checks, CheckTrace{
			Check: "deviation", Passed: true,
			Detail: fmt.Sprintf("deviation %.4fbps within limit %.4fbps", devBPS, opt.Rule.MaxDeviationBPS),
			Value:  devBPS, Limit: opt.Rule.MaxDeviationBPS,
		})
	}

	r.Outcome = OutcomePublish
	r.Price = price
	r.UsedQuotes = sorted
	return r
}

func bandDetail(raw, final float64, rl Rule, low, high bool) string {
	switch {
	case low:
		return fmt.Sprintf("raw median %.6f below hard min %.6f, clamped", raw, rl.HardMinPrice)
	case high:
		return fmt.Sprintf("raw median %.6f above hard max %.6f, clamped", raw, rl.HardMaxPrice)
	default:
		return fmt.Sprintf("raw median %.6f within hard band [%.6f, %.6f]", raw, rl.HardMinPrice, rl.HardMaxPrice)
	}
}

// MarkFallback 供引擎在策略为回退时复用：把不足结论标记为 fallback。
func MarkFallback(r *Result, lastValid float64) {
	r.Outcome = OutcomeFallback
	r.Price = lastValid
}
