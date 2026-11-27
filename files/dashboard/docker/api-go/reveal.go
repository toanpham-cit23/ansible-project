package main

// One-shot confirmation tokens for credential reveals, plus the per-session
// reveal budget.
//
// The problem this solves: /api/devices/{id}/password used to hand back a
// plaintext password to any admin session that asked. Opening DevTools and
// running a three-line loop walked the whole inventory. "Admin-only" was true
// and irrelevant - the attacker was already admin.
//
// The fix is not to hide the button harder. A button is a UI element and the
// attacker controls the UI. The fix is to make each reveal an explicit,
// separate, recorded act that costs something:
//
//	POST /api/devices/{id}/password/confirm  -> token, valid revealTokenTTL
//	GET  /api/devices/{id}/password?token=.. -> the password, token burnt
//
// A token is bound to (user, device), so a token minted for device 3 cannot be
// replayed against device 4. It is single-use, so the same token cannot be
// reused in a loop. And the budget caps how many reveals one session can make
// in total, which is the part that actually stops a script: a human clicking
// the eye icon a handful of times never notices it.
//
// State is in memory, like the login limiter. That is deliberate: a restart
// clearing the budget is fine, and it keeps this out of the database where a
// stale token table would need its own cleanup job.

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

const (
	// revealTokenTTL is short enough that a token is useless if it leaks
	// through a log or a screenshot, long enough for a click to complete.
	revealTokenTTL = 60 * time.Second

	// passwordRevealMax is the number of reveals one user may make per
	// session. The Database view has 16 devices; a demo reveals two or three.
	// 12 leaves room to be generous while still stopping a full sweep.
	passwordRevealMax = 12
)

type revealToken struct {
	userID   int64
	deviceID int64
	expires  time.Time
}

var (
	revealMu     sync.Mutex
	revealTokens = map[string]revealToken{}
	revealCount  = map[int64]int{}
)

func newRevealTokenID() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not something to paper over with a weak
		// token; the caller treats an empty token as "cannot reveal".
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// issueRevealToken mints a single-use token for one (user, device) pair.
func issueRevealToken(userID, deviceID int64) string {
	tok := newRevealTokenID()
	if tok == "" {
		return ""
	}
	revealMu.Lock()
	defer revealMu.Unlock()
	now := time.Now()
	// Drop expired tokens on the way in so the map cannot grow without bound.
	for k, v := range revealTokens {
		if now.After(v.expires) {
			delete(revealTokens, k)
		}
	}
	revealTokens[tok] = revealToken{
		userID:   userID,
		deviceID: deviceID,
		expires:  now.Add(revealTokenTTL),
	}
	return tok
}

// consumeRevealToken validates and burns a token. It returns false for a
// missing, expired, already-used, or wrong-device token - all of which are the
// same answer to the caller: you did not confirm this reveal.
func consumeRevealToken(userID, deviceID int64, tok string) bool {
	if tok == "" {
		return false
	}
	revealMu.Lock()
	defer revealMu.Unlock()
	v, ok := revealTokens[tok]
	if !ok {
		return false
	}
	// Burn it whatever happens next, so a failed match cannot be retried with
	// the same value.
	delete(revealTokens, tok)
	if v.userID != userID || v.deviceID != deviceID {
		return false
	}
	if time.Now().After(v.expires) {
		return false
	}
	return true
}

// consumePasswordReveal charges one reveal to the session budget. It returns
// false when the budget is spent, which the handler turns into a 429.
func consumePasswordReveal(userID int64) bool {
	revealMu.Lock()
	defer revealMu.Unlock()
	if revealCount[userID] >= passwordRevealMax {
		return false
	}
	revealCount[userID]++
	return true
}

// resetRevealBudget is called on a successful login, so a new session starts
// clean. Without it, a user who used up the budget could never reveal again
// without restarting the process.
func resetRevealBudget(userID int64) {
	revealMu.Lock()
	defer revealMu.Unlock()
	delete(revealCount, userID)
}

// revealBudgetLeft is exposed for tests and for the UI to warn before the
// budget runs out.
func revealBudgetLeft(userID int64) int {
	revealMu.Lock()
	defer revealMu.Unlock()
	left := passwordRevealMax - revealCount[userID]
	if left < 0 {
		return 0
	}
	return left
}
