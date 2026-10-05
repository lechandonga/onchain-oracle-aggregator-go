package oracle

import "sort"

func sortPrices(q []AcceptedSubmission) []Price {
	out := make([]Price, len(q))
	for i, s := range q {
		out[i] = s.Price
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func meanInt64(xs []Price) Price {
	if len(xs) == 0 {
		return 0
	}
	var sum int64
	for _, x := range xs {
		sum += int64(x)
	}
	return Price(sum / int64(len(xs)))
}

func medianPrice(sorted []Price) Price {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return Price((int64(sorted[n/2-1]) + int64(sorted[n/2])) / 2)
}

func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func clampToBand(p, last Price, floorBps, ceilBps int) (Price, bool) {
	floor := last - Price(int64(last)*int64(floorBps)/10000)
	ceil := last + Price(int64(last)*int64(ceilBps)/10000)
	if p < floor {
		return floor, true
	}
	if p > ceil {
		return ceil, true
	}
	return p, false
}

// aggregate is a pure, deterministic function of the accepted quotes, the
// asset rule and the last confirmed price. All arithmetic is integer based.
func aggregate(asset string, quotes []AcceptedSubmission, rule AssetRule, cfgVersion int64, last *Price) AssetDecision {
	d := AssetDecision{
		Asset:          asset,
		Status:         RoundPublished,
		ConfigVersion:  cfgVersion,
		AcceptedQuotes: quotes,
		Checks:         []string{},
	}

	// 1. Source count gate.
	if len(quotes) < rule.MinSources {
		d.Status = RoundRejected
		d.RejectReason = "insufficient_sources"
		d.Checks = append(d.Checks, checkf("source_count=%d < min_sources=%d", len(quotes), rule.MinSources))
		return d
	}
	d.Checks = append(d.Checks, checkf("source_count=%d >= min_sources=%d", len(quotes), rule.MinSources))

	// 2. Deterministic ordering (by price; quotes list is already source
	// sorted), then trim the configured number of extremes.
	ordered := sortPrices(quotes)
	d.OrderedPrices = ordered
	lo, hi := rule.TrimLow, rule.TrimHigh
	if lo+hi >= len(ordered) {
		d.Status = RoundRejected
		d.RejectReason = "trim_exhausts_quotes"
		d.Checks = append(d.Checks, checkf("trim_low=%d + trim_high=%d exhausts %d quotes", lo, hi, len(ordered)))
		return d
	}
	trimmed := append([]Price(nil), ordered[lo:len(ordered)-hi]...)
	d.TrimmedPrices = trimmed

	med := medianPrice(trimmed)
	mn := meanInt64(trimmed)
	d.Median = &med
	d.Mean = &mn

	// 3. Deviation gate. The spread is measured over ALL accepted quotes
	// (before trimming), relative to the median, so a single extreme outlier
	// cannot be silently trimmed away and hide a disagreement anomaly.
	loP, hiP := ordered[0], ordered[len(ordered)-1]
	var devBp int64
	if med > 0 {
		devBp = int64(hiP-loP) * 10000 / int64(med)
	}
	d.DeviationBp = &devBp
	if int(devBp) > rule.MaxDeviationBasisPoints {
		d.Status = RoundRejected
		d.RejectReason = "deviation_exceeds_threshold"
		d.Checks = append(d.Checks, checkf("deviation_bp=%d > max_bp=%d (spread=%d median=%d)", devBp, rule.MaxDeviationBasisPoints, hiP-loP, med))
		return d
	}
	d.Checks = append(d.Checks, checkf("deviation_bp=%d <= max_bp=%d", devBp, rule.MaxDeviationBasisPoints))

	// 4. Mean of trimmed quotes, then anti-collusion hard band around the
	// last confirmed result.
	price := mn
	if last != nil && (rule.HardFloorBps > 0 || rule.HardCeilingBps > 0) {
		clamped, didClamp := clampToBand(price, *last, rule.HardFloorBps, rule.HardCeilingBps)
		if didClamp {
			d.Clamped = true
			d.Checks = append(d.Checks, checkf("mean=%d outside hard band of last=%d -> clamped=%d", price, *last, clamped))
			price = clamped
		} else {
			d.Checks = append(d.Checks, checkf("mean=%d within hard band of last=%d", price, *last))
		}
	}
	p := price
	d.Price = &p
	d.Checks = append(d.Checks, checkf("published_price=%d (trimmed_mean=%d)", price, mn))
	return d
}

func checkf(format string, args ...interface{}) string {
	return sprintf(format, args...)
}
