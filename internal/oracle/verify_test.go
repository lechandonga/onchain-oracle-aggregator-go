package oracle_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/oracle"
)

// TestVerifyConsistentLog: a healthy log passes every audit check.
func TestVerifyConsistentLog(t *testing.T) {
	path, _ := writeSomeRounds(t)
	store, err := oracle.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	res, err := oracle.Verify(store)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("expected OK, got problems=%v mismatches=%v", res.Problems, res.Mismatches)
	}
	if res.RoundsFinalized != 2 || res.Events == 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(res.CompletedRounds) != 2 || res.CompletedRounds[0] != 1 || res.CompletedRounds[1] != 2 {
		t.Fatalf("completed rounds not contiguous: %v", res.CompletedRounds)
	}
	t.Log("业务[自查入口] 用例[一致日志全项通过且轮次连续] 通过")
}

// tamperFinalizedPrice rewrites the recorded BTCUSD price of the given
// round's finalize event in the log file.
func tamperFinalizedPrice(t *testing.T, path string, round uint64, delta int64) {
	t.Helper()
	lines := readLines(t, path)
	for i, ln := range lines {
		var ev map[string]interface{}
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatal(err)
		}
		if ev["type"] != oracle.EvRoundFinalized || uint64(ev["round"].(float64)) != round {
			continue
		}
		rep := ev["report"].(map[string]interface{})
		dec := rep["decisions"].(map[string]interface{})["BTCUSD"].(map[string]interface{})
		dec["price_micros"] = dec["price_micros"].(float64) + float64(delta)
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		lines[i] = string(data)
		writeLines(t, path, lines)
		return
	}
	t.Fatalf("no finalize event for round %d", round)
}

// TestVerifyDetectsTampering: a doctored recorded result is caught and
// attributed to the exact round.
func TestVerifyDetectsTampering(t *testing.T) {
	path, good := writeSomeRounds(t)
	tamperFinalizedPrice(t, path, 1, 1000)

	store, err := oracle.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	res, err := oracle.Verify(store)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Fatal("tampered log must not pass verification")
	}
	if len(res.Mismatches) == 0 || res.Mismatches[0].Round != 1 {
		t.Fatalf("mismatch must point at round 1, got %+v", res.Mismatches)
	}
	joined := strings.Join(res.Mismatches[0].Diffs, "; ")
	if !strings.Contains(joined, "price") {
		t.Fatalf("diff should mention the price field: %s", joined)
	}
	if !strings.Contains(joined, "1000") && !strings.Contains(joined, itoa(good+1000)) {
		t.Fatalf("diff should show recorded vs recomputed values: %s", joined)
	}
	t.Logf("业务[自查入口] 用例[篡改轮次1结果被定位: %s] 通过", joined)
}

func itoa(p oracle.Price) string {
	return strings.TrimSpace(jsonNumber(int64(p)))
}

func jsonNumber(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestVerifyDetectsDuplicatedRound: a replayed finalize record is flagged as
// a structural problem.
func TestVerifyDetectsDuplicatedRound(t *testing.T) {
	path, _ := writeSomeRounds(t)
	lines := readLines(t, path)
	// Append a second copy of round 1's finalize event with a fresh seq.
	var dup string
	maxSeq := uint64(0)
	for _, ln := range lines {
		var ev map[string]interface{}
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatal(err)
		}
		if s := uint64(ev["seq"].(float64)); s > maxSeq {
			maxSeq = s
		}
		if ev["type"] == oracle.EvRoundFinalized && uint64(ev["round"].(float64)) == 1 {
			dup = ln
		}
	}
	var ev map[string]interface{}
	if err := json.Unmarshal([]byte(dup), &ev); err != nil {
		t.Fatal(err)
	}
	ev["seq"] = float64(maxSeq + 1)
	b, _ := json.Marshal(ev)
	lines = append(lines, string(b))
	writeLines(t, path, lines)

	store, err := oracle.OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	res, err := oracle.Verify(store)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Fatal("duplicated finalize must not pass verification")
	}
	joined := strings.Join(res.Problems, "; ")
	if !strings.Contains(joined, "round 1") {
		t.Fatalf("problem should identify round 1: %s", joined)
	}
	t.Logf("业务[自查入口] 用例[重复定稿被识别: %s] 通过", joined)
}
