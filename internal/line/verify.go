// Package line talks to the LINE platform: verifying the ID tokens that LIFF
// hands the frontend, and pushing Flex Messages back into a chat.
package line

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const verifyEndpoint = "https://api.line.me/oauth2/v2.1/verify"

// ErrInvalidToken reports an ID token that LINE would not vouch for.
var ErrInvalidToken = errors.New("line: invalid ID token")

// Profile is the verified identity behind an ID token.
//
// These fields come from LINE, not from the browser. The frontend also has a
// liff.getProfile() call that returns the same shape, but anything the client
// sends can be edited by the client, so a user ID is only trustworthy once it
// has made the round trip through Verify.
type Profile struct {
	UserID      string
	DisplayName string
	PictureURL  string
}

// Verifier checks LIFF ID tokens against the LINE platform.
type Verifier struct {
	ChannelID string
	HTTP      *http.Client
}

// NewVerifier returns a Verifier for the given LINE Login channel ID — the
// channel the LIFF app belongs to, not the Messaging API channel.
func NewVerifier(channelID string) *Verifier {
	return &Verifier{
		ChannelID: channelID,
		HTTP:      &http.Client{Timeout: 5 * time.Second},
	}
}

type verifyResponse struct {
	Iss     string `json:"iss"`
	Sub     string `json:"sub"`
	Aud     string `json:"aud"`
	Exp     int64  `json:"exp"`
	Name    string `json:"name"`
	Picture string `json:"picture"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Verify exchanges a LIFF ID token for the identity it represents.
//
// LINE validates the signature and expiry; this function additionally pins the
// audience to the configured channel. Without that check, a token minted for
// somebody else's LIFF app would be accepted here, letting them act as any
// user in this one.
func (v *Verifier) Verify(ctx context.Context, idToken string) (*Profile, error) {
	if strings.TrimSpace(idToken) == "" {
		return nil, fmt.Errorf("%w: empty token", ErrInvalidToken)
	}

	form := url.Values{
		"id_token":  {idToken},
		"client_id": {v.ChannelID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, verifyEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	res, err := v.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("line: verify request failed: %w", err)
	}
	defer res.Body.Close()

	var body verifyResponse
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("line: decode verify response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s (%s)", ErrInvalidToken, body.Error, body.ErrorDescription)
	}
	if body.Aud != v.ChannelID {
		return nil, fmt.Errorf("%w: token was issued for channel %s", ErrInvalidToken, body.Aud)
	}
	if body.Sub == "" {
		return nil, fmt.Errorf("%w: response carried no subject", ErrInvalidToken)
	}

	return &Profile{
		UserID:      body.Sub,
		DisplayName: body.Name,
		PictureURL:  body.Picture,
	}, nil
}
