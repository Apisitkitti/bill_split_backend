package line

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"

	"github.com/OatApisit/billsplit-api/internal/money"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestValidateSignature(t *testing.T) {
	const secret = "channel-secret"
	body := []byte(`{"events":[{"type":"message"}]}`)
	good := sign(secret, body)

	if !ValidateSignature(secret, body, good) {
		t.Error("a correctly signed request was rejected")
	}

	tests := []struct {
		name      string
		secret    string
		body      []byte
		signature string
	}{
		{"wrong secret", "other-secret", body, good},
		{"tampered body", secret, []byte(`{"events":[{"type":"unfollow"}]}`), good},
		{"empty signature", secret, body, ""},
		{"garbage signature", secret, body, "not-base64!!"},
		{"truncated signature", secret, body, good[:len(good)-1]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if ValidateSignature(tt.secret, tt.body, tt.signature) {
				t.Error("forged request was accepted")
			}
		})
	}
}

// Whitespace differences change the signature, which is why the raw body must
// be validated before anything re-encodes it.
func TestValidateSignatureIsByteExact(t *testing.T) {
	const secret = "channel-secret"
	original := []byte(`{"a":1}`)
	reencoded := []byte(`{"a": 1}`)

	if ValidateSignature(secret, reencoded, sign(secret, original)) {
		t.Error("a re-encoded body validated against the original signature")
	}
}

func TestSettlementFlexAltText(t *testing.T) {
	lines := []SettlementLine{
		{FromName: "Bob", ToName: "Alice", Amount: money.Satang(3333)},
	}

	msg := SettlementFlex("ทริปเชียงใหม่", lines, "https://liff.line.me/x")
	alt, ok := msg["altText"].(string)
	if !ok || alt == "" {
		t.Fatal("message has no altText; it would be blank in the chat list")
	}
	if msg["type"] != "flex" {
		t.Errorf("type = %v, want flex", msg["type"])
	}

	// A settled group still needs a message, and it must not claim there are
	// payments to make.
	empty := SettlementFlex("ทริปเชียงใหม่", nil, "")
	emptyAlt, _ := empty["altText"].(string)
	if emptyAlt == alt {
		t.Error("a settled group produced the same altText as an unsettled one")
	}
	if _, hasFooter := empty["contents"].(map[string]any)["footer"]; hasFooter {
		t.Error("a bubble with no LIFF URL should have no footer button")
	}
}
