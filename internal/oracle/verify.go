package oracle

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// RoundMismatch describes one finalized round whose recorded report does not
// match the report recomputed from the recorded inputs and rules.
type RoundMismatch struct {
	Round uint64
	Diffs []string
}

// VerifyResult is the outcome of auditing an event log.
type VerifyResult struct {
	Events          int
	RoundsFinalized int
	RoundsVoided    int
	// CompletedRounds lists every finalized or voided round, in order.
	CompletedRounds []uint64
	// Problems are structural violations: missing/duplicate/out-of-order
	// completed rounds, finalize events without an open round, etc.
	Problems   []string
	Mismatches []RoundMismatch
}

// OK reports whether the log passed every check: record integrity (verified
// during Replay), contiguous non-duplicated completed rounds, and every
// recorded report matching its recomputation.
func (r *VerifyResult) OK() bool {
	return len(r.Problems) == 0 && len(r.Mismatches) == 0
}

// Verify replays the event log and recomputes every finalized round from the
// recorded inputs (installed configs, accepted submissions, timestamps) with
// the same deterministic code the engine used, then compares the outcome
// against the recorded report round by round. It also checks that completed
// rounds are neither lost, duplicated, nor skipped. Record-level corruption
// (gaps, disorder, unparsable lines) is returned as a LogIntegrityError.
func Verify(store Store) (*VerifyResult, error) {
	var events []Event
	if err := store.Replay(func(ev Event) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		return nil, err
	}

	res := &VerifyResult{Events: len(events)}
	configs := map[int64]Config{}
	var open *openRound
	last := map[string]lastConfirmed{}
	completed := map[uint64]bool{}

	for _, ev := range events {
		switch ev.Type {
		case EvConfigInstalled:
			if ev.Config != nil {
				configs[ev.Config.Version] = *ev.Config
			}
		case EvRoundOpened:
			if open != nil {
				res.Problems = append(res.Problems, fmt.Sprintf("round %d opened while round %d still open", ev.Round, open.round))
			}
			open = &openRound{
				round:    ev.Round,
				openedAt: time.Unix(0, ev.AtUnix).UTC(),
				cfgVer:   ev.ConfigVersion,
				quotes:   map[string]map[string]AcceptedSubmission{},
			}
		case EvSubmissionAccepted:
			if open == nil || ev.Sub == nil {
				res.Problems = append(res.Problems, fmt.Sprintf("seq %d: submission accepted with no open round", ev.Seq))
				continue
			}
			acc := *ev.Sub
			if open.quotes[acc.Asset] == nil {
				open.quotes[acc.Asset] = map[string]AcceptedSubmission{}
			}
			open.quotes[acc.Asset][acc.SourceID] = acc
		case EvRoundFinalized:
			if ev.Report == nil {
				res.Problems = append(res.Problems, fmt.Sprintf("seq %d: finalize event without report", ev.Seq))
				continue
			}
			if open == nil {
				res.Problems = append(res.Problems, fmt.Sprintf("round %d finalized without being opened", ev.Report.Round))
			} else if open.round != ev.Report.Round {
				res.Problems = append(res.Problems, fmt.Sprintf("round %d finalized while round %d open", ev.Report.Round, open.round))
			}
			cfg, ok := configs[ev.Report.ConfigVersion]
			if !ok {
				res.Problems = append(res.Problems, fmt.Sprintf("round %d: config v%d not found in log", ev.Report.Round, ev.Report.ConfigVersion))
				recordPublishedLocal(last, ev.Report)
			} else if open != nil {
				recomputed := finalizeReport(open, cfg, time.Unix(0, ev.AtUnix).UTC(), last)
				if diffs := diffReports(ev.Report, recomputed); len(diffs) > 0 {
					res.Mismatches = append(res.Mismatches, RoundMismatch{Round: ev.Report.Round, Diffs: diffs})
				}
				// The honest chain continues from the recomputed state, so
				// one tampered round cannot launder later rounds.
				recordPublishedLocal(last, recomputed)
			} else {
				recordPublishedLocal(last, ev.Report)
			}
			markCompleted(res, completed, ev.Report.Round)
			res.RoundsFinalized++
			open = nil
		case EvRoundVoided:
			markCompleted(res, completed, ev.Round)
			res.RoundsVoided++
			open = nil
		}
	}

	// Completed rounds must form the contiguous sequence 1..N: nothing lost,
	// nothing duplicated, nothing skipped.
	for i, r := range res.CompletedRounds {
		if want := uint64(i + 1); r != want {
			res.Problems = append(res.Problems, fmt.Sprintf("completed round %d at position %d, expected %d (lost, duplicated or skipped round)", r, i+1, want))
			break
		}
	}
	return res, nil
}

