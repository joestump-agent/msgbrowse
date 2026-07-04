// Same-origin + per-session-token protection for the privileged Setup POSTs
// (SPEC-0013 §Security "Same-origin protection for privileged POSTs"). The
// read-only /setup GET renders detection cards under the ADR-0010 loopback
// single-user trust and needs no token; but /setup/enable (and /setup/refresh,
// /setup/refresh-all, /setup/recheck, which reuse this gate) spawns a bundled
// exporter that reads a personal database — a privileged local action that MUST
// NOT be triggerable cross-origin, even under loopback, because another local
// process or a malicious page loaded in a browser could otherwise drive the
// exporter.
//
// The gate requires BOTH, and rejects with 403 BEFORE any subprocess starts:
//
//  1. Same-origin: the request's Origin (or, absent it, Sec-Fetch-Site:
//     same-origin, or a same-origin Referer) must match the server's own
//     loopback origin — the address the client actually reached us on.
//  2. A per-session token: minted at /setup render, submitted with the POST
//     (form field or the X-Setup-Token header htmx sends), and verified against
//     the server's live token set with a constant-time comparison.
//
// Governing: SPEC-0013 §Security "Same-origin protection for privileged POSTs"
// ("rejected unless same-origin AND a per-session token … rejected with 403 and
// MUST NOT start any subprocess"), §Security "Request body size limits".
package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// setupTokenField is the form field / header name the minted per-session token
// is submitted under. The hidden input in the Setup template carries it; htmx
// also mirrors it into the X-Setup-Token request header via hx-headers.
const (
	setupTokenField  = "setup_token"
	setupTokenHeader = "X-Setup-Token"
)

// setupBodyLimit caps the Setup POST bodies. They carry only a source enum plus
// the token — a few dozen bytes — so a kilobyte cap (SPEC-0013 §Security
// "Request body size limits": KB, not MB) rejects any malformed or oversized
// body before processing.
const setupBodyLimit = 4 << 10 // 4 KiB

// setupTokenTTL bounds how long a minted per-session Setup token stays valid. A
// token is minted at /setup render and submitted with the follow-on privileged
// POST; a real user acts within seconds-to-minutes, so an hour is generous
// headroom while still expiring stale tokens (#135 hardening). Expiry is the
// primary bound on the set's size for a long-lived `serve` process.
const setupTokenTTL = time.Hour

// setupTokenCap is the hard ceiling on the live token set, so even a burst of
// /setup renders within the TTL window cannot grow it without bound. When mint
// would exceed the cap it first drops expired tokens, then evicts the oldest
// remaining — bounding both memory and the O(n) constant-time scan in valid()
// (#135 hardening). The cap is generous: it only bites under thousands of renders
// inside one TTL window, which the single-user loopback model never reaches
// legitimately.
const setupTokenCap = 1024

// setupToken is one minted token with its expiry, so valid() can reject a stale
// token and mint()/valid() can evict expired entries.
type setupToken struct {
	expires time.Time
}

// setupTokens is the server's live set of valid per-session Setup tokens. A token
// is minted each time /setup is rendered and remembered here until it expires
// (setupTokenTTL) or is evicted to hold the set under setupTokenCap. This keeps a
// very long-lived `serve` process from accumulating unbounded tokens and linearly
// slowing the constant-time valid() scan, while preserving the security
// properties: tokens are unguessable (256-bit crypto/rand), compared in constant
// time, and only ever minted at render. Access is synchronized because handlers
// run concurrently.
//
// now is injectable so the eviction/expiry behavior is testable without sleeping;
// it defaults to time.Now.
type setupTokens struct {
	mu     sync.Mutex
	tokens map[string]setupToken
	now    func() time.Time
}

func newSetupTokens() *setupTokens {
	return &setupTokens{tokens: make(map[string]setupToken), now: time.Now}
}

// mint generates a fresh 256-bit random token, records it with a TTL expiry, and
// returns its hex string for embedding in the rendered Setup page. Before
// inserting it prunes expired tokens; if the set is still at the cap it evicts the
// single oldest token, so the set is bounded regardless of render volume.
func (s *setupTokens) mint() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneExpiredLocked(now)
	if len(s.tokens) >= setupTokenCap {
		s.evictOldestLocked()
	}
	s.tokens[tok] = setupToken{expires: now.Add(setupTokenTTL)}
	return tok, nil
}

