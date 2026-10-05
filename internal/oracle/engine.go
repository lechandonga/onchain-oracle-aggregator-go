// Package oracle 是聚合组件的核心引擎：负责并发提交的串行化、
// 轮次生命周期、规则切换（仅对新轮次生效）以及基于事件日志的崩溃恢复。
package oracle

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/aggregator"
	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/auth"
	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/config"
	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/store"
)

// RecoveryPolicy 定义重启时未完成（open）轮次的处置。
type RecoveryPolicy string

const (
	// RecoveryVoid 作废未完成轮次（默认）：重启后旧轮次追加 voided 事件，
	// 并开一个新轮次，语义明确、不会出现“半轮价格”。
	RecoveryVoid RecoveryPolicy = "void"
	// RecoveryResume 恢复未完成轮次：继续接受该轮提交，由调用方负责关闭。
	RecoveryResume RecoveryPolicy = "resume"
)

// Options 是引擎参数。
type Options struct {
	DataDir           string
	History           *config.History
	Recovery          RecoveryPolicy
	FutureToleranceNS int64 // 允许来源时钟超前引擎的幅度
}

// Engine 是线程安全的聚合引擎。
type Engine struct {
	mu     sync.Mutex
	hist   *config.History
	st     *store.Store
	policy RecoveryPolicy
	future int64

	roundID int64
	open    bool
	openNS  int64
	cfgVer  int64
	subs    []auth.Submission
	seen    map[string]string // sourceID -> 已接受 nonce（重放/重复检测）

	lastValidRound int64
	lastValidPrice float64
	results        map[int64]*RoundResult
	confirmedSeq   map[int64]int64 // roundID -> 事件 seq

	listeners []func(RoundResult)
}

// New 创建（或恢复）引擎。
func New(opt Options) (*Engine, error) {
	if opt.History == nil || opt.History.Latest() == nil {
		return nil, errors.New("options.History is empty")
	}
	if opt.Recovery == "" {
		opt.Recovery = RecoveryVoid
	}
	if opt.Recovery != RecoveryVoid && opt.Recovery != RecoveryResume {
		return nil, fmt.Errorf("unknown recovery policy %q", opt.Recovery)
	}

	e := &Engine{
		hist: opt.History, policy: opt.Recovery, future: opt.FutureToleranceNS,
		seen: map[string]string{}, results: map[int64]*RoundResult{},
		confirmedSeq: map[int64]int64{},
	}
	st, err := store.Open(opt.DataDir, e.replay)
	if err != nil {
		return nil, err
	}
	e.st = st

	// 崩溃恢复：处理日志尾部未完成的轮次。
	if e.open {
		switch e.policy {
		case RecoveryVoid:
			if err := e.voidCurrentLocked(0, "voided on restart: unfinished round"); err != nil {
				return nil, err
			}
			if err := e.openRoundLocked(0); err != nil {
				return nil, err
			}
		case RecoveryResume:
			// 保留 open 状态继续收集；配置版本冻结为该轮创建时的版本。
		}
	} else {
		// 全新库或最后一轮已终结：确保调用方立即可用，自动开启新一轮（不跳轮）。
		if err := e.openRoundLocked(0); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// Close 持久化并关闭。
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.st.Close()
}

func (e *Engine) replay(ev store.Event) error {
	e.roundID = ev.RoundID
	switch ev.Type {
	case store.EvtRoundOpened:
		e.open = true
		e.openNS = ev.TimeNS
		e.cfgVer = ev.ConfigVer
		e.subs = nil
		e.seen = map[string]string{}
	case store.EvtSubmission:
		var s auth.Submission
		if err := json.Unmarshal(ev.Payload, &s); err != nil {
			return err
		}
		e.subs = append(e.subs, s)
		e.seen[s.SourceID] = s.Nonce
	case store.EvtRoundConfirmed, store.EvtRoundRejected, store.EvtRoundVoided:
		var r RoundResult
		if err := json.Unmarshal(ev.Payload, &r); err != nil {
			return err
		}
		e.open = false
		e.results[r.RoundID] = &r
		e.confirmedSeq[r.RoundID] = ev.Seq
		if ev.Type == store.EvtRoundConfirmed {
			e.lastValidRound = r.RoundID
			e.lastValidPrice = r.Price
		}
	}
	return nil
}

func (e *Engine) currentCfg() *config.Config {
	c, _ := e.hist.Version(e.cfgVer)
	if c == nil {
		c = e.hist.Latest()
	}
	return c
}

// OpenRound 显式开启新一轮；若当前轮仍 open 则返回错误，避免跳轮。
func (e *Engine) OpenRound(nowNS int64) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.open {
		return 0, fmt.Errorf("round %d still open; close it before opening a new one", e.roundID)
	}
	err := e.openRoundLocked(nowNS)
	return e.roundID, err
}

