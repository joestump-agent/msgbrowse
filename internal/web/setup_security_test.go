package web

import (
	"testing"
	"time"
)

// The token-set hardening (#135): setupTokens is bounded (setupTokenCap) and
// TTL-expiring (setupTokenTTL) so a very long-lived `serve` process rendering
// /setup many times cannot accumulate unbounded tokens or linearly slow the
// constant-time valid() scan. These tests drive expiry/eviction deterministically
// through the injectable clock — no sleeping, so the gate never goes flaky — while
// asserting the security properties (a live token still validates, an expired or
// evicted one does not).

// newTokensAt builds a setupTokens whose clock is driven by *now, so a test can
// advance time without sleeping.
func newTokensAt(now *time.Time) *setupTokens {
	s := newSetupTokens()
	s.now = func() time.Time { return *now }
	return s
}

// TestTokenValidWithinTTL: a freshly minted token validates while inside its TTL.
func TestTokenValidWithinTTL(t *testing.T) {
	now := time.Unix(0, 0)
	s := newTokensAt(&now)

	tok, err := s.mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Advance just short of the TTL — still valid.
	now = now.Add(setupTokenTTL - time.Second)
	if !s.valid(tok) {
		t.Fatal("token should still be valid within its TTL")
	}
}

// TestTokenExpiresAfterTTL: a token is rejected once its TTL has elapsed, and the
// expired entry is pruned from the set (so it does not linger and slow the scan).
func TestTokenExpiresAfterTTL(t *testing.T) {
	now := time.Unix(0, 0)
	s := newTokensAt(&now)

	tok, err := s.mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if s.size() != 1 {
		t.Fatalf("set size = %d after one mint, want 1", s.size())
	}

	// Advance past the TTL — the token is now expired and must be rejected.
	now = now.Add(setupTokenTTL + time.Second)
	if s.valid(tok) {
		t.Fatal("token should be rejected after its TTL elapses")
	}
	// valid() prunes the expired entry as it scans.
	if s.size() != 0 {
		t.Fatalf("expired token was not pruned; set size = %d, want 0", s.size())
	}
}

// TestMintPrunesExpired: minting a new token drops already-expired tokens, so the
// set does not carry dead entries across mints even without a valid() call.
func TestMintPrunesExpired(t *testing.T) {
	now := time.Unix(0, 0)
	s := newTokensAt(&now)

	if _, err := s.mint(); err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Let the first token expire, then mint a second.
	now = now.Add(setupTokenTTL + time.Second)
	fresh, err := s.mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// The expired first token was pruned on the second mint; only the fresh one
	// remains.
	if s.size() != 1 {
		t.Fatalf("set size = %d after expiry + re-mint, want 1 (expired pruned)", s.size())
	}
	if !s.valid(fresh) {
		t.Fatal("the freshly minted token should be valid")
	}
}

// TestTokenSetBoundedByCap is the core eviction guarantee: minting far past the
// cap within one TTL window keeps the set at the cap (never unbounded), by
// evicting the oldest tokens. The most-recently minted token is always still
// valid — eviction never drops the live token a fresh render just handed out.
func TestTokenSetBoundedByCap(t *testing.T) {
	now := time.Unix(0, 0)
	s := newTokensAt(&now)

	var last string
	// Mint well past the cap. Advance the clock by a tiny amount each mint so the
	// "oldest expiry" eviction has a stable order, but stay far inside the TTL so
	// nothing expires — the cap (not the TTL) must be what bounds the set here.
	for i := 0; i < setupTokenCap*2; i++ {
		now = now.Add(time.Millisecond)
		tok, err := s.mint()
		if err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
		last = tok
	}
	if s.size() > setupTokenCap {
		t.Fatalf("set size = %d exceeds cap %d — not bounded", s.size(), setupTokenCap)
	}
	// The most recent token is still live (eviction targets the oldest, not the
	// newest), so a page that just rendered can still POST.
	if !s.valid(last) {
		t.Fatal("the most-recently minted token should survive cap eviction")
	}
}

// TestOldestEvictedFirst: past the cap, the OLDEST token is the one evicted, not
// an arbitrary one — so a token still validates as long as newer tokens have not
// pushed it out.
func TestOldestEvictedFirst(t *testing.T) {
	now := time.Unix(0, 0)
	s := newTokensAt(&now)

	// Fill exactly to the cap.
	var oldest string
	for i := 0; i < setupTokenCap; i++ {
		now = now.Add(time.Millisecond)
		tok, err := s.mint()
		if err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
		if i == 0 {
			oldest = tok
		}
	}
	if !s.valid(oldest) {
		t.Fatal("precondition: the first token should still be valid at the cap")
	}
	// One more mint evicts the oldest (the first) token.
	now = now.Add(time.Millisecond)
	if _, err := s.mint(); err != nil {
		t.Fatalf("over-cap mint: %v", err)
	}
	if s.valid(oldest) {
		t.Fatal("the oldest token should have been evicted once the cap was exceeded")
	}
	if s.size() != setupTokenCap {
		t.Fatalf("set size = %d after over-cap mint, want %d", s.size(), setupTokenCap)
	}
}

// TestEmptyTokenRejected: the empty string is never valid (guards the header/form
// "no token supplied" path).
func TestEmptyTokenRejected(t *testing.T) {
	now := time.Unix(0, 0)
	s := newTokensAt(&now)
	if s.valid("") {
		t.Fatal("empty token must never be valid")
	}
}
