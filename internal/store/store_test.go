package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendFsyncAndReplay(t *testing.T) {
	// 业务：事件持久化与重放
	t.Log("业务：事件持久化 —— 追加写事件可被新句柄逐条重放，序号连续")
	dir := t.TempDir()
	st, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 3; i++ {
		if _, err := st.Append(i, 1, i*10, EvtRoundOpened, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var seen []int64
	st2, err := Open(dir, func(e Event) error {
		seen = append(seen, e.Seq)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[0] != 1 || seen[2] != 3 {
		t.Fatalf("重放序号应为 [1 2 3], 得到 %v", seen)
	}
	// 重放后继续追加，序号必须接续（模拟同一进程重放后继续工作）
	ev, err := st2.Append(4, 1, 40, EvtRoundVoided, map[string]string{"reason": "test"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 4 {
		t.Fatalf("新事件序号应为 4, 得到 %d", ev.Seq)
	}
	st2.Close()
	t.Log("用例通过: 3 条事件重放序号连续，重开后第 4 条序号正确接续")
}

func TestCorruptedLogRejected(t *testing.T) {
	// 业务：损坏日志不得被静默接受
	t.Log("业务：日志完整性 —— 序号断裂/行损坏时拒绝打开，避免在错误状态上运行")
	dir := t.TempDir()
	path := filepath.Join(dir, logFileName)
	good := `{"seq":1,"type":"round_opened","round_id":1,"config_version":1,"time_ns":1}` + "\n"
	bad := `{"seq":3,"type":"round_opened","round_id":2,"config_version":1,"time_ns":2}` + "\n"
	if err := os.WriteFile(path, []byte(good+bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, nil); err == nil {
		t.Fatal("序号断裂的日志必须导致 Open 失败")
	} else {
		t.Logf("用例通过: 断裂日志被拒绝 -> %v", err)
	}
}
