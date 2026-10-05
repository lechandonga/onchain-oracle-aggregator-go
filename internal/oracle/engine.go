package oracle

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

type openRound struct {
	round    uint64
	openedAt time.Time
	cfgVer   int64
	quotes   map[string]map[string]AcceptedSubmission
}

// Engine is the concurrency-safe aggregator. All state transitions are
// durably appended to Store before being applied in memory.
type Engine struct {
	mu    sync.Mutex
	store Store
	now   func() time.Time

	configs map[int64]Config
	active  Config

	open    *openRound
	reports map[uint64]*RoundReport
	voided  map[uint64]bool
	nonces  map[string]bool
	subIDs  map[string]bool

	lastPrice map[string]Price
	lastRound map[string]uint64
	lastPubAt map[string]time.Time
}

func NewEngine(store Store, now func() time.Time) (*Engine, error) {
	if now == nil {
		now = time.Now
	}
	e := &Engine{
		store:     store,
		now:       now,
		configs:   map[int64]Config{},
		reports:   map[uint64]*RoundReport{},
		voided:    map[uint64]bool{},
		nonces:    map[string]bool{},
		subIDs:    map[string]bool{},
		lastPrice: map[string]Price{},
		lastRound: map[string]uint64{},
		lastPubAt: map[string]time.Time{},
	}
	if err := e.recover(); err != nil {
		return nil, err
	}
	return e, nil
}

func submissionID(s Submission) string {
	sum := sha256.Sum256([]byte(SigningMessage(s.SourceID, s.Asset, s.Round, s.Price, s.QuoteTime, s.Nonce)))
	return hex.EncodeToString(sum[:])
}

func (e *Engine) InstallConfig(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, exists := e.configs[c.Version]; exists {
		return &RejectError{Reason: "config_version_exists", Detail: sprintf("v%d", c.Version)}
	}
	if e.active.Version != 0 && c.Version <= e.active.Version {
		return &RejectError{Reason: "config_version_too_old", Detail: sprintf("v%d <= v%d", c.Version, e.active.Version)}
	}
	if err := e.store.Append(Event{Type: EvConfigInstalled, Config: &c}); err != nil {
		return err
	}
	e.applyConfig(c)
	return nil
}

func (e *Engine) applyConfig(c Config) {
	e.configs[c.Version] = c
	if e.active.Version == 0 || c.Version > e.active.Version {
		e.active = c
	}
}

func (e *Engine) ActiveConfig() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active
}

func (e *Engine) CurrentRound() (uint64, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.open == nil {
		return 0, false
	}
	return e.open.round, true
}

func (e *Engine) lastRoundNumber() uint64 {
	var n uint64
	for r := range e.reports {
		if r > n {
			n = r
		}
	}
	for r := range e.voided {
		if r > n {
			n = r
		}
	}
	if e.open != nil && e.open.round > n {
		n = e.open.round
	}
	return n
}

func (e *Engine) OpenRound() (uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.openRoundLocked(e.now())
}

func (e *Engine) openRoundLocked(at time.Time) (uint64, error) {
	if e.active.Version == 0 {
		return 0, &RejectError{Reason: "no_config_installed"}
	}
	if e.open != nil {
		return 0, &RejectError{Reason: "round_already_open", Detail: sprintf("round %d", e.open.round)}
	}
	n := e.lastRoundNumber() + 1
	if err := e.store.Append(Event{Type: EvRoundOpened, Round: n, AtUnix: at.UnixNano(), ConfigVersion: e.active.Version}); err != nil {
		return 0, err
	}
	e.open = &openRound{round: n, openedAt: at, cfgVer: e.active.Version, quotes: map[string]map[string]AcceptedSubmission{}}
	return n, nil
}

// Advance closes the open round and immediately opens the next one, so the
// close+open transition is atomic and concurrent submitters can never see a
// gap in which a quote is lost or counted twice.
func (e *Engine) Advance() (*RoundReport, uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	report, err := e.closeRoundLocked(e.now())
	if err != nil {
		return nil, 0, err
	}
	next, err := e.openRoundLocked(e.now())
	if err != nil {
		return report, 0, err
	}
	return report, next, nil
}

