package middleware

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/OatApisit/billsplit-api/internal/line"
	"github.com/OatApisit/billsplit-api/internal/model"
)

// Without a negative cache, the same junk bearer token costs one outbound
// verify call to LINE every time it arrives — a single unauthenticated shell
// loop measured ~50/sec, and exhausting the channel's verify quota locks every
// real user out of logging in.
func TestTokenCacheRemembersFailures(t *testing.T) {
	c := newTestCache()

	if _, _, hit := c.get("junk"); hit {
		t.Fatal("an unseen token reported a cache hit")
	}

	c.putFailure("junk")

	user, valid, hit := c.get("junk")
	if !hit {
		t.Fatal("a rejected token was not cached; every replay hits LINE again")
	}
	if valid {
		t.Error("a rejected token came back as valid")
	}
	if user != (model.User{}) {
		t.Errorf("a rejected token carried a profile: %+v", user)
	}
}

func TestTokenCacheRemembersSuccesses(t *testing.T) {
	c := newTestCache()
	alice := model.User{ID: "U_alice", DisplayName: "Alice"}

	c.put("good", alice)

	user, valid, hit := c.get("good")
	if !hit || !valid {
		t.Fatalf("verified token: hit=%v valid=%v, want both true", hit, valid)
	}
	if user != alice {
		t.Errorf("got %+v, want %+v", user, alice)
	}
}

// The failure TTL must be shorter than the success one. A rejection can become
// valid the moment a clock skew resolves or LINE recovers, so a wrong "no" has
// to expire fast; a wrong "yes" is the dangerous direction and keeps the
// longer, still-short window.
func TestFailureTTLIsShorterThanSuccessTTL(t *testing.T) {
	c := newTokenCache()
	if c.failTTL >= c.ttl {
		t.Errorf("failTTL %s is not shorter than ttl %s", c.failTTL, c.ttl)
	}
	if c.failTTL <= 0 {
		t.Errorf("failTTL is %s; failures would not be cached at all", c.failTTL)
	}
}

func TestTokenCacheEntriesExpire(t *testing.T) {
	c := newTestCache()
	c.ttl, c.failTTL = -time.Second, -time.Second

	c.put("good", model.User{ID: "U_alice"})
	c.putFailure("junk")

	if _, _, hit := c.get("good"); hit {
		t.Error("an expired success still counted as a hit")
	}
	if _, _, hit := c.get("junk"); hit {
		t.Error("an expired failure still counted as a hit")
	}
}

// Both outcomes are keyed by the SHA-256 of the token. A rejected token is
// still a credential-shaped string, and a map keyed by the raw value puts it in
// every heap dump in plaintext.
func TestTokenCacheKeysAreHashed(t *testing.T) {
	c := newTestCache()
	c.put("good-token", model.User{ID: "U_alice"})
	c.putFailure("junk-token")

	for _, raw := range []string{"good-token", "junk-token"} {
		if _, present := c.entries[raw]; present {
			t.Errorf("%q is a map key in plaintext", raw)
		}
		if _, present := c.entries[hashToken(raw)]; !present {
			t.Errorf("%q is not stored under its hash", raw)
		}
	}
}

// roundTripFunc lets a test stand in for the call to LINE without a network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newTestAuth(rt roundTripFunc) *Auth {
	return &Auth{
		verifier: &line.Verifier{ChannelID: "channel", HTTP: &http.Client{Transport: rt}},
		cache:    newTestCache(),
	}
}

// A failure to reach LINE is not a verdict about the token.
//
// Caching it as one turns a five-second blip into thirty seconds of hard 401s
// on credentials that were never invalid, continuing after LINE has recovered —
// and a 401 tells the client to throw the token away and re-login, which is the
// wrong instruction when the token is fine and the server is not.
func TestUnreachableVerifierIs503AndNotCached(t *testing.T) {
	calls := 0
	a := newTestAuth(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("dial tcp: i/o timeout")
	})

	app := fiber.New()
	app.Get("/", a.Handler, func(c *fiber.Ctx) error { return c.SendString("ok") })

	for i := 1; i <= 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set(fiber.HeaderAuthorization, "Bearer a-perfectly-good-token")
		res, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != fiber.StatusServiceUnavailable {
			t.Fatalf("request %d: got %d, want 503 — a transport failure is not a rejection",
				i, res.StatusCode)
		}
	}

	if calls != 2 {
		t.Errorf("LINE was called %d times across two requests; the outage was cached as a verdict", calls)
	}
	if _, _, hit := a.cache.get("a-perfectly-good-token"); hit {
		t.Error("an unreachable verifier left a cached failure against a valid token")
	}
}

// A token LINE actually rejected is still a 401, and still cached: that negative
// cache is what stops one replay loop from burning the channel's verify quota.
func TestRejectedTokenIs401AndCached(t *testing.T) {
	calls := 0
	a := newTestAuth(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body: io.NopCloser(strings.NewReader(
				`{"error":"invalid_request","error_description":"invalid id token"}`)),
			Header: http.Header{},
		}, nil
	})

	app := fiber.New()
	app.Get("/", a.Handler, func(c *fiber.Ctx) error { return c.SendString("ok") })

	for i := 1; i <= 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set(fiber.HeaderAuthorization, "Bearer junk")
		res, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != fiber.StatusUnauthorized {
			t.Fatalf("request %d: got %d, want 401", i, res.StatusCode)
		}
	}

	if calls != 1 {
		t.Errorf("LINE was called %d times for the same rejected token, want 1", calls)
	}
}

// newTestCache builds a cache without newTokenCache's background reaper, which
// would otherwise outlive the test.
func newTestCache() *tokenCache {
	return &tokenCache{
		entries: make(map[string]cacheEntry),
		ttl:     5 * time.Minute,
		failTTL: 30 * time.Second,
	}
}
