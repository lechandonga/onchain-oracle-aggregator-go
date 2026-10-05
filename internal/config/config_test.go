package config

import "testing"

func TestHistoryVersioning(t *testing.T) {
	// 业务：配置版本演进约束
	t.Log("业务：配置版本演进 —— 版本必须递增、禁止换交易对、历史版本可取回复现")
	v1 := &Config{Version: 1, Pair: "BTC/USD", Sources: map[string]Source{
		"a": {ID: "a", Secret: "x", Weight: 1},
	}, Rule: AggregationRule{MinSources: 1, InsufficientPolicy: PolicyReject, DeviationPolicy: PolicyReject}}
	h, err := NewHistory(v1)
	if err != nil {
		t.Fatal(err)
	}

	v2 := &Config{Version: 2, Pair: "BTC/USD", Sources: map[string]Source{
		"a": {ID: "a", Secret: "x", Weight: 1},
		"b": {ID: "b", Secret: "y", Weight: 1},
	}, Rule: AggregationRule{MinSources: 1, InsufficientPolicy: PolicyReject, DeviationPolicy: PolicyReject}}
	if err := h.AddVersion(v2); err != nil {
		t.Fatal(err)
	}
	if got := h.Latest().Version; got != 2 {
		t.Fatalf("最新版本应为 2, 得到 %d", got)
	}
	if c, ok := h.Version(1); !ok || len(c.Sources) != 1 {
		t.Fatal("历史 v1 应可取回且保持原样")
	}
	if err := h.AddVersion(v2); err == nil {
		t.Fatal("重复/回退版本号必须被拒绝")
	}
	v3 := &Config{Version: 3, Pair: "ETH/USD", Sources: map[string]Source{
		"a": {ID: "a", Secret: "x", Weight: 1},
	}, Rule: AggregationRule{MinSources: 1, InsufficientPolicy: PolicyReject, DeviationPolicy: PolicyReject}}
	if err := h.AddVersion(v3); err == nil {
		t.Fatal("升级更换交易对必须被拒绝")
	}
	t.Log("用例通过: v1->v2 生效、v1 可复现取回、重复版本与换交易对被拒绝")
}

func TestInvalidConfigs(t *testing.T) {
	// 业务：非法配置不得进入系统
	t.Log("业务：配置自校验 —— 空来源、非法权重、min_sources 越界等必须报错")
	base := func() *Config {
		return &Config{Version: 1, Pair: "BTC/USD", Sources: map[string]Source{
			"a": {ID: "a", Secret: "x", Weight: 1},
		}, Rule: AggregationRule{MinSources: 1, InsufficientPolicy: PolicyReject, DeviationPolicy: PolicyReject}}
	}
	bad := func(mut func(*Config)) error {
		c := base()
		mut(c)
		return c.Validate()
	}
	if err := bad(func(c *Config) { c.Version = 0 }); err == nil {
		t.Error("version=0 应非法")
	}
	if err := bad(func(c *Config) { c.Sources["a"] = Source{ID: "a", Weight: 1} }); err == nil {
		t.Error("空密钥应非法")
	}
	if err := bad(func(c *Config) { c.Sources["a"] = Source{ID: "a", Secret: "x", Weight: 0} }); err == nil {
		t.Error("零权重应非法")
	}
	if err := bad(func(c *Config) { c.Rule.MinSources = 2 }); err == nil {
		t.Error("min_sources 超过来源数应非法")
	}
	if err := bad(func(c *Config) { c.Rule.InsufficientPolicy = "bogus" }); err == nil {
		t.Error("未知降级策略应非法")
	}
	t.Log("用例通过: 版本号/密钥/权重/min_sources/策略等非法配置均被拒绝")
}