func (e *Engine) openRoundLocked(nowNS int64) error {
	next := e.roundID + 1 // roundID 单调 +1，杜绝跳轮
	cfg := e.hist.Latest()
	if _, err := e.st.Append(next, cfg.Version, nowNS, store.EvtRoundOpened, nil); err != nil {
		return err
	}
	e.roundID = next
	e.open = true
	e.openNS = nowNS
	e.cfgVer = cfg.Version
	e.subs = nil
	e.seen = map[string]string{}
	return nil
}

func (e *Engine) voidCurrentLocked(nowNS int64, reason string) error {
	r := &RoundResult{
		RoundID: e.roundID, Pair: e.currentCfg().Pair, ConfigVer: e.cfgVer,
		Status: StatusVoided, OpenNS: e.openNS, CloseNS: nowNS,
		Submissions: append([]auth.Submission(nil), e.subs...), Reason: reason,
	}
	if _, err := e.st.Append(r.RoundID, r.ConfigVer, nowNS, store.EvtRoundVoided, r); err != nil {
		return err
	}
	e.results[r.RoundID] = r
	e.open = false
	return nil
}

// Submit 鉴权并接收一份报价提交。并发安全；同一轮同一来源重复提交被拒绝。
func (e *Engine) Submit(s auth.Submission, nowNS int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := auth.ValidateFormat(s); err != nil {
		return err
	}
	if !e.open {
		return &auth.RejectError{Reason: auth.ReasonClosedRound, Detail: fmt.Sprintf("no open round (last=%d)", e.roundID)}
	}
	if s.RoundID != e.roundID {
		return &auth.RejectError{Reason: auth.ReasonClosedRound,
			Detail: fmt.Sprintf("submission targets round %d but open round is %d", s.RoundID, e.roundID)}
	}
	cfg := e.currentCfg()
	v := auth.NewValidator(cfg)
	if err := v.Authenticate(s, nowNS, e.future); err != nil {
		return err
	}
	if nonce, dup := e.seen[s.SourceID]; dup {
		return &auth.RejectError{Reason: auth.ReasonDuplicate,
			Detail: fmt.Sprintf("source %q already submitted round %d (nonce %s)", s.SourceID, e.roundID, nonce)}
	}
	if _, err := e.st.Append(e.roundID, cfg.Version, nowNS, store.EvtSubmission, s); err != nil {
		return err
	}
	e.subs = append(e.subs, s)
	e.seen[s.SourceID] = s.Nonce
	return nil
}

