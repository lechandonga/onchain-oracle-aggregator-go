// Command oracleverify audits a durable event log: it checks record
// integrity (consecutive sequence numbers, no corrupt records), confirms
// that completed rounds are neither lost, duplicated nor skipped, and
// recomputes every finalized round from the recorded inputs and rules,
// comparing the outcome against the recorded report round by round.
//
// Usage:
//
//	go run ./cmd/oracleverify -store data/oracle-events.jsonl
//
// Exit code is 0 only when every check passes.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/oracle"
)

func main() {
	storePath := flag.String("store", "data/oracle-events.jsonl", "event log path to audit")
	flag.Parse()

	store, err := oracle.OpenFileStore(*storePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: cannot open log: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()
	if n := store.TruncatedTailBytes(); n > 0 {
		fmt.Printf("note: truncated %d byte(s) of unconfirmed torn tail (killed mid-write, never acknowledged)\n", n)
	}

	res, err := oracle.Verify(store)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("audited %d event(s): %d finalized round(s), %d voided round(s)\n",
		res.Events, res.RoundsFinalized, res.RoundsVoided)
	for _, p := range res.Problems {
		fmt.Printf("  PROBLEM: %s\n", p)
	}
	for _, m := range res.Mismatches {
		fmt.Printf("  MISMATCH round %d:\n", m.Round)
		for _, d := range m.Diffs {
			fmt.Printf("    - %s\n", d)
		}
	}
	if !res.OK() {
		fmt.Println("RESULT: FAIL")
		os.Exit(1)
	}
	fmt.Println("RESULT: OK (record integrity, round continuity and every recomputed report match the log)")
}
