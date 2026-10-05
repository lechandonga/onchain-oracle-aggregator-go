// Package config 定义数据源与聚合规则的版本化配置。
//
// 一份配置对应一个版本号：版本号单调递增，引擎只在开启新一轮次时切换到
// 已生效的最新版本，已发布轮次永远使用其创建时的配置版本进行解释，
// 从而保证升级不追溯改写历史。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
)

// FallbackPolicy 定义异常情形下的处置策略。
type FallbackPolicy string

const (
	// PolicyReject 拒绝发布本轮结果（轮次标记为 rejected，不产出价格）。
	PolicyReject FallbackPolicy = "reject"
	// PolicyFallback 回退到最近一次 confirmed 的有效结果。
	PolicyFallback FallbackPolicy = "fallback_last_confirmed"
)

// Source 是单个报价数据源的身份与授权信息。
type Source struct {
	// ID 来源唯一标识（全局唯一，参与签名规范串）。
	ID string `json:"id"`
	// Secret 该来源的共享密钥（HMAC-SHA256），仅本地模拟链下身份使用。
	Secret string `json:"secret"`
	// Weight 聚合权重，必须 > 0。
	Weight float64 `json:"weight"`
	// NotBeforeRFC3339 身份生效起点，空表示不限制。
	NotBeforeRFC3339 string `json:"not_before,omitempty"`
	// NotAfterRFC3339 身份失效时刻，空表示不限制。
	NotAfterRFC3339 string `json:"not_after,omitempty"`
}

// AggregationRule 是某一版本的聚合规则。
type AggregationRule struct {
	// MinSources 参与聚合所需的最少有效报价来源数（按去重后的来源计）。
	MinSources int `json:"min_sources"`
	// MaxStaleness 报价相对轮次关闭时刻允许的最大年龄；0 表示不检查。
	MaxStalenessNS int64 `json:"max_staleness_ns"`
	// MaxDeviationBPS 相对最近一次有效结果允许的最大偏差（基点，1bp=0.01%）。
	// 0 表示不检查；首轮没有参照结果时该检查跳过。
	MaxDeviationBPS float64 `json:"max_deviation_bps"`
	// InsufficientPolicy 来源不足/全部过期时的处置策略。
	InsufficientPolicy FallbackPolicy `json:"insufficient_policy"`
	// DeviationPolicy 偏差超限时的处置策略。
	DeviationPolicy FallbackPolicy `json:"deviation_policy"`
	// HardMinPrice / HardMaxPrice 硬性价格带：无论多少来源串通，
	// 产出价格永远不会落在区间外；<=0 的端点表示该侧不设限。
	HardMinPrice float64 `json:"hard_min_price"`
	HardMaxPrice float64 `json:"hard_max_price"`
}

// Config 是一个不可变的配置版本。
type Config struct {
	Version int64             `json:"version"`
	Pair    string            `json:"pair"`
	Sources map[string]Source `json:"sources"`
	Rule    AggregationRule   `json:"rule"`
}

// Validate 校验配置自身的合法性（非法配置不得被引擎接受）。
func (c *Config) Validate() error {
	if c.Version <= 0 {
		return errors.New("config version must be positive")
	}
	if c.Pair == "" {
		return errors.New("config pair is required")
	}
	if len(c.Sources) == 0 {
		return errors.New("config must contain at least one source")
	}
	ids := make([]string, 0, len(c.Sources))
	for id, s := range c.Sources {
		if id == "" || s.ID != id {
			return fmt.Errorf("source map key %q mismatches source id %q", id, s.ID)
		}
		if s.Secret == "" {
			return fmt.Errorf("source %q secret is required", id)
		}
		if s.Weight <= 0 {
			return fmt.Errorf("source %q weight must be positive", id)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	r := c.Rule
	if r.MinSources <= 0 {
		return errors.New("rule.min_sources must be positive")
	}
	if r.MinSources > len(c.Sources) {
		return errors.New("rule.min_sources cannot exceed configured source count")
	}
	if r.MaxStalenessNS < 0 {
		return errors.New("rule.max_staleness_ns cannot be negative")
	}
	if r.MaxDeviationBPS < 0 {
		return errors.New("rule.max_deviation_bps cannot be negative")
	}
	if r.HardMinPrice > 0 && r.HardMaxPrice > 0 && r.HardMinPrice > r.HardMaxPrice {
		return errors.New("rule hard price band is inverted")
	}
	switch r.InsufficientPolicy {
	case PolicyReject, PolicyFallback:
	default:
		return fmt.Errorf("invalid insufficient_policy %q", r.InsufficientPolicy)
	}
	switch r.DeviationPolicy {
	case PolicyReject, PolicyFallback:
	default:
		return fmt.Errorf("invalid deviation_policy %q", r.DeviationPolicy)
	}
	return nil
}

// History 是按版本号排列的配置历史（最新版本在末尾）。
type History struct {
	versions []*Config
}

// NewHistory 从一组配置构建历史，并做唯一性与排序校验。
func NewHistory(cfgs ...*Config) (*History, error) {
	h := &History{versions: append([]*Config(nil), cfgs...)}
	sort.Slice(h.versions, func(i, j int) bool { return h.versions[i].Version < h.versions[j].Version })
	for i, c := range h.versions {
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("config v%d invalid: %w", c.Version, err)
		}
		if i > 0 && h.versions[i-1].Version == c.Version {
			return nil, fmt.Errorf("duplicate config version %d", c.Version)
		}
		if i > 0 && h.versions[i-1].Pair != c.Pair {
			return nil, fmt.Errorf("config v%d changes trading pair, which is forbidden", c.Version)
		}
	}
	return h, nil
}

// At 返回不晚于 atNS 时刻可用的最高版本配置。
// 当前实现中所有配置视为立即可用（升级通过 AddVersion 在下一轮切换），
// 因此直接返回最新版本；保留时间参数以便未来支持计划生效。
func (h *History) At(atNS int64) *Config {
	if len(h.versions) == 0 {
		return nil
	}
	return h.versions[len(h.versions)-1]
}

// Version 按版本号取配置（用于复现历史轮次）。
func (h *History) Version(v int64) (*Config, bool) {
	i := sort.Search(len(h.versions), func(i int) bool { return h.versions[i].Version >= v })
	if i < len(h.versions) && h.versions[i].Version == v {
		return h.versions[i], true
	}
	return nil, false
}

// Latest 返回最新版本配置。
func (h *History) Latest() *Config { return h.At(0) }

// Versions 返回已登记的版本号（有序）。
func (h *History) Versions() []int64 {
	out := make([]int64, len(h.versions))
	for i, c := range h.versions {
		out[i] = c.Version
	}
	return out
}

// AddVersion 追加一个更高版本的配置；升级只对后续新轮次生效。
func (h *History) AddVersion(c *Config) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("config v%d invalid: %w", c.Version, err)
	}
	if latest := h.Latest(); latest != nil {
		if c.Version <= latest.Version {
			return fmt.Errorf("config version %d not greater than latest %d", c.Version, latest.Version)
		}
		if c.Pair != latest.Pair {
			return errors.New("config upgrade must not change trading pair")
		}
	}
	h.versions = append(h.versions, c)
	return nil
}

// LoadFile 从 JSON 文件加载单个配置。
func LoadFile(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return c, nil
}