// CloseRound 关闭当前轮：执行聚合、按策略处置并持久化终局结果。
// 关闭后若 autoOpen 为真，立即开启下一轮（使用当时最新配置版本）。
func (e *Engine) CloseRound(nowNS int64, autoOpen bool) (*RoundResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.open {
		return nil, fmt.Errorf("no open round to close (last=%d)", e.roundID)
	}
	cfg := e.currentCfg()

	quotes := make([]aggregator.Quote, 0, len(e.subs))
	for _, s := range e.subs {
		src := cfg.Sources[s.SourceID]
		quotes = append(quotes, aggregator.Quote{
			SourceID: s.SourceID, Price: s.Price, Weight: src.Weight,
			Timestamp: s.Timestamp, Nonce: s.Nonce,
		})
	}

	rl := cfg.Rule
	ag := aggregator.Aggregate(quotes, aggregator.Options{
		Rule: aggregator.Rule{
			MinSources: rl.MinSources, MaxStalenessNS: rl.MaxStalenessNS,
			MaxDeviationBPS: rl.MaxDeviationBPS,
			HardMinPrice:    rl.HardMinPrice, HardMaxPrice: rl.HardMaxPrice,
		},
		CloseTimeNS:     nowNS,
		LastValidPrice:  e.lastValidPrice,
		DeviationPolicy: string(rl.DeviationPolicy),
	})

	r := &RoundResult{
		RoundID: e.roundID, Pair: cfg.Pair, ConfigVer: cfg.Version,
		OpenNS: e.openNS, CloseNS: nowNS,
		Submissions: append([]auth.Submission(nil), e.subs...),
		Checks:      ag.Checks, MedianRaw: ag.MedianRaw,
		UsedSources: sourceIDs(ag.UsedQuotes),
	}

	switch ag.Outcome {
	case aggregator.OutcomePublish:
		r.Status = StatusPublished
		r.Price = ag.Price
	case aggregator.OutcomeFallback:
		if e.lastValidRound == 0 {
			// 策略要求回退但历史上没有任何有效结果：降级为拒绝，绝不静默产出。
			r.Status = StatusRejected
			r.Reason = ag.Reason + " (no prior valid price to fall back to)"
		} else {
			r.Status = StatusFallback
			r.Price = ag.Price
			r.DerivedFrom = e.lastValidRound
			r.Reason = ag.Reason
		}
	case aggregator.OutcomeReject:
		// 不足/陈旧类异常也允许按 insufficient_policy 回退。
		if ag.Reason == "insufficient_sources" && rl.InsufficientPolicy == config.PolicyFallback && e.lastValidRound != 0 {
			aggregator.MarkFallback(ag, e.lastValidPrice)
			r.Status = StatusFallback
			r.Price = ag.Price
			r.DerivedFrom = e.lastValidRound
			r.Reason = ag.Reason
		} else {
			r.Status = StatusRejected
			r.Reason = ag.Reason
			if ag.Reason == "insufficient_sources" && rl.InsufficientPolicy == config.PolicyFallback {
				r.Reason = ag.Reason + " (no prior valid price to fall back to)"
			}
		}
	}

	var evType store.EventType
	switch r.Status {
	case StatusPublished, StatusFallback:
		evType = store.EvtRoundConfirmed
	default:
		evType = store.EvtRoundRejected
	}
	ev, err := e.st.Append(r.RoundID, r.ConfigVer, nowNS, evType, r)
	if err != nil {
		return nil, err
	}
	e.open = false
	e.results[r.RoundID] = r
	e.confirmedSeq[r.RoundID] = ev.Seq
	if r.Status == StatusPublished || r.Status == StatusFallback {
		e.lastValidRound = r.RoundID
		e.lastValidPrice = r.Price
	}

	res := *r
	if autoOpen {
		if err := e.openRoundLocked(nowNS); err != nil {
			return nil, err
		}
	}
	for _, fn := range e.listeners {
		fn(res)
	}
	return &res, nil
}

// Upgrade 登记新版本配置；只影响之后开启的轮次。
func (e *Engine) Upgrade(c *config.Config) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hist.AddVersion(c)
}

// Subscribe 注册“轮次终局”回调（内存订阅，用于本地消费/演示）。
func (e *Engine) Subscribe(fn func(RoundResult)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.listeners = append(e.listeners, fn)
}

func sourceIDs(qs []aggregator.Quote) []string {
	out := make([]string, 0, len(qs))
	for _, q := range qs {
		out = append(out, q.SourceID)
	}
	return out
}

// --- 只读快照（测试与 CLI 使用，均加锁） ---

func (e *Engine) CurrentRoundID() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.roundID
}

func (e *Engine) IsOpen() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.open
}

func (e *Engine) CurrentConfigVersion() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfgVer
}

// Result 返回某轮终局结果；open 轮返回 nil。
func (e *Engine) Result(roundID int64) *RoundResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r, ok := e.results[roundID]; ok {
		cp := *r
		return &cp
	}
	return nil
}

// LastValid 返回最近一次有效（published/fallback）轮次与价格。
func (e *Engine) LastValid() (int64, float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastValidRound, e.lastValidPrice
}

// PendingSubmissions 返回当前 open 轮已接受的提交（恢复测试用）。
func (e *Engine) PendingSubmissions() []auth.Submission {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]auth.Submission(nil), e.subs...)
}

// ConfirmedSeq 返回轮次终局事件的日志序号（证明“只确认一次”）。
func (e *Engine) ConfirmedSeq(roundID int64) (int64, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	seq, ok := e.confirmedSeq[roundID]
	return seq, ok
}
