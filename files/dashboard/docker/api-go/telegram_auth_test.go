package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// telegramSignedFields reproduces the Telegram Login Widget payload: sort the
// non-hash fields, join with \n, then HMAC-SHA256 with SHA256(bot_token).
func telegramSignedFields(token string, authDate int64) map[string]string {
	f := map[string]string{
		"id":         "123456789",
		"first_name": "Test",
		"username":   "tester",
		"auth_date":  strconv.FormatInt(authDate, 10),
	}
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+f[k])
	}
	secret := sha256.Sum256([]byte(token))
	mac := hmac.New(sha256.New, secret[:])
	_, _ = mac.Write([]byte(strings.Join(parts, "\n")))
	f["hash"] = hex.EncodeToString(mac.Sum(nil))
	return f
}

func TestTelegramAuthRejectsStaleSignature(t *testing.T) {
	const token = "unit-test-token-not-a-real-bot"
	if verifyTelegramAuth(telegramSignedFields(token, time.Now().Add(-25*time.Hour).Unix()), token) {
		t.Fatal("auth_date older than 24h must be rejected")
	}
	if !verifyTelegramAuth(telegramSignedFields(token, time.Now().Unix()), token) {
		t.Fatal("a fresh, correctly signed payload must be accepted")
	}
	missing := telegramSignedFields(token, time.Now().Unix())
	delete(missing, "auth_date")
	if verifyTelegramAuth(missing, token) {
		t.Fatal("a payload without auth_date must be rejected")
	}
}

func TestLinkTokenCannotBeReused(t *testing.T) {
	old := db
	d, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	defer func() { db = old; _ = d.Close() }()
	db = d
	if _, err := db.Exec(`CREATE TABLE telegram_link_tokens (
		token TEXT PRIMARY KEY, user_id INTEGER NOT NULL,
		expires_at TEXT NOT NULL, used INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO telegram_link_tokens(token,user_id,expires_at)
		VALUES ('abc123',7,datetime('now','+10 minutes'))`); err != nil {
		t.Fatal(err)
	}
	uid, ok := consumeLinkToken("abc123")
	if !ok || uid != 7 {
		t.Fatalf("first redeem: uid=%d ok=%v", uid, ok)
	}
	if _, ok := consumeLinkToken("abc123"); ok {
		t.Fatal("link token replay succeeded")
	}
}
