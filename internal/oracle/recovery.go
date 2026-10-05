package oracle

import (
	"sort"
	"time"
)

// recover replays the durable event log. Confirmed rounds are rebuilt
// byte-for-byte; a round left open by a crash is either resumed with all
// accepted quotes or voided, according to the config under which it opened.
func (e *Engine) recover() error {
	var pendingOpen *openRound

	apply := func(ev Event) error {
		switch ev.Type {
		case EvConfigInstalled:
			if ev.Config != nil {
				e.applyConfig(*ev.Config)
			}
		case EvRoundOpened:
			pendingOpen = &openRound{
				round:    ev.Round,
				openedAt: time.Unix(0, ev.AtUnix),
				cfgVer:   ev.ConfigVersion,
				quotes:   map[string]map[string]AcceptedSubmission{},
			}
		case EvSubmissionAccepted:
			if pendingOpen == nil || ev.Sub == nil {
				return nil
			}
			acc := *ev.Sub
			if pendingOpen.quotes[acc.Asset] == nil {
				pendingOpen.quotes[acc.Asset] = map[string]AcceptedSubmission{}
			}
			pendingOpen.quotes[acc.Asset][acc.SourceID] = acc
			e.subIDs[acc.SubmissionID] = true
			e.nonces[acc.Nonce] = true
		case EvRoundFinalized:
			if ev.Report == nil {
				return nil
			}
			r := *ev.Report
			e.reports[r.Round] = &r
			pendingOpen = nil
			e.recordPublished(&r)
			for _, d := range r.Decisions {
				for _, q := range d.AcceptedQuotes {
					e.subIDs[q.SubmissionID] = true
					e.nonces[q.Nonce] = true
				}
			}
		case EvRoundVoided:
			e.voided[ev.Round] = true
			pendingOpen = nil
		}
		return nil
	}

	if err := e.store.Replay(apply); err != nil {
		return err
	}

	if pendingOpen != nil {
		cfg, ok := e.configs[pendingOpen.cfgVer]
		policy := "resume"
		if ok && cfg.OnRestartOpenRound != "" {
			policy = cfg.OnRestartOpenRound
		}
		if policy == "void" {
			// Quotes of the voided round must not block fresh nonces in the
			// next round unless the same submission is literally replayed;
			// subIDs/nonces are kept so replays are deterministically
			// rejected as duplicates and re-signed instead.
			if err := e.store.Append(Event{Type: EvRoundVoided, Round: pendingOpen.round, Reason: "open_round_voided_on_restart"}); err != nil {
				return err
			}
			e.voided[pendingOpen.round] = true
		} else {
			e.open = pendingOpen
		}
	}
	return nil
}

// recordPublished rebuilds the "last confirmed fresh result" indexes. Only
// PUBLISHED decisions count; FALLBACK does not extend freshness.
func (e *Engine) recordPublished(r *RoundReport) {
	assets := make([]string, 0, len(r.Decisions))
	for a := range r.Decisions {
		assets = append(assets, a)
	}
	sort.Strings(assets)
	for _, a := range assets {
		d := r.Decisions[a]
		if d.Status == RoundPublished && d.Price != nil {
			e.lastPrice[a] = *d.Price
			e.lastRound[a] = r.Round
			e.lastPubAt[a] = r.ClosedAt
		}
	}
}
