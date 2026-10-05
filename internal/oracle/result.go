package oracle

import (
	"encoding/json"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/aggregator"
	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/auth"
)

// RoundStatus 是轮次生命周期状态。
type RoundStatus string

const (
	StatusOpen      RoundStatus = "open"
	StatusPublished RoundStatus = "published" // 正常发布
	StatusFallback  RoundStatus = "fallback"  // 发布，但价格回退自最近有效轮次
	StatusRejected  RoundStatus = "rejected"  // 拒绝发布
	StatusVoided    RoundStatus = "voided"    // 崩溃恢复时作废的未完成轮次
)

// RoundResult 是一个轮次的最终结论与可核验判定依据，会整体写入事件日志。
type RoundResult struct {
	RoundID     int64                   `json:"round_id"`
	Pair        string                  `json:"pair"`
	ConfigVer   int64                   `json:"config_version"`
	Status      RoundStatus             `json:"status"`
	Price       float64                 `json:"price,omitempty"`
	DerivedFrom int64                   `json:"derived_from_round,omitempty"` // fallback 时指向价格来源轮次
	OpenNS      int64                   `json:"open_ns"`
	CloseNS     int64                   `json:"close_ns"`
	MedianRaw   float64                 `json:"median_raw,omitempty"`
	UsedSources []string                `json:"used_sources,omitempty"`
	Submissions []auth.Submission       `json:"submissions"` // 该轮全部已接受提交（去重后）
	Checks      []aggregator.CheckTrace `json:"checks,omitempty"`
	Reason      string                  `json:"reason,omitempty"`
}

func (r RoundResult) JSON() json.RawMessage {
	b, _ := json.MarshalIndent(r, "", "  ")
	return b
}
