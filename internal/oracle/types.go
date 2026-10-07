package oracle

import "time"

// Price is a fixed-point quote expressed in micro-units (1e-6) of the asset
// quote currency. Integers are used everywhere so that results are exact and
// reproducible regardless of platform floating point behaviour.
type Price int64

const (
	MicrosPerUnit      Price = 1_000_000
	RejectUnauthorized       = "unauthorized_source"
	RejectExpired            = "submission_expired"
	RejectDuplicate          = "duplicate_submission"
	RejectBadFormat          = "bad_format"
	RejectBadSignature       = "bad_signature"
	RejectRoundClosed        = "round_closed"
	RejectAssetUnknown       = "asset_not_configured"
	// RejectAuthWindow means the source exists and is active, but the
	// submission arrived outside its configured authorization window.
	RejectAuthWindow = "source_auth_window"
)

// SourceConfig describes one authorized data source at a given config version.
type SourceConfig struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Key    string `json:"key"`
	Active bool   `json:"active"`
	// NotBefore / NotAfter bound the authorization window of this source.
	// Semantics are fixed and identical everywhere: the window is
	// [NotBefore, NotAfter) — NotBefore is inclusive, NotAfter is exclusive.
	// A nil endpoint means unbounded on that side; a source with neither
	// endpoint set is authorized whenever it is active (long-term valid).
	// The window is evaluated against the engine clock at submission time,
	// under the engine mutex, so concurrent submissions at the exact edge
	// are serialized and always decided the same way.
	NotBefore *time.Time `json:"not_before,omitempty"`
	NotAfter  *time.Time `json:"not_after,omitempty"`
}

// WithinWindow reports whether at falls inside the authorization window.
// Start inclusive, end exclusive: at == NotBefore is authorized,
// at == NotAfter is not.
func (s SourceConfig) WithinWindow(at time.Time) bool {
	if s.NotBefore != nil && at.Before(*s.NotBefore) {
		return false
	}
	if s.NotAfter != nil && !at.Before(*s.NotAfter) {
		return false
	}
	return true
}

// Anomaly policies selectable per asset via AssetRule.OnAnomaly.
const (
	// AnomalyReject publishes nothing for the asset on an anomalous round.
	AnomalyReject = "reject"
	// AnomalyFallback reuses the most recent confirmed result, bounded by
	// Config.FallbackStaleness.
	AnomalyFallback = "fallback"
)

// AssetRule holds the aggregation parameters for one asset.
type AssetRule struct {
	// MinSources is the minimum number of distinct accepted sources required
	// to publish a fresh result.
	MinSources int `json:"min_sources"`
	// TrimLow/TrimHigh remove extreme quotes before averaging.
	TrimLow  int `json:"trim_low"`
	TrimHigh int `json:"trim_high"`
	// MaxDeviationBasisPoints bounds the spread (max-min)/median of the
	// accepted quotes. Exceeding it is treated as a disagreement anomaly.
	MaxDeviationBasisPoints int `json:"max_deviation_bp"`
	// HardFloorBps / HardCeilingBps clamp the published result relative to
	// the last confirmed result, so a minority of colluding sources can
	// never push the price outside the allowed band. They are relative to
	// the last price and are only active once a previous result exists.
	HardFloorBps   int `json:"hard_floor_bp"`
	HardCeilingBps int `json:"hard_ceiling_bp"`
	// MinPositivePrice rejects non-positive prices as malformed at the
	// aggregation stage.
	MinPositivePrice Price `json:"min_positive_price"`
	// OnAnomaly selects what happens when a round cannot produce a fresh
	// result for this asset (too few sources, stale quotes, deviation too
	// large): AnomalyReject publishes nothing; AnomalyFallback reuses the
	// most recent confirmed result if it is younger than
	// Config.FallbackStaleness. Empty means AnomalyFallback, which keeps
	// pre-existing configs behaving exactly as before.
	OnAnomaly string `json:"on_anomaly"`
}

// AnomalyPolicy resolves the effective policy, applying the default.
func (r AssetRule) AnomalyPolicy() string {
	if r.OnAnomaly == "" {
		return AnomalyFallback
	}
	return r.OnAnomaly
}