// valid reports whether tok is a live, unexpired minted token, using a
// constant-time compare against each candidate so a rejected token leaks no
// timing signal. It prunes expired tokens as it scans, so the set does not carry
// dead entries between mints. A token that matches but is expired is rejected.
func (s *setupTokens) valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	match := false
	for known, meta := range s.tokens {
		if now.After(meta.expires) {
			// Opportunistically drop the expired entry; deleting during range is
			// safe in Go and keeps the set from carrying dead tokens.
			delete(s.tokens, known)
			continue
		}
		// Constant-time compare on every live candidate — do NOT break early on a
		// match, so the scan cost does not depend on which token matched (no timing
		// signal). We still finish pruning the remaining entries.
		if subtle.ConstantTimeCompare([]byte(known), []byte(tok)) == 1 {
			match = true
		}
	}
	return match
}

// pruneExpiredLocked drops every token whose TTL has passed. Caller holds mu.
func (s *setupTokens) pruneExpiredLocked(now time.Time) {
	for tok, meta := range s.tokens {
		if now.After(meta.expires) {
			delete(s.tokens, tok)
		}
	}
}

// evictOldestLocked removes the single token with the earliest expiry (the oldest
// mint, since every token shares the same TTL) so mint can insert under the cap.
// Caller holds mu and has already pruned expired entries.
func (s *setupTokens) evictOldestLocked() {
	var oldestTok string
	var oldestExp time.Time
	first := true
	for tok, meta := range s.tokens {
		if first || meta.expires.Before(oldestExp) {
			oldestTok, oldestExp, first = tok, meta.expires, false
		}
	}
	if !first {
		delete(s.tokens, oldestTok)
	}
}

// size reports the number of live tokens currently held (for tests).
func (s *setupTokens) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokens)
}

// checkSetupPOST enforces the same-origin + token gate on a privileged Setup
// POST. It returns true when the request may proceed; on failure it has already
// written a 403 and the caller MUST return without starting any work (SPEC-0013
// §Security "rejected with 403 and MUST NOT start any subprocess").
//
// It also installs the MaxBytesReader body cap so the subsequent form parse can
// never read an oversized body.
func (s *Server) checkSetupPOST(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, setupBodyLimit)

	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return false
	}

	// The token arrives either as the X-Setup-Token header (htmx hx-headers) or
	// the setup_token form field (a plain form POST fallback). Parse the form to
	// read the field; ParseForm respects the MaxBytesReader cap above.
	tok := r.Header.Get(setupTokenHeader)
	if tok == "" {
		// ParseForm reads the (capped) body; an oversized body errors here and is
		// treated as an invalid request.
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid request", http.StatusForbidden)
			return false
		}
		tok = r.PostFormValue(setupTokenField)
	}
	if !s.setupTokens.valid(tok) {
		http.Error(w, "missing or invalid setup token", http.StatusForbidden)
		return false
	}
	return true
}

// sameOrigin reports whether a state-changing request originates from this
// server's own loopback page. The check is layered to match browser behavior:
//
//   - Origin present: it must equal the request's own scheme://host (the address
//     the client reached us on). A cross-origin page sending a form/fetch POST
//     carries its OWN Origin, which will not match — that is exactly the attack
//     this rejects.
//   - No Origin (some same-origin GETs/navigations omit it) but
//     Sec-Fetch-Site present: accept only "same-origin"/"none"; reject
//     "cross-site"/"same-site". Modern browsers always send this on POSTs.
//   - Neither header (a bare programmatic client): fall back to Referer, which
//     must be same-origin. With no Origin, no Sec-Fetch-Site, and no Referer at
//     all, reject — a privileged POST with zero provenance is not trusted.
//
// The server's own origin is derived from the request (Host + the always-http
// loopback scheme, ADR-0010), so it is correct for both the desktop shell's
// ephemeral port and `msgbrowse serve`'s configured loopback bind with no
// mode-specific configuration.
func sameOrigin(r *http.Request) bool {
	self := "http://" + r.Host

	if origin := r.Header.Get("Origin"); origin != "" {
		return origin == self
	}

	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "cross-site", "same-site":
		return false
	}

	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		if err != nil {
			return false
		}
		return u.Scheme+"://"+u.Host == self
	}

	// No provenance headers at all: reject a privileged POST.
	return false
}
