package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"sync"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/auth"
	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/config"
	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/oracle"
)

var demoCounter int

func newSub(cfg *config.Config, roundID int64, sourceID string, price float64, tsNS int64) auth.Submission {
	demoCounter++
	s := auth.Submission{
		SourceID: sourceID, RoundID: roundID, Price: price,
		Timestamp: tsNS, Nonce: "n-" + strconv.Itoa(demoCounter),
	}
	s.Sig = auth.Sign(cfg.Sources[sourceID].Secret, s)
	return s
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	dataDir := fs.String("data-dir", "./data/oracle-demo", "event log directory (cleaned before run)")
	cfgDir := fs.String("config-dir", "./configs", "config directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.RemoveAll(*dataDir); err != nil {
		return err
	}

	h, err := loadHistory(*cfgDir, 1)
	if err != nil {
		return err
	}

	const second = int64(1_000_000_000)
	var t int64 = 1_000 * second

	fmt.Println("=== 剧情 0：启动引擎，自动开启 round 1（配置 v1）===")
	eng, err := oracle.New(oracle.Options{DataDir: *dataDir, History: h,
		Recovery: oracle.RecoveryVoid, FutureToleranceNS: 500 * 1_000_000})
	if err != nil {
		return err
	}
	cfg1 := h.Latest()
	r1 := eng.CurrentRoundID()
	fmt.Printf("open round = %d, config = v%d\n", r1, eng.CurrentConfigVersion())

	fmt.Println("=== 剧情 1：4 个来源乱序并发提交（d,a,c,b），只应各计入一次 ===")
	prices := map[string]float64{"src-a": 100, "src-b": 99.5, "src-c": 100.5, "src-d": 101}
	order := []string{"src-d", "src-a", "src-c", "src-b"}
	var wg sync.WaitGroup
	for _, id := range order {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := eng.Submit(newSub(cfg1, r1, id, prices[id], t), t); err != nil {
				fmt.Printf("   提交 %s 被拒: %v\n", id, err)
			} else {
				fmt.Printf("   提交 %s @ %.2f 已接受\n", id, prices[id])
			}
		}(id)
	}
	wg.Wait()

	fmt.Println("=== 剧情 2：重复提交、伪造来源、签名错误、负价格、未来时间戳均应拒绝 ===")
	tryReject := func(label string, s auth.Submission, now int64) {
		err := eng.Submit(s, now)
		fmt.Printf("   %-22s => %v\n", label, err)
	}
	tryReject("src-a 重复提交", newSub(cfg1, r1, "src-a", 100.1, t), t)
	tryReject("未登记来源 evil", newSub(cfg1, r1, "evil", 100, t), t)
	bad := newSub(cfg1, r1, "src-b", 99.5, t)
	bad.Sig = "deadbeef"
	tryReject("src-b 签名错误", bad, t)
	neg := newSub(cfg1, r1, "src-b", -1, t)
	tryReject("负价格", neg, t)
	fut := newSub(cfg1, r1, "src-b", 99.5, t+10*second)
	tryReject("报价时间戳在未来", fut, t)

	t += 10 * second
	res1, err := eng.CloseRound(t, true)
	if err != nil {
		return err
	}
	printResult(res1)
	r2 := eng.CurrentRoundID()

	fmt.Println("=== 剧情 3：只有 2 个新鲜来源 -> 不足，回退到 round 1 价格 ===")
	eng.Submit(newSub(cfg1, r2, "src-a", 100, t+1), t+1)
	eng.Submit(newSub(cfg1, r2, "src-b", 100, t+1), t+1)
	t += 10 * second
	res2, _ := eng.CloseRound(t, true)
	printResult(res2)
	r3 := eng.CurrentRoundID()

	fmt.Println("=== 剧情 4：首轮报价陈旧（超出 30s 新鲜窗口）-> 同样回退 ===")
	eng.Submit(newSub(cfg1, r3, "src-a", 100, t-40*second), t)
	eng.Submit(newSub(cfg1, r3, "src-b", 100, t-40*second), t)
	eng.Submit(newSub(cfg1, r3, "src-c", 100, t-40*second), t)
	t += 10 * second
	res3, _ := eng.CloseRound(t, true)
	printResult(res3)
	r4 := eng.CurrentRoundID()

	fmt.Println("=== 剧情 5：3 个来源串通报价 500，被硬价格带钳制到上限 110 ===")
	eng.Submit(newSub(cfg1, r4, "src-a", 500, t+1), t+1)
	eng.Submit(newSub(cfg1, r4, "src-b", 500, t+1), t+1)
	eng.Submit(newSub(cfg1, r4, "src-c", 500, t+1), t+1)
	t += 10 * second
	res4, _ := eng.CloseRound(t, true)
	printResult(res4)
	r5 := eng.CurrentRoundID()

	fmt.Println("=== 剧情 6：升级到配置 v2（新增 src-e、偏差阈值更严）；v2 只对新轮次生效 ===")
	cfg2, err := config.LoadFile(*cfgDir + "/v2.json")
	if err != nil {
		return err
	}
	if err := eng.Upgrade(cfg2); err != nil {
		return err
	}
	fmt.Printf("   当前仍开着的 round %d 冻结在配置 v%d\n", r5, eng.CurrentConfigVersion())
	eng.Submit(newSub(cfg2, r5, "src-a", 100, t+1), t+1)
	eng.Submit(newSub(cfg2, r5, "src-b", 100, t+1), t+1)
	eng.Submit(newSub(cfg2, r5, "src-c", 100, t+1), t+1)
	eng.Submit(newSub(cfg2, r5, "src-d", 100, t+1), t+1)
	t += 10 * second
	res5, _ := eng.CloseRound(t, true)
	printResult(res5)
	r6 := eng.CurrentRoundID()
	fmt.Printf("   升级后新轮次 round %d 使用配置 v%d；历史 round 1 仍记录为 v%d\n",
		r6, eng.CurrentConfigVersion(), eng.Result(r1).ConfigVer)

	fmt.Println("=== 剧情 7：v2 下报价偏离 1%（>100bps），策略=回退到上一有效价格 ===")
	eng.Submit(newSub(cfg2, r6, "src-a", 101.5, t+1), t+1)
	eng.Submit(newSub(cfg2, r6, "src-b", 101.5, t+1), t+1)
	eng.Submit(newSub(cfg2, r6, "src-c", 101.5, t+1), t+1)
	t += 10 * second
	res6, _ := eng.CloseRound(t, true)
	printResult(res6)

	fmt.Println("=== 剧情 8：模拟崩溃：引擎关闭后用同一数据目录重启 ===")
	if err := eng.Close(); err != nil {
		return err
	}
	// 重启时历史配置必须完整提供，引擎按日志里记录的版本号解释每一轮。
	h2, err := loadHistory(*cfgDir, 1, 2)
	if err != nil {
		return err
	}
	eng2, err := oracle.New(oracle.Options{DataDir: *dataDir, History: h2, Recovery: oracle.RecoveryVoid})
	if err != nil {
		return err
	}
	for id := int64(1); id <= 6; id++ {
		r := eng2.Result(id)
		if r != nil {
			fmt.Printf("   恢复 round %d: status=%s price=%.6f config=v%d\n",
				id, r.Status, r.Price, r.ConfigVer)
		}
	}
	last, price := eng2.LastValid()
	fmt.Printf("   重启后当前轮次=%d，最近有效价格来自 round %d = %.6f（未丢失、未重复发布）\n",
		eng2.CurrentRoundID(), last, price)
	return eng2.Close()
}