// Config is one immutable, versioned set of sources and rules. Upgrades only
// affect rounds opened after the config is installed; finalized rounds keep
// the config version they were decided under.
type Config struct {
	Version int64                   `json:"version"`
	Sources map[string]SourceConfig `json:"sources"`
	Assets  map[string]AssetRule    `json:"assets"`
	// SubmissionTTL is how long after its timestamp a submission stays valid.
	SubmissionTTL time.Duration `json:"submission_ttl"`
	// ClockSkew allows submission timestamps slightly in the future.
	ClockSkew time.Duration `json:"clock_skew"`
	// MaxQuoteStaleness is the maximum quote age (now - quote timestamp)
	// allowed when a round closes.
	MaxQuoteStaleness time.Duration `json:"max_quote_staleness"`
	// FallbackStaleness bounds reuse of a previous confirmed result. A
	// fallback older than this forces the round to REJECTED.
	FallbackStaleness time.Duration `json:"fallback_staleness"`
	// OnRestartOpenRound chooses what happens to a round that was open when
	// the previous process died: "resume" or "void".
	OnRestartOpenRound string `json:"on_restart_open_round"`
}

// Submission is a signed quote from a source for one asset and round.
type Submission struct {
	SourceID  string
	Asset     string
	Round     uint64
	Price     Price
	QuoteTime time.Time
	Nonce     string
	Sig       string
}

// AcceptedSubmission is the engine's canonical record of an accepted quote.
type AcceptedSubmission struct {
	SourceID     string    `json:"source_id"`
	Asset        string    `json:"asset"`
	Round        uint64    `json:"round"`
	Price        Price     `json:"price_micros"`
	QuoteTime    time.Time `json:"quote_time"`
	AcceptedAt   time.Time `json:"accepted_at"`
	Nonce        string    `json:"nonce"`
	SubmissionID string    `json:"submission_id"`
}

type RoundStatus string

const (
	RoundOpen      RoundStatus = "OPEN"
	RoundPublished RoundStatus = "PUBLISHED"
	RoundFallback  RoundStatus = "FALLBACK"
	RoundRejected  RoundStatus = "REJECTED"
	RoundVoided    RoundStatus = "VOIDED"
)

// AssetDecision records exactly why an asset's round outcome was reached.
type AssetDecision struct {
	Asset          string               `json:"asset"`
	Status         RoundStatus          `json:"status"`
	Price          *Price               `json:"price_micros,omitempty"`
	BasedOnRound   *uint64              `json:"based_on_round,omitempty"`
	ConfigVersion  int64                `json:"config_version"`
	AnomalyPolicy  string               `json:"anomaly_policy,omitempty"`
	AcceptedQuotes []AcceptedSubmission `json:"accepted_quotes"`
	OrderedPrices  []Price              `json:"ordered_prices"`
	TrimmedPrices  []Price              `json:"trimmed_prices"`
	Mean           *Price               `json:"mean_micros,omitempty"`
	Median         *Price               `json:"median_micros,omitempty"`
	DeviationBp    *int64               `json:"deviation_bp,omitempty"`
	Clamped        bool                 `json:"clamped,omitempty"`
	RejectReason   string               `json:"reject_reason,omitempty"`
	Checks         []string             `json:"checks"`
}

// RoundReport is the verifiable decision record for a finalized round.
type RoundReport struct {
	Round         uint64                   `json:"round"`
	OpenedAt      time.Time                `json:"opened_at"`
	ClosedAt      time.Time                `json:"closed_at"`
	ConfigVersion int64                    `json:"config_version"`
	Status        RoundStatus              `json:"status"`
	Decisions     map[string]AssetDecision `json:"decisions"`
}

// RejectError lets callers distinguish rejection reasons programmatically.
type RejectError struct {
	Reason string
	Detail string
}

func (e *RejectError) Error() string {
	if e.Detail == "" {
		return "submission rejected: " + e.Reason
	}
	return "submission rejected: " + e.Reason + ": " + e.Detail
}
