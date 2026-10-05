// Package sim provides fully local, deterministic stand-ins for off-chain
// data sources: each source signs quotes with its configured HMAC key.
package sim

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/oracle"
)

type Source struct {
	Cfg oracle.SourceConfig
}

func New(cfg oracle.SourceConfig) *Source { return &Source{Cfg: cfg} }

// Quote builds and signs a valid submission for the given round.
func (s *Source) Quote(asset string, round uint64, price oracle.Price, at time.Time, nonce string) (oracle.Submission, error) {
	if nonce == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return oracle.Submission{}, err
		}
		nonce = hex.EncodeToString(b[:])
	}
	sig := oracle.Sign(s.Cfg.Key, s.Cfg.ID, asset, round, price, at, nonce)
	return oracle.Submission{
		SourceID:  s.Cfg.ID,
		Asset:     asset,
		Round:     round,
		Price:     price,
		QuoteTime: at,
		Nonce:     nonce,
		Sig:       sig,
	}, nil
}

// QuoteTampered returns a submission whose signature does not match, to
// exercise the bad_signature rejection path.
func (s *Source) QuoteBadSignature(asset string, round uint64, price oracle.Price, at time.Time) (oracle.Submission, error) {
	sub, err := s.Quote(asset, round, price, at, "")
	if err != nil {
		return sub, err
	}
	sub.Sig = "0000000000000000000000000000000000000000000000000000000000000000"
	return sub, nil
}

// QuoteAs signs with this source's key but claims another identity: the
// signature will not verify under the claimed source's configured key.
func (s *Source) QuoteAs(claimedID, key, asset string, round uint64, price oracle.Price, at time.Time) (oracle.Submission, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return oracle.Submission{}, err
	}
	nonce := hex.EncodeToString(b[:])
	return oracle.Submission{
		SourceID:  claimedID,
		Asset:     asset,
		Round:     round,
		Price:     price,
		QuoteTime: at,
		Nonce:     nonce,
		Sig:       oracle.Sign(key, claimedID, asset, round, price, at, nonce),
	}, nil
}

// ScenarioFeed models one source's view of one asset: a base price plus a
// fixed per-source offset and optional per-round overrides.
type ScenarioFeed struct {
	SourceID string
	Offset   oracle.Price
}

// Sim drives the engine through rounds using local feeds. It is deterministic
// given the same clock and overrides.
type Sim struct {
	Engine  *oracle.Engine
	Sources map[string]*Source
	Base    map[string]oracle.Price
	Feeds   map[string][]ScenarioFeed // asset -> source feeds in source order
}

func NewSim(eng *oracle.Engine, cfg oracle.Config) *Sim {
	srcs := map[string]*Source{}
	for id, sc := range cfg.Sources {
		srcs[id] = New(sc)
	}
	return &Sim{Engine: eng, Sources: srcs, Base: map[string]oracle.Price{}, Feeds: map[string][]ScenarioFeed{}}
}

func (m *Sim) SetFeed(asset string, base oracle.Price, feeds ...ScenarioFeed) {
	m.Base[asset] = base
	m.Feeds[asset] = feeds
}

// SubmitRound sends each configured feed's quote for round r. override lets a
// scenario mutate individual submissions (e.g. mark one stale or colluding).
func (m *Sim) SubmitRound(r uint64, at time.Time, override func(sourceID string, sub oracle.Submission) oracle.Submission) ([]string, error) {
	reasons := []string{}
	for asset, feeds := range m.Feeds {
		base := m.Base[asset]
		for _, f := range feeds {
			src, ok := m.Sources[f.SourceID]
			if !ok {
				return reasons, fmt.Errorf("unknown sim source %q", f.SourceID)
			}
			sub, err := src.Quote(asset, r, base+f.Offset, at, fmt.Sprintf("r%d-%s-%s", r, asset, f.SourceID))
			if err != nil {
				return reasons, err
			}
			if override != nil {
				sub = override(f.SourceID, sub)
			}
			if _, err := m.Engine.Submit(sub); err != nil {
				if re, ok := err.(*oracle.RejectError); ok {
					reasons = append(reasons, re.Reason)
				} else {
					reasons = append(reasons, err.Error())
				}
			}
		}
	}
	return reasons, nil
}