func (e *Engine) CloseRound() (*RoundReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closeRoundLocked(e.now())
}

func (e *Engine) closeRoundLocked(at time.Time) (*RoundReport, error) {
	if e.open == nil {
		return nil, &RejectError{Reason: "no_open_round"}
	}
	cfg, ok := e.configs[e.open.cfgVer]
	if !ok {
		return nil, &RejectError{Reason: "config_missing", Detail: sprintf("v%d", e.open.cfgVer)}
	}

	report := &RoundReport{
		Round:         e.open.round,
		OpenedAt:      e.open.openedAt,
		ClosedAt:      at,
		ConfigVersion: e.open.cfgVer,
		Decisions:     map[string]AssetDecision{},
	}

	assets := make([]string, 0, len(cfg.Assets))
	for a := range cfg.Assets {
		assets = append(assets, a)
	}
	sort.Strings(assets)

	overall := RoundPublished
	for _, asset := range assets {
		rule := cfg.Assets[asset]
		bySource := e.open.quotes[asset]

		fresh := make([]AcceptedSubmission, 0, len(bySource))
		staleIDs := []string{}
		for srcID, q := range bySource {
			if at.Sub(q.QuoteTime) > cfg.MaxQuoteStaleness {
				staleIDs = append(staleIDs, srcID)
				continue
			}
			fresh = append(fresh, q)
		}
		sort.Slice(fresh, func(i, j int) bool {
			if fresh[i].SourceID != fresh[j].SourceID {
				return fresh[i].SourceID < fresh[j].SourceID
			}
			if !fresh[i].QuoteTime.Equal(fresh[j].QuoteTime) {
				return fresh[i].QuoteTime.Before(fresh[j].QuoteTime)
			}
			return fresh[i].SubmissionID < fresh[j].SubmissionID
		})
		sort.Strings(staleIDs)

		d := aggregate(asset, fresh, rule, cfg.Version, e.lastPricePtr(asset))
		for _, id := range staleIDs {
			d.Checks = append(d.Checks, checkf("dropped_stale_quote source=%s max_staleness=%s", id, cfg.MaxQuoteStaleness))
		}

		if d.Status == RoundRejected {
			if lp, has := e.lastPrice[asset]; has {
				pubAt := e.lastPubAt[asset]
				age := at.Sub(pubAt)
				if age <= cfg.FallbackStaleness {
					based := e.lastRound[asset]
					d.Status = RoundFallback
					p := lp
					d.Price = &p
					d.BasedOnRound = &based
					d.Checks = append(d.Checks, checkf("fallback to round=%d price=%d age=%s within=%s", based, lp, age, cfg.FallbackStaleness))
				} else {
					d.Checks = append(d.Checks, checkf("fallback refused: last result age=%s exceeds=%s", age, cfg.FallbackStaleness))
				}
			} else {
				d.Checks = append(d.Checks, "fallback refused: no previous confirmed result")
			}
		}

		if d.Status == RoundPublished {
			e.lastPrice[asset] = *d.Price
			e.lastRound[asset] = report.Round
			e.lastPubAt[asset] = at
		}
		switch d.Status {
		case RoundRejected:
			overall = RoundRejected
		case RoundFallback:
			if overall != RoundRejected {
				overall = RoundFallback
			}
		}
		report.Decisions[asset] = d
	}
	report.Status = overall

	if err := e.store.Append(Event{Type: EvRoundFinalized, Round: report.Round, AtUnix: at.UnixNano(), ConfigVersion: cfg.Version, Report: report}); err != nil {
		return nil, err
	}
	e.reports[report.Round] = report
	e.open = nil
	return report, nil
}

func (e *Engine) lastPricePtr(asset string) *Price {
	if p, ok := e.lastPrice[asset]; ok {
		return &p
	}
	return nil
}

