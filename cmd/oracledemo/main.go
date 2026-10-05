// Command oracledemo runs the local on-chain oracle aggregator through a
// scripted, fully offline scenario and prints the verifiable round reports.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/oracle"
	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/sim"
)

type fakeClock struct{ t time.Time }

func main() {
	storePath := flag.String("store", "data/oracle-events.jsonl", "event log path (use :memory: for ephemeral)")
	flag.Parse()

	clock := &fakeClock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	now := func() time.Time { return clock.t }

	var store oracle.Store = oracle.NewMemoryStore()
	if *storePath != ":memory:" {
		if err := os.MkdirAll(fileDir(*storePath), 0o755); err != nil {
			fail(err)
		}
		fs, err := oracle.OpenFileStore(*storePath)
		if err != nil {
			fail(err)
		}
		store = fs
		defer fs.Close()
	}

	eng, err := oracle.NewEngine(store, now)
	failIf(err)

	cfg := demoConfigV1()
	if eng.ActiveConfig().Version == 0 {
		failIf(eng.InstallConfig(cfg))
	}

	// Restart path: the event log already contains history. Demonstrate
	// recovery instead of replaying the scripted scenario.
	if reports := eng.Reports(); len(reports) > 0 {
		fmt.Printf("recovered %d finalized round(s) from %s\n", len(reports), *storePath)
		for i := range reports {
			printReport(&reports[i])
		}
		if r, open := eng.CurrentRound(); open {
			fmt.Printf("\nunfinished round %d resumed and open for new submissions\n", r)
		} else {
			fmt.Println("\nno open round; last round cleanly finalized (no double publish)")
		}
		return
	}

	m := sim.NewSim(eng, cfg)
	m.SetFeed("BTCUSD", 60_000*oracle.MicrosPerUnit,
		sim.ScenarioFeed{SourceID: "src-a", Offset: 0},
		sim.ScenarioFeed{SourceID: "src-b", Offset: 30 * oracle.MicrosPerUnit},
		sim.ScenarioFeed{SourceID: "src-c", Offset: -20 * oracle.MicrosPerUnit},
		sim.ScenarioFeed{SourceID: "src-d", Offset: 10 * oracle.MicrosPerUnit},
	)

	// Round 1: healthy aggregation.
	r1, err := eng.OpenRound()
	failIf(err)
	_, err = m.SubmitRound(r1, clock.t, nil)
	failIf(err)
	rep1, _, err := eng.Advance()
	failIf(err)
	printReport(rep1)

	// Round 2: all four sources return ~8% higher quotes. They agree with
	// each other (deviation is zero), so the anti-collusion hard band around
	// the last confirmed result (last price +/-5%) is the only thing that
	// stops the price being pushed outside the allowed range.
	clock.t = clock.t.Add(30 * time.Second)
	r2, _ := eng.CurrentRound()
	_, _ = m.SubmitRound(r2, clock.t, func(id string, sub oracle.Submission) oracle.Submission {
		sub.Price += 4_800 * oracle.MicrosPerUnit
		sub.Sig = oracle.Sign(m.Sources[id].Cfg.Key, sub.SourceID, sub.Asset, sub.Round, sub.Price, sub.QuoteTime, sub.Nonce)
		return sub
	})
	rep2, _, err := eng.Advance()
	failIf(err)
	printReport(rep2)

	// Round 3 opens after the upgrade and is decided under config v2: all
	// four sources agree. It is still decided under config v1 because it
	// was opened before the upgrade.
	r3, _ := eng.CurrentRound()
	_, _ = m.SubmitRound(r3, clock.t, nil)
	rep3, _, err := eng.Advance()
	failIf(err)
	printReport(rep3)

	// Install upgraded config v2 (stricter min sources) between rounds; it
	// only binds rounds opened from now on.
	clock.t = clock.t.Add(30 * time.Second)
	failIf(eng.InstallConfig(demoConfigV2()))

	// Round 4: only two sources quote -> insufficient under v2's min=4 ->
	// fallback to the round-3 confirmed price.
	clock.t = clock.t.Add(30 * time.Second)
	r4, _ := eng.CurrentRound()
	_, _ = m.SubmitRound(r4, clock.t, func(id string, sub oracle.Submission) oracle.Submission {
		if id == "src-c" || id == "src-d" {
			sub.Round = 0 // force bad-format rejection for half the sources
		}
		return sub
	})
	rep4, _, err := eng.Advance()
	failIf(err)
	printReport(rep4)

	fmt.Println("\ntotal durable reports:", len(eng.Reports()))
	fmt.Println("restart with the same --store path to recover all rounds")
}

func demoConfigV1() oracle.Config {
	sources := map[string]oracle.SourceConfig{
		"src-a": {ID: "src-a", Name: "Alpha Feed", Key: "key-a-secret", Active: true},
		"src-b": {ID: "src-b", Name: "Bravo Feed", Key: "key-b-secret", Active: true},
		"src-c": {ID: "src-c", Name: "Charlie Feed", Key: "key-c-secret", Active: true},
		"src-d": {ID: "src-d", Name: "Delta Feed", Key: "key-d-secret", Active: true},
	}
	assets := map[string]oracle.AssetRule{"BTCUSD": oracle.DefaultAssetRule()}
	return oracle.DefaultConfig(1, sources, assets)
}

func demoConfigV2() oracle.Config {
	c := demoConfigV1()
	c.Version = 2
	rule := c.Assets["BTCUSD"]
	rule.MinSources = 4
	c.Assets["BTCUSD"] = rule
	return c
}

func printReport(r *oracle.RoundReport) {
	fmt.Printf("\n=== round %d [%s] config v%d closed=%s ===\n", r.Round, r.Status, r.ConfigVersion, r.ClosedAt.Format(time.RFC3339))
	for asset, d := range r.Decisions {
		price := "-"
		if d.Price != nil {
			price = fmt.Sprintf("%d micros", *d.Price)
		}
		fmt.Printf("  %s: %s price=%s reason=%s clamped=%v\n", asset, d.Status, price, d.RejectReason, d.Clamped)
		for _, c := range d.Checks {
			fmt.Printf("    - %s\n", c)
		}
	}
}

func fileDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

func failIf(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
