package auth

import (
	"errors"
	"testing"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/config"
)

func testCfg() *config.Config {
	return &config.Config{
		Version: 1,
		Pair:    "BTC/USD",
		Sources: map[string]config.Source{
			"src-a": {ID: "src-a", Secret: "sa", Weight: 1, NotAfterRFC3339: "2025-01-02T00:00:00Z"},
			"src-b": {ID: "src-b", Secret: "sb", Weight: 1},
		},
		Rule: config.AggregationRule{MinSources: 1, InsufficientPolicy: config.PolicyReject, DeviationPolicy: config.PolicyReject},
	}
}

func TestAuthenticateBusinessCases(t *testing.T) {
	// 业务：数据源提交鉴权
	t.Log("业务：数据源提交鉴权 —— 覆盖合法提交、越权、签名伪造、身份过期与明显非法格式")
	cfg := testCfg()
	v := NewValidator(cfg)
	const now = int64(1735776000000000000) // 2025-01-01T12:00:00Z，在 src-a 有效期内

	base := func() Submission {
		s := Submission{SourceID: "src-a", RoundID: 1, Price: 100, Timestamp: now - 1, Nonce: "n1"}
		s.Sig = Sign("sa", s)
		return s
	}

	cases := []struct {
		name   string
		mutate func(*Submission)
		reason RejectReason
		ok     bool
	}{
		{"合法签名提交不得误伤", func(s *Submission) {}, "", true},
		{"来源未登记应拒绝(unauthorized)", func(s *Submission) { s.SourceID = "ghost"; s.Sig = Sign("sa", *s) }, ReasonUnauthorized, false},
		{"签名密钥错误应拒绝(unauthorized)", func(s *Submission) { s.Sig = Sign("wrong", *s) }, ReasonUnauthorized, false},
		{"签名内容被篡改应拒绝(unauthorized)", func(s *Submission) {
			s.Price = 101
			s.Sig = Sign("sa", Submission{SourceID: s.SourceID, RoundID: s.RoundID, Price: 100, Timestamp: s.Timestamp, Nonce: s.Nonce})
		}, ReasonUnauthorized, false},
		{"身份超过有效期应拒绝(expired)", func(s *Submission) {}, ReasonExpired, false},
		{"空来源ID应拒绝(malformed)", func(s *Submission) { s.SourceID = "" }, ReasonMalformed, false},
		{"非法轮次号应拒绝(malformed)", func(s *Submission) { s.RoundID = 0 }, ReasonMalformed, false},
		{"零/负价格应拒绝(malformed)", func(s *Submission) { s.Price = 0 }, ReasonMalformed, false},
		{"空nonce应拒绝(malformed)", func(s *Submission) { s.Nonce = "" }, ReasonMalformed, false},
		{"未来时间戳应拒绝(future_quote)", func(s *Submission) {
			s.Timestamp = now + 100
			s.Sig = Sign("sa", *s)
		}, ReasonFutureQuote, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base()
			tc.mutate(&s)
			ts := now
			if tc.name == "身份超过有效期应拒绝(expired)" {
				ts = 1735776000000000000 + 0
				// 推进到 src-a 失效时刻之后
				ts = 1735776001000000000
			}
			err := v.Authenticate(s, ts, 10)
			if tc.ok {
				if err != nil {
					t.Fatalf("合法提交被误伤: %v", err)
				}
				t.Logf("用例通过: 合法提交接受")
				return
			}
			var re *RejectError
			if !errors.As(err, &re) {
				t.Fatalf("期望 RejectError, got %v", err)
			}
			if re.Reason != tc.reason {
				t.Fatalf("期望原因 %s, 实际 %s (%s)", tc.reason, re.Reason, re.Detail)
			}
			t.Logf("用例通过: 拒绝原因=%s 说明=%s", re.Reason, re.Detail)
		})
	}
}

func TestSigningDeterminism(t *testing.T) {
	// 业务：签名规范串确定性
	t.Log("业务：签名规范串确定性 —— 同一信封重复签名结果一致")
	s := Submission{SourceID: "x", RoundID: 9, Price: 1.23, Timestamp: 42, Nonce: "n"}
	if Sign("k", s) != Sign("k", s) {
		t.Fatal("签名不确定")
	}
	if Sign("k", s) == Sign("other", s) {
		t.Fatal("不同密钥签名不应相同")
	}
}
