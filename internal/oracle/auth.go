package oracle

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SigningMessage builds the canonical byte string that is signed. It contains
// every field that matters to quote validity, so none of them can be tampered
// with independently. Fields are joined with '|' which is forbidden in IDs.
func SigningMessage(sourceID, asset string, round uint64, price Price, quoteTime time.Time, nonce string) string {
	return strings.Join([]string{
		"v1",
		sourceID,
		asset,
		strconv.FormatUint(round, 10),
		strconv.FormatInt(int64(price), 10),
		strconv.FormatInt(quoteTime.UnixNano(), 10),
		nonce,
	}, "|")
}

// Sign produces the hex HMAC-SHA256 signature a data source puts on a quote.
func Sign(key, sourceID, asset string, round uint64, price Price, quoteTime time.Time, nonce string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(SigningMessage(sourceID, asset, round, price, quoteTime, nonce)))
	return hex.EncodeToString(mac.Sum(nil))
}

func validID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// ValidateFormat checks fields independent of configuration/state.
func (s Submission) ValidateFormat() error {
	if !validID(s.SourceID) {
		return &RejectError{Reason: RejectBadFormat, Detail: "invalid source id"}
	}
	if !validID(s.Asset) {
		return &RejectError{Reason: RejectBadFormat, Detail: "invalid asset id"}
	}
	if s.Round == 0 {
		return &RejectError{Reason: RejectBadFormat, Detail: "round must be positive"}
	}
	if s.Price <= 0 {
		return &RejectError{Reason: RejectBadFormat, Detail: "price must be positive"}
	}
	if s.QuoteTime.IsZero() {
		return &RejectError{Reason: RejectBadFormat, Detail: "quote time missing"}
	}
	if s.Nonce == "" || len(s.Nonce) > 128 {
		return &RejectError{Reason: RejectBadFormat, Detail: "nonce missing or too long"}
	}
	if len(s.Sig) != sha256.Size*2 {
		return &RejectError{Reason: RejectBadFormat, Detail: "malformed signature"}
	}
	return nil
}

func verifySignature(key string, s Submission) error {
	got, err := hex.DecodeString(s.Sig)
	if err != nil {
		return &RejectError{Reason: RejectBadSignature, Detail: "signature is not hex"}
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(SigningMessage(s.SourceID, s.Asset, s.Round, s.Price, s.QuoteTime, s.Nonce)))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return &RejectError{Reason: RejectBadSignature, Detail: fmt.Sprintf("source %s", s.SourceID)}
	}
	return nil
}
