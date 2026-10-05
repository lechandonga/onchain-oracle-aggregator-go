package oracle_test

import (
	"path/filepath"
	"testing"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/oracle"
)

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := oracle.LoadConfigFile(filepath.Join("..", "..", "configs", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 1 || len(cfg.Sources) != 4 || len(cfg.Assets) != 1 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.SubmissionTTL <= 0 || cfg.FallbackStaleness <= 0 || cfg.MaxQuoteStaleness <= 0 {
		t.Fatal("durations did not decode from nanosecond integers")
	}
	t.Log("业务[配置] 用例[示例配置文件可加载且时长字段正确解码] 通过")
}
