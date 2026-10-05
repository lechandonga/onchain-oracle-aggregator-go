// 本地可运行的预言机聚合演示/校验命令。
//
// 子命令：
//
//	demo      运行内置剧情（正常轮、重复/越权/过期/乱序提交、来源不足回退、
//	           串通价格带钳制、配置升级、崩溃重启恢复），打印每轮判定依据
//	inspect   读取数据目录下的事件日志，重放并打印每轮终局结果
//	verify    校验指定轮次：用日志中记录的配置版本与提交，可复现该轮结论
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/auth"
	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/config"
	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/oracle"
	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo(os.Args[2:])
	case "inspect":
		err = runInspect(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: oracled <demo|inspect|verify> [flags]")
	os.Exit(2)
}

func loadHistory(dir string, versions ...int64) (*config.History, error) {
	var cfgs []*config.Config
	for _, v := range versions {
		c, err := config.LoadFile(filepath.Join(dir, fmt.Sprintf("v%d.json", v)))
		if err != nil {
			return nil, err
		}
		cfgs = append(cfgs, c)
	}
	return config.NewHistory(cfgs...)
}

func printResult(r *oracle.RoundResult) {
	fmt.Printf("── round %d  status=%s  config=v%d  pair=%s\n", r.RoundID, r.Status, r.ConfigVer, r.Pair)
	if r.Status == oracle.StatusPublished || r.Status == oracle.StatusFallback {
		extra := ""
		if r.DerivedFrom != 0 {
			extra = fmt.Sprintf(" (derived from round %d, reason=%s)", r.DerivedFrom, r.Reason)
		}
		fmt.Printf("   price=%.6f%s\n", r.Price, extra)
	} else {
		fmt.Printf("   no price published, reason=%s\n", r.Reason)
	}
	for _, c := range r.Checks {
		mark := "PASS"
		if !c.Passed {
			mark = "FAIL"
		}
		fmt.Printf("   [%s] %-14s %s\n", mark, c.Check, c.Detail)
	}
}

func runInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	dataDir := fs.String("data-dir", "./data/oracle", "event log directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	events, err := store.ReadAll(*dataDir)
	if err != nil {
		return err
	}
	for _, ev := range events {
		switch ev.Type {
		case store.EvtRoundConfirmed, store.EvtRoundRejected, store.EvtRoundVoided:
			var r oracle.RoundResult
			if err := json.Unmarshal(ev.Payload, &r); err != nil {
				return err
			}
			printResult(&r)
		case store.EvtRoundOpened:
			fmt.Printf(".. round %d opened (config v%d) at %d\n", ev.RoundID, ev.ConfigVer, ev.TimeNS)
		case store.EvtSubmission:
			var s auth.Submission
			if err := json.Unmarshal(ev.Payload, &s); err != nil {
				return err
			}
			fmt.Printf(".. round %d accepted submission from %s @ %.4f\n", ev.RoundID, s.SourceID, s.Price)
		}
	}
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dataDir := fs.String("data-dir", "./data/oracle", "event log directory")
	cfgDir := fs.String("config-dir", "./configs", "config directory")
	round := fs.Int64("round", 1, "round id to verify")
	if err := fs.Parse(args); err != nil {
		return err
	}
	events, err := store.ReadAll(*dataDir)
	if err != nil {
		return err
	}
	var stored *oracle.RoundResult
	for _, ev := range events {
		if ev.RoundID == *round && (ev.Type == store.EvtRoundConfirmed || ev.Type == store.EvtRoundRejected) {
			var r oracle.RoundResult
			if err := json.Unmarshal(ev.Payload, &r); err != nil {
				return err
			}
			stored = &r
		}
	}
	if stored == nil {
		return fmt.Errorf("round %d not found in log", *round)
	}
	verifyDir := filepath.Join(*dataDir, "_verify_tmp")
	if err := os.RemoveAll(verifyDir); err != nil {
		return err
	}
	defer os.RemoveAll(verifyDir)
	// 收集配置目录下日志引用过的全部版本（按版本号升序），
	// 从 round 1 起逐轮重放到目标轮：回退参照、升级时点随之自然复现。
	versions := map[int64]bool{}
	for _, ev := range events {
		versions[ev.ConfigVer] = true
	}
	var vers []int64
	for v := range versions {
		vers = append(vers, v)
	}
	sortInts(vers)
	cfgByVersion := map[int64]*config.Config{}
	for _, v := range vers {
		c, err := config.LoadFile(filepath.Join(*cfgDir, fmt.Sprintf("v%d.json", v)))
		if err != nil {
			return fmt.Errorf("load config v%d: %w", v, err)
		}
		cfgByVersion[v] = c
	}
	first := cfgByVersion[vers[0]]
	h, err := config.NewHistory(first)
	if err != nil {
		return err
	}
	eng, err := oracle.New(oracle.Options{DataDir: verifyDir, History: h, Recovery: oracle.RecoveryVoid})
	if err != nil {
		return err
	}
	defer eng.Close()

	// 从事件日志按轮抽取提交与关轮时刻，逐轮重放。
	subsByRound := map[int64][]auth.Submission{}
	var closeNS map[int64]int64 = map[int64]int64{}
	verByRound := map[int64]int64{}
	for _, ev := range events {
		switch ev.Type {
		case store.EvtRoundOpened:
			verByRound[ev.RoundID] = ev.ConfigVer
		case store.EvtSubmission:
			var s auth.Submission
			if err := json.Unmarshal(ev.Payload, &s); err != nil {
				return err
			}
			subsByRound[ev.RoundID] = append(subsByRound[ev.RoundID], s)
		case store.EvtRoundConfirmed, store.EvtRoundRejected, store.EvtRoundVoided:
			var r oracle.RoundResult
			if err := json.Unmarshal(ev.Payload, &r); err != nil {
				return err
			}
			closeNS[ev.RoundID] = r.CloseNS
		}
	}
	for id := int64(1); id <= *round; id++ {
		// 升级必须发生在“该轮开启之前”：上一轮关闭时不自动开新轮，
		// 登记本轮版本后再显式开轮，精确复刻升级时点。
		if id > 1 && eng.IsOpen() {
			if _, err := eng.CloseRound(closeNS[id-1], false); err != nil {
				return err
			}
		}
		if c := cfgByVersion[verByRound[id]]; c != nil {
			_ = h.AddVersion(c) // 已存在的版本会返回错误，忽略
		}
		if id > 1 {
			if _, err := eng.OpenRound(0); err != nil {
				return err
			}
		}
		for _, s := range subsByRound[id] {
			if err := eng.Submit(s, s.Timestamp); err != nil {
				return fmt.Errorf("replay round %d submission %s: %w", id, s.SourceID, err)
			}
		}
		if eng.IsOpen() {
			got, err := eng.CloseRound(closeNS[id], false)
			if err != nil {
				return err
			}
			if id == *round {
				if got.Status != stored.Status || got.Price != stored.Price {
					return fmt.Errorf("REPRODUCIBILITY MISMATCH at round %d: stored status=%s price=%.6f, recomputed status=%s price=%.6f",
						id, stored.Status, stored.Price, got.Status, got.Price)
				}
				fmt.Printf("OK: round %d reproduced exactly (status=%s price=%.6f) using config v%d\n",
					id, got.Status, got.Price, stored.ConfigVer)
				return nil
			}
		}
	}
	return fmt.Errorf("round %d was voided or never finalized, nothing to reproduce", *round)
}

func sortInts(s []int64) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
