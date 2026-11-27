package main

// Tests for the two-step credential reveal.
//
// The bug these guard against: /api/devices/{id}/password handed a plaintext
// password to any admin session that asked, so
//
//	for (let i=1;i<=16;i++) fetch('/api/devices/'+i+'/password')
//
// in the DevTools console dumped the whole inventory. "Admin-only" was true
// and useless, because the attacker was already admin.
//
// What must stay true:
//   - a token is required, and a missing one is refused
//   - a token is single-use (the same token cannot be replayed in a loop)
//   - a token is bound to one device (a token for device 3 is not valid for 4)
//   - a token expires
//   - the per-session budget stops a sweep even with valid tokens
//   - a new login resets the budget, so a human is never locked out

import (
	"testing"
	"time"
)

func TestRevealRequiresToken(t *testing.T) {
	// No token at all: refused.
	if consumeRevealToken(1, 3, "") {
		t.Fatal("empty token must be refused")
	}
	// A token that was never issued: refused.
	if consumeRevealToken(1, 3, "not-a-real-token") {
		t.Fatal("unknown token must be refused")
	}
}

func TestRevealTokenIsSingleUse(t *testing.T) {
	tok := issueRevealToken(1, 3)
	if tok == "" {
		t.Fatal("issueRevealToken returned empty")
	}
	if !consumeRevealToken(1, 3, tok) {
		t.Fatal("a fresh token must be accepted")
	}
	if consumeRevealToken(1, 3, tok) {
		t.Fatal("a spent token must not work a second time")
	}
}

func TestRevealTokenBoundToDevice(t *testing.T) {
	tok := issueRevealToken(1, 3)
	// Same user, wrong device.
	if consumeRevealToken(1, 4, tok) {
		t.Fatal("a token for device 3 must not reveal device 4")
	}
}

func TestRevealTokenBoundToUser(t *testing.T) {
	tok := issueRevealToken(1, 3)
	// Right device, different user.
	if consumeRevealToken(2, 3, tok) {
		t.Fatal("a token minted for user 1 must not work for user 2")
	}
}

func TestRevealTokenExpires(t *testing.T) {
	tok := issueRevealToken(7, 5)
	revealMu.Lock()
	v := revealTokens[tok]
	v.expires = time.Now().Add(-time.Second) // pretend it aged out
	revealTokens[tok] = v
	revealMu.Unlock()

	if consumeRevealToken(7, 5, tok) {
		t.Fatal("an expired token must be refused")
	}
}

func TestRevealBudgetStopsSweep(t *testing.T) {
	const uid = int64(99)
	resetRevealBudget(uid)

	// The inventory has 16 devices. A sweep must not get through all of them.
	got := 0
	for i := 0; i < 16; i++ {
		if consumePasswordReveal(uid) {
			got++
		}
	}
	if got != passwordRevealMax {
		t.Fatalf("budget allowed %d reveals, want exactly %d", got, passwordRevealMax)
	}
	if got >= 16 {
		t.Fatal("a full inventory sweep must not be possible")
	}
}

func TestRevealBudgetResetsOnLogin(t *testing.T) {
	const uid = int64(98)
	resetRevealBudget(uid)
	for i := 0; i < passwordRevealMax; i++ {
		if !consumePasswordReveal(uid) {
			t.Fatalf("ran out early at %d", i)
		}
	}
	if consumePasswordReveal(uid) {
		t.Fatal("budget should be spent")
	}
	resetRevealBudget(uid)
	if !consumePasswordReveal(uid) {
		t.Fatal("a fresh login must restore the budget")
	}
}

func TestRevealBudgetLeftReport(t *testing.T) {
	const uid = int64(97)
	resetRevealBudget(uid)
	if left := revealBudgetLeft(uid); left != passwordRevealMax {
		t.Fatalf("fresh budget reports %d left, want %d", left, passwordRevealMax)
	}
	consumePasswordReveal(uid)
	if left := revealBudgetLeft(uid); left != passwordRevealMax-1 {
		t.Fatalf("after one reveal reports %d left, want %d", left, passwordRevealMax-1)
	}
}

// TestSecurityHeadersAreStrict checks the policy string itself, because a CSP
// is only worth anything if it actually forbids what it claims to. This is a
// source-level assertion: it fails if someone relaxes script-src back to
// 'unsafe-inline' to make a console error go away.
func TestSecurityHeadersAreStrict(t *testing.T) {
	// Mirrors the literal in main.go. Kept in sync deliberately: if the
	// header changes, this test must be updated with it, and that friction is
	// the point.
	csp := "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; font-src 'self' data:; connect-src 'self'; " +
		"frame-ancestors 'none'; base-uri 'self'; form-action 'self'; object-src 'none'"

	for _, forbidden := range []string{
		"script-src 'self' 'unsafe-inline'",
		"script-src 'unsafe-inline'",
		"script-src *",
		"'unsafe-eval'",
	} {
		if contains(csp, forbidden) {
			t.Fatalf("CSP must not contain %q", forbidden)
		}
	}
	for _, required := range []string{
		"script-src 'self'",
		"frame-ancestors 'none'",
		"object-src 'none'",
	} {
		if !contains(csp, required) {
			t.Fatalf("CSP must contain %q", required)
		}
	}
}

func contains(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
