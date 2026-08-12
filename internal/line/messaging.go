package line

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/OatApisit/billsplit-api/internal/money"
)

const pushEndpoint = "https://api.line.me/v2/bot/message/push"

// Messenger pushes messages through the Messaging API. Its channel access
// token belongs to the Messaging API channel, which is a different channel
// from the LINE Login one used by Verifier.
type Messenger struct {
	AccessToken string
	HTTP        *http.Client
}

// NewMessenger returns a Messenger using the given channel access token.
func NewMessenger(accessToken string) *Messenger {
	return &Messenger{
		AccessToken: accessToken,
		HTTP:        &http.Client{Timeout: 10 * time.Second},
	}
}

// Push sends messages to a user, group, or room ID.
//
// LINE bills for pushes and rejects a bad target, so callers should prefer
// replying to a webhook event or letting the client use liff.shareTargetPicker
// where possible, and reserve Push for things the user genuinely asked to be
// notified about.
func (m *Messenger) Push(ctx context.Context, to string, messages ...any) error {
	if len(messages) == 0 {
		return nil
	}

	payload, err := json.Marshal(map[string]any{"to": to, "messages": messages})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pushEndpoint,
		bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.AccessToken)

	res, err := m.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("line: push failed: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("line: push returned %d: %s", res.StatusCode, body)
	}
	return nil
}

// ValidateSignature reports whether a webhook request really came from LINE.
//
// LINE signs the raw request body with the channel secret. The comparison uses
// hmac.Equal rather than == because a byte-by-byte comparison leaks, through
// its timing, how much of a forged signature was correct.
//
// The body must be the exact bytes received: re-encoding the JSON first will
// change the signature and reject every real request.
func ValidateSignature(channelSecret string, body []byte, signature string) bool {
	mac := hmac.New(sha256.New, []byte(channelSecret))
	mac.Write(body)
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(signature))
}

// SettlementLine is one payment rendered in a settlement summary.
type SettlementLine struct {
	FromName string
	ToName   string
	Amount   money.Satang
}

// SettlementFlex builds the Flex Message that reports who should pay whom.
//
// It is returned as a plain map rather than a typed struct because Flex is a
// large, frequently extended schema, and mirroring it in Go types costs more
// than it catches for a single layout. The altText matters: it is what appears
// in the chat list and in notifications, where the bubble itself does not
// render.
func SettlementFlex(groupName string, lines []SettlementLine, liffURL string) map[string]any {
	rows := make([]any, 0, len(lines))
	for _, l := range lines {
		rows = append(rows, map[string]any{
			"type":   "box",
			"layout": "horizontal",
			"margin": "md",
			"contents": []any{
				map[string]any{
					"type": "text", "text": l.FromName, "size": "sm",
					"color": "#555555", "flex": 4, "wrap": true,
				},
				map[string]any{
					"type": "text", "text": "→", "size": "sm",
					"color": "#aaaaaa", "flex": 1, "align": "center",
				},
				map[string]any{
					"type": "text", "text": l.ToName, "size": "sm",
					"color": "#555555", "flex": 4, "wrap": true,
				},
				map[string]any{
					"type": "text", "text": "฿" + l.Amount.Format(), "size": "sm",
					"color": "#111111", "flex": 3, "align": "end", "weight": "bold",
				},
			},
		})
	}

	altText := fmt.Sprintf("สรุปยอดกลุ่ม %s: %d รายการที่ต้องโอน", groupName, len(lines))
	if len(lines) == 0 {
		altText = fmt.Sprintf("กลุ่ม %s เคลียร์ยอดครบแล้ว", groupName)
		rows = append(rows, map[string]any{
			"type": "text", "text": "ไม่มียอดค้าง 🎉", "size": "sm",
			"color": "#888888", "align": "center", "margin": "md",
		})
	}

	body := []any{
		map[string]any{
			"type": "text", "text": groupName, "weight": "bold", "size": "lg", "wrap": true,
		},
		map[string]any{
			"type": "text", "text": "สรุปยอดที่ต้องโอน", "size": "xs",
			"color": "#888888", "margin": "sm",
		},
		map[string]any{"type": "separator", "margin": "lg"},
		map[string]any{
			"type": "box", "layout": "vertical", "margin": "lg", "contents": rows,
		},
	}

	bubble := map[string]any{
		"type": "bubble",
		"body": map[string]any{"type": "box", "layout": "vertical", "contents": body},
	}
	if liffURL != "" {
		bubble["footer"] = map[string]any{
			"type": "box", "layout": "vertical", "contents": []any{
				map[string]any{
					"type": "button", "style": "primary", "height": "sm",
					"action": map[string]any{
						"type": "uri", "label": "เปิดดูรายละเอียด", "uri": liffURL,
					},
				},
			},
		}
	}

	return map[string]any{
		"type":     "flex",
		"altText":  altText,
		"contents": bubble,
	}
}
