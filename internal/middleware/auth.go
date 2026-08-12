// Package middleware holds the cross-cutting request handling: who the caller
// is, and what to do when a handler panics.
package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/OatApisit/billsplit-api/internal/line"
	"github.com/OatApisit/billsplit-api/internal/model"
	"github.com/OatApisit/billsplit-api/internal/repo"
)

// userKey is where the authenticated profile is stashed on the request.
const userKey = "billsplit.user"

// Auth verifies the LIFF ID token on every request and attaches the caller.
type Auth struct {
	verifier *line.Verifier
	repo     *repo.Repo
	cache    *tokenCache
}

// NewAuth builds the authentication middleware.
func NewAuth(v *line.Verifier, r *repo.Repo) *Auth {
	return &Auth{verifier: v, repo: r, cache: newTokenCache()}
}

// Handler authenticates the request or rejects it.
//
// The client sends the ID token from liff.getIDToken() as a bearer token.
// Verifying it means an HTTP call to LINE, which would be absurd to repeat for
// every request in a session, so successful verifications are cached for a flat
// five minutes. The token's own exp is not consulted, which is why that window
// is kept far shorter than any token's lifetime. The cache key is a hash of the
// token: tokens are credentials, and a map keyed by the raw value ends up in
// heap dumps and debugger sessions in plaintext.
func (a *Auth) Handler(c *fiber.Ctx) error {
	token, ok := bearerToken(c)
	if !ok {
		return fiber.NewError(fiber.StatusUnauthorized, "missing bearer token")
	}

	if profile, valid, hit := a.cache.get(token); hit {
		if !valid {
			return fiber.NewError(fiber.StatusUnauthorized, "invalid ID token")
		}
		c.Locals(userKey, profile)
		return c.Next()
	}

	profile, err := a.verifier.Verify(c.UserContext(), token)
	if err != nil {
		// Only a verdict is cached, never a failure to reach one. Verify
		// returns ErrInvalidToken when LINE said no, and a bare wrapped error
		// for a timeout or an unreadable response — pinning the latter would
		// turn a five-second network blip into thirty seconds of hard 401s on
		// tokens that are perfectly valid, continuing long after LINE recovered.
		if !errors.Is(err, line.ErrInvalidToken) {
			slog.Error("id token verification unavailable", "error", err)
			return fiber.NewError(fiber.StatusServiceUnavailable,
				"cannot verify your session right now, please retry")
		}
		slog.Warn("id token rejected", "error", err)
		a.cache.putFailure(token)
		return fiber.NewError(fiber.StatusUnauthorized, "invalid ID token")
	}

	user := model.User{
		ID:          profile.UserID,
		DisplayName: profile.DisplayName,
		PictureURL:  profile.PictureURL,
	}
	// Every request refreshes the stored profile, which is how a user who
	// renames themselves in LINE shows up renamed to the rest of their group.
	if err := a.repo.UpsertUser(c.UserContext(), user); err != nil {
		return err
	}

	a.cache.put(token, user)
	c.Locals(userKey, user)
	return c.Next()
}

// CurrentUser returns the authenticated caller. It is only valid inside a
// route wrapped by Auth.Handler.
func CurrentUser(c *fiber.Ctx) model.User {
	user, _ := c.Locals(userKey).(model.User)
	return user
}

func bearerToken(c *fiber.Ctx) (string, bool) {
	header := c.Get(fiber.HeaderAuthorization)
	if len(header) < 8 || !strings.EqualFold(header[:7], "bearer ") {
		return "", false
	}
	token := strings.TrimSpace(header[7:])
	return token, token != ""
}

// tokenCache remembers the outcome of a verification for a short window.
//
// The success TTL is deliberately far shorter than a LIFF token's lifetime. A
// cache entry outliving the token it represents would keep accepting a
// credential that LINE has already retired, so the window is kept small enough
// that the worst case is a few minutes of staleness rather than hours.
//
// Rejections are cached too, and separately. Without that, one unauthenticated
// shell loop replaying the same junk token turns into one outbound call to LINE
// per request, and exhausting the channel's verify quota locks every real user
// out of logging in. Only an actual rejection is stored — a timeout or an
// unreadable response is not a verdict about the token and must not be recorded
// as one. The failure TTL is much shorter than the success one:
// a rejection can become valid the moment a clock skew resolves or LINE
// recovers, so a wrongly-cached "no" must expire quickly, whereas a wrongly
// cached "yes" is the dangerous direction.
//
// Only the fact of failure is stored, never the reason. A cached reason would
// go stale independently of the verdict and invite a handler to branch on it.
type tokenCache struct {
	mu       sync.RWMutex
	entries  map[string]cacheEntry
	ttl      time.Duration
	failTTL  time.Duration
	reapEver time.Duration
}

type cacheEntry struct {
	user    model.User
	valid   bool
	expires time.Time
}

func newTokenCache() *tokenCache {
	c := &tokenCache{
		entries:  make(map[string]cacheEntry),
		ttl:      5 * time.Minute,
		failTTL:  30 * time.Second,
		reapEver: 30 * time.Second,
	}
	go c.reap()
	return c
}

// get reports the cached outcome: the profile, whether verification succeeded,
// and whether there was an entry at all.
func (c *tokenCache) get(token string) (user model.User, valid, hit bool) {
	key := hashToken(token)

	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok || time.Now().After(entry.expires) {
		return model.User{}, false, false
	}
	return entry.user, entry.valid, true
}

func (c *tokenCache) put(token string, user model.User) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[hashToken(token)] = cacheEntry{
		user:    user,
		valid:   true,
		expires: time.Now().Add(c.ttl),
	}
}

// putFailure records that this token was rejected, keyed the same hashed way —
// a rejected token is still a credential-shaped string and still must not sit
// in memory in plaintext.
func (c *tokenCache) putFailure(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[hashToken(token)] = cacheEntry{
		expires: time.Now().Add(c.failTTL),
	}
}

// reap drops expired entries so that a long-running server does not accumulate
// one entry per token it has ever seen.
//
// It runs on the failure TTL rather than the success one: junk tokens arrive
// far faster than real ones and each is a distinct key, so sweeping on the
// slower interval would let a flood sit in the map for minutes after it stopped
// being useful.
func (c *tokenCache) reap() {
	for range time.Tick(c.reapEver) {
		now := time.Now()
		c.mu.Lock()
		for key, entry := range c.entries {
			if now.After(entry.expires) {
				delete(c.entries, key)
			}
		}
		c.mu.Unlock()
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
