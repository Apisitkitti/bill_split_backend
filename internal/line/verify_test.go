package line

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// redirectTransport sends every request to a test server instead of api.line.me.
// The endpoint is a constant in production code — there is no URL to inject —
// so the injection point is the Verifier's HTTP client, which is the same seam
// a caller would use to add a proxy or a timeout.
type redirectTransport struct{ to *url.URL }

func (rt redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = rt.to.Scheme
	clone.URL.Host = rt.to.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// newTestVerifier returns a Verifier for channelID whose requests land on
// handler, and a counter of how many requests actually got that far.
func newTestVerifier(t *testing.T, channelID string, handler http.HandlerFunc) (*Verifier, *int) {
	t.Helper()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Verifier{
		ChannelID: channelID,
		HTTP:      &http.Client{Transport: redirectTransport{to: base}},
	}, &calls
}

func TestVerifySuccess(t *testing.T) {
	const channel = "1234567890"

	v, calls := newTestVerifier(t, channel, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		// The token goes to LINE in the body, never in the URL: a query string
		// lands in access logs and proxy history in plaintext.
		if got := r.PostFormValue("id_token"); got != "a.b.c" {
			t.Errorf("id_token = %q, want the token under verification", got)
		}
		if got := r.PostFormValue("client_id"); got != channel {
			t.Errorf("client_id = %q, want %q", got, channel)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"iss":"https://access.line.me","sub":"U_alice","aud":"` + channel + `",
			"name":"Alice","picture":"https://example.test/a.jpg"}`))
	})

	profile, err := v.Verify(context.Background(), "a.b.c")
	if err != nil {
		t.Fatalf("a token LINE vouched for was rejected: %v", err)
	}
	if *calls != 1 {
		t.Errorf("LINE was called %d times, want 1", *calls)
	}
	if profile.UserID != "U_alice" {
		t.Errorf("UserID = %q, want the sub LINE returned", profile.UserID)
	}
	if profile.DisplayName != "Alice" {
		t.Errorf("DisplayName = %q, want Alice", profile.DisplayName)
	}
	if profile.PictureURL != "https://example.test/a.jpg" {
		t.Errorf("PictureURL = %q, want the picture LINE returned", profile.PictureURL)
	}
}

// The load-bearing check. LINE will happily validate the signature and expiry of
// a token minted for somebody else's LIFF app; without pinning aud to our own
// channel, whoever holds that token could act as any user here. Deleting the
// `body.Aud != v.ChannelID` block in Verify fails this test.
func TestVerifyRejectsATokenForAnotherChannel(t *testing.T) {
	const ours = "1234567890"

	v, _ := newTestVerifier(t, ours, func(w http.ResponseWriter, r *http.Request) {
		// A perfectly valid token — for a different channel.
		w.Write([]byte(`{"iss":"https://access.line.me","sub":"U_attacker",
			"aud":"9999999999","name":"Attacker"}`))
	})

	profile, err := v.Verify(context.Background(), "a.b.c")
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("got %v, want ErrInvalidToken: a token for channel 9999999999 was accepted", err)
	}
	if profile != nil {
		t.Errorf("got profile %+v alongside the error, want nil", profile)
	}
}

// An empty bearer token is refused before any network call: LINE cannot be asked
// to vouch for nothing, and the round trip would only slow down the rejection.
func TestVerifyRejectsAnEmptyToken(t *testing.T) {
	v, calls := newTestVerifier(t, "1234567890", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"sub":"U_alice","aud":"1234567890"}`))
	})

	for _, token := range []string{"", "   ", "\t\n"} {
		if _, err := v.Verify(context.Background(), token); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Verify(%q) = %v, want ErrInvalidToken", token, err)
		}
	}
	if *calls != 0 {
		t.Errorf("an empty token reached LINE %d times, want 0", *calls)
	}
}

// LINE refusing the token is the ordinary expired-or-forged case, and it must
// come back as ErrInvalidToken so middleware.Auth answers 401 rather than 500.
func TestVerifyRejectsANon200FromLine(t *testing.T) {
	v, _ := newTestVerifier(t, "1234567890", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_request","error_description":"IdToken expired."}`))
	})

	if _, err := v.Verify(context.Background(), "a.b.c"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("got %v, want ErrInvalidToken", err)
	}
}

// A 200 carrying no subject names nobody. Accepting it would hand the rest of
// the app an empty user ID, which every group query would then happily match
// rows against.
func TestVerifyRejectsAResponseWithNoSubject(t *testing.T) {
	const channel = "1234567890"

	v, _ := newTestVerifier(t, channel, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"iss":"https://access.line.me","sub":"","aud":"` + channel + `"}`))
	})

	if _, err := v.Verify(context.Background(), "a.b.c"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("got %v, want ErrInvalidToken", err)
	}
}
