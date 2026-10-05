// Package auth 负责报价提交的格式校验、身份鉴权与有效期检查。
// 它对应真实系统中“数据源用私钥对报价签名，链上合约用已登记身份验签”
// 的部分，本地用 HMAC-SHA256 共享密钥模拟。
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/lechandonga/onchain-oracle-aggregator-go/internal/config"
)

// RejectReason 是可区分的拒绝原因，便于审计与测试断言。
type RejectReason string

const (
	ReasonMalformed    RejectReason = "malformed"    // 字段缺失/类型明显非法
	ReasonUnauthorized RejectReason = "unauthorized" // 来源未登记或签名错误
	ReasonExpired      RejectReason = "expired"      // 身份不在有效期内
	ReasonStaleQuote   RejectReason = "stale_quote"  // 报价时间戳过旧（由引擎在关轮时判）
	ReasonFutureQuote  RejectReason = "future_quote" // 报价时间戳明显在未来
	ReasonDuplicate    RejectReason = "duplicate"    // 同一轮次同一来源重复提交
	ReasonClosedRound  RejectReason = "closed_round" // 轮次已关闭/作废
)

// RejectError 携带机器可读原因与人类可读说明。
type RejectError struct {
	Reason RejectReason
	Detail string
}

func (e *RejectError) Error() string { return string(e.Reason) + ": " + e.Detail }

// Is 让 errors.Is 可按原因匹配。
func (e *RejectError) Is(target error) bool {
	t, ok := target.(*RejectError)
	return ok && t.Reason == e.Reason
}

// Submission 是来源提交的报价信封。
type Submission struct {
	SourceID  string  `json:"source_id"`
	RoundID   int64   `json:"round_id"`
	Price     float64 `json:"price"`
	Timestamp int64   `json:"timestamp_ns"` // 来源声称的报价采集时刻（Unix 纳秒）
	Nonce     string  `json:"nonce"`        // 报价唯一标识，重放检测使用
	Sig       string  `json:"sig"`          // hex(HMAC-SHA256(secret, canonical))
}

// CanonicalString 是被签名的确定性字节串，字段以 \n 分隔。
func (s Submission) CanonicalString() string {
	return strings.Join([]string{
		s.SourceID,
		fmt.Sprintf("%d", s.RoundID),
		strconvFormatFloat(s.Price),
		fmt.Sprintf("%d", s.Timestamp),
		s.Nonce,
	}, "\n")
}

func strconvFormatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Sign 用来源密钥生成签名（供模拟数据源使用）。
func Sign(secret string, s Submission) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(s.CanonicalString()))
	return hex.EncodeToString(mac.Sum(nil))
}

// SignBase64 兼容以 base64 携带签名的来源。
func SignBase64(secret string, s Submission) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(s.CanonicalString()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Validator 基于一份不可变配置校验提交。
type Validator struct {
	cfg *config.Config
}

func NewValidator(cfg *config.Config) *Validator { return &Validator{cfg: cfg} }

func reject(r RejectReason, format string, args ...any) error {
	return &RejectError{Reason: r, Detail: fmt.Sprintf(format, args...)}
}

// ValidateFormat 只做与身份无关的明显格式检查。
func ValidateFormat(s Submission) error {
	if s.SourceID == "" {
		return reject(ReasonMalformed, "source_id is empty")
	}
	if s.RoundID <= 0 {
		return reject(ReasonMalformed, "round_id must be positive, got %d", s.RoundID)
	}
	if s.Nonce == "" {
		return reject(ReasonMalformed, "nonce is empty")
	}
	if math.IsNaN(s.Price) || math.IsInf(s.Price, 0) || s.Price <= 0 {
		return reject(ReasonMalformed, "price must be a finite positive number, got %v", s.Price)
	}
	if s.Timestamp <= 0 {
		return reject(ReasonMalformed, "timestamp_ns must be positive, got %d", s.Timestamp)
	}
	if s.Sig == "" {
		return reject(ReasonMalformed, "signature is empty")
	}
	return nil
}

// Authenticate 校验格式、身份、签名、身份有效期与报价时间戳合理性。
// nowNS 是引擎当前时间；futureToleranceNS 是允许的时钟偏移。
func (v *Validator) Authenticate(s Submission, nowNS, futureToleranceNS int64) error {
	if err := ValidateFormat(s); err != nil {
		return err
	}
	src, ok := v.cfg.Sources[s.SourceID]
	if !ok {
		return reject(ReasonUnauthorized, "source %q not registered in config v%d", s.SourceID, v.cfg.Version)
	}
	if !verifySig(src.Secret, s) {
		return reject(ReasonUnauthorized, "signature mismatch for source %q", s.SourceID)
	}
	if src.NotBeforeRFC3339 != "" {
		t, err := time.Parse(time.RFC3339Nano, src.NotBeforeRFC3339)
		if err != nil {
			return reject(ReasonUnauthorized, "invalid not_before for source %q: %v", s.SourceID, err)
		}
		if nowNS < t.UnixNano() {
			return reject(ReasonExpired, "source %q credential not valid yet", s.SourceID)
		}
	}
	if src.NotAfterRFC3339 != "" {
		t, err := time.Parse(time.RFC3339Nano, src.NotAfterRFC3339)
		if err != nil {
			return reject(ReasonUnauthorized, "invalid not_after for source %q: %v", s.SourceID, err)
		}
		if nowNS > t.UnixNano() {
			return reject(ReasonExpired, "source %q credential expired at %s", s.SourceID, src.NotAfterRFC3339)
		}
	}
	if s.Timestamp > nowNS+futureToleranceNS {
		return reject(ReasonFutureQuote, "quote timestamp %d is more than %d ns in the future (now %d)", s.Timestamp, futureToleranceNS, nowNS)
	}
	return nil
}

func verifySig(secret string, s Submission) bool {
	want := hmac.New(sha256.New, []byte(secret))
	want.Write([]byte(s.CanonicalString()))
	got, err := hex.DecodeString(s.Sig)
	if err != nil {
		// 允许 base64 签名作为备选编码。
		if b64, err2 := base64.StdEncoding.DecodeString(s.Sig); err2 == nil {
			got = b64
		} else {
			return false
		}
	}
	return subtle.ConstantTimeCompare(got, want.Sum(nil)) == 1
}