func markCompleted(res *VerifyResult, completed map[uint64]bool, round uint64) {
	if completed[round] {
		res.Problems = append(res.Problems, fmt.Sprintf("round %d completed more than once", round))
		return
	}
	completed[round] = true
	res.CompletedRounds = append(res.CompletedRounds, round)
}

func recordPublishedLocal(last map[string]lastConfirmed, r *RoundReport) {
	assets := make([]string, 0, len(r.Decisions))
	for a := range r.Decisions {
		assets = append(assets, a)
	}
	sort.Strings(assets)
	for _, a := range assets {
		d := r.Decisions[a]
		if d.Status == RoundPublished && d.Price != nil {
			last[a] = lastConfirmed{price: *d.Price, round: r.Round, pubAt: r.ClosedAt}
		}
	}
}

// diffReports returns a human-readable list of differences between the
// recorded report and the recomputed one. Identical reports yield nil.
func diffReports(recorded, recomputed *RoundReport) []string {
	rj, _ := json.Marshal(recorded)
	cj, _ := json.Marshal(recomputed)
	if string(rj) == string(cj) {
		return nil
	}
	var diffs []string
	add := func(format string, args ...interface{}) { diffs = append(diffs, fmt.Sprintf(format, args...)) }
	if recorded.Status != recomputed.Status {
		add("status: recorded=%s recomputed=%s", recorded.Status, recomputed.Status)
	}
	if recorded.ConfigVersion != recomputed.ConfigVersion {
		add("config_version: recorded=%d recomputed=%d", recorded.ConfigVersion, recomputed.ConfigVersion)
	}
	if !recorded.OpenedAt.Equal(recomputed.OpenedAt) {
		add("opened_at: recorded=%s recomputed=%s", recorded.OpenedAt, recomputed.OpenedAt)
	}
	if !recorded.ClosedAt.Equal(recomputed.ClosedAt) {
		add("closed_at: recorded=%s recomputed=%s", recorded.ClosedAt, recomputed.ClosedAt)
	}
	assets := map[string]bool{}
	for a := range recorded.Decisions {
		assets[a] = true
	}
	for a := range recomputed.Decisions {
		assets[a] = true
	}
	names := make([]string, 0, len(assets))
	for a := range assets {
		names = append(names, a)
	}
	sort.Strings(names)
	for _, a := range names {
		rd, rok := recorded.Decisions[a]
		cd, cok := recomputed.Decisions[a]
		if !rok || !cok {
			add("asset %s: present recorded=%v recomputed=%v", a, rok, cok)
			continue
		}
		if rd.Status != cd.Status {
			add("asset %s status: recorded=%s recomputed=%s", a, rd.Status, cd.Status)
		}
		if fmt.Sprint(ptrVal(rd.Price)) != fmt.Sprint(ptrVal(cd.Price)) {
			add("asset %s price: recorded=%v recomputed=%v", a, ptrVal(rd.Price), ptrVal(cd.Price))
		}
		if fmt.Sprint(ptrVal(rd.BasedOnRound)) != fmt.Sprint(ptrVal(cd.BasedOnRound)) {
			add("asset %s based_on_round: recorded=%v recomputed=%v", a, ptrVal(rd.BasedOnRound), ptrVal(cd.BasedOnRound))
		}
		if rd.RejectReason != cd.RejectReason {
			add("asset %s reject_reason: recorded=%q recomputed=%q", a, rd.RejectReason, cd.RejectReason)
		}
		if rd.AnomalyPolicy != cd.AnomalyPolicy {
			add("asset %s anomaly_policy: recorded=%q recomputed=%q", a, rd.AnomalyPolicy, cd.AnomalyPolicy)
		}
		if rd.Clamped != cd.Clamped {
			add("asset %s clamped: recorded=%v recomputed=%v", a, rd.Clamped, cd.Clamped)
		}
		if len(rd.AcceptedQuotes) != len(cd.AcceptedQuotes) {
			add("asset %s accepted_quotes: recorded=%d recomputed=%d", a, len(rd.AcceptedQuotes), len(cd.AcceptedQuotes))
		}
		if len(rd.Checks) != len(cd.Checks) {
			add("asset %s checks: recorded=%d entries recomputed=%d entries", a, len(rd.Checks), len(cd.Checks))
		}
	}
	if len(diffs) == 0 {
		diffs = append(diffs, "reports differ in detail fields only")
	}
	return diffs
}

func ptrVal[T any](p *T) interface{} {
	if p == nil {
		return "<nil>"
	}
	return *p
}