func (e *Engine) Submit(s Submission) (AcceptedSubmission, error) {
	if err := s.ValidateFormat(); err != nil {
		return AcceptedSubmission{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.open == nil {
		return AcceptedSubmission{}, &RejectError{Reason: RejectRoundClosed, Detail: "no round currently open"}
	}
	if s.Round != e.open.round {
		return AcceptedSubmission{}, &RejectError{Reason: RejectRoundClosed, Detail: sprintf("submitted round %d, open round %d", s.Round, e.open.round)}
	}
	cfg, ok := e.configs[e.open.cfgVer]
	if !ok {
		return AcceptedSubmission{}, &RejectError{Reason: "config_missing", Detail: sprintf("v%d", e.open.cfgVer)}
	}
	src, authorized := cfg.Source(s.SourceID)
	if !authorized {
		return AcceptedSubmission{}, &RejectError{Reason: RejectUnauthorized, Detail: sprintf("source %s not active in config v%d", s.SourceID, cfg.Version)}
	}
	if _, known := cfg.Assets[s.Asset]; !known {
		return AcceptedSubmission{}, &RejectError{Reason: RejectAssetUnknown, Detail: s.Asset}
	}
	if err := verifySignature(src.Key, s); err != nil {
		return AcceptedSubmission{}, err
	}
	now := e.now()
	if age := now.Sub(s.QuoteTime); age > cfg.SubmissionTTL {
		return AcceptedSubmission{}, &RejectError{Reason: RejectExpired, Detail: sprintf("quote age %s > ttl %s", age, cfg.SubmissionTTL)}
	}
	if s.QuoteTime.Sub(now) > cfg.ClockSkew {
		return AcceptedSubmission{}, &RejectError{Reason: RejectExpired, Detail: "quote timestamp too far in the future"}
	}

	id := submissionID(s)
	if e.subIDs[id] {
		return AcceptedSubmission{}, &RejectError{Reason: RejectDuplicate, Detail: "identical submission already accepted"}
	}
	if e.nonces[s.Nonce] {
		return AcceptedSubmission{}, &RejectError{Reason: RejectDuplicate, Detail: sprintf("nonce %s already used", s.Nonce)}
	}
	if bySrc, exists := e.open.quotes[s.Asset]; exists {
		if _, dup := bySrc[s.SourceID]; dup {
			return AcceptedSubmission{}, &RejectError{Reason: RejectDuplicate, Detail: sprintf("source %s already quoted %s this round", s.SourceID, s.Asset)}
		}
	}

	acc := AcceptedSubmission{
		SourceID:     s.SourceID,
		Asset:        s.Asset,
		Round:        s.Round,
		Price:        s.Price,
		QuoteTime:    s.QuoteTime,
		AcceptedAt:   now,
		Nonce:        s.Nonce,
		SubmissionID: id,
	}
	if err := e.store.Append(Event{Type: EvSubmissionAccepted, Round: s.Round, AtUnix: now.UnixNano(), ConfigVersion: cfg.Version, Sub: &acc}); err != nil {
		return AcceptedSubmission{}, err
	}
	if e.open.quotes[s.Asset] == nil {
		e.open.quotes[s.Asset] = map[string]AcceptedSubmission{}
	}
	e.open.quotes[s.Asset][s.SourceID] = acc
	e.subIDs[id] = true
	e.nonces[s.Nonce] = true
	return acc, nil
}

func (e *Engine) Report(round uint64) (*RoundReport, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.reports[round]
	return r, ok
}

func (e *Engine) Reports() []RoundReport {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]RoundReport, 0, len(e.reports))
	for _, r := range e.reports {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Round < out[j].Round })
	return out
}

func (e *Engine) LatestPrice(asset string) (Price, uint64, time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.lastPrice[asset]
	if !ok {
		return 0, 0, time.Time{}, false
	}
	return p, e.lastRound[asset], e.lastPubAt[asset], true
}

func (e *Engine) IsVoided(round uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.voided[round]
}
