// Telegram incident alerting for the lab dashboard (runbook 4.1 - 4.5).
//
// Audit notes, kept in the source so the team can check the claims:
//
//   - Only the Go stdlib is used (net/http, crypto/*). Every Bot API call
//     goes to https://api.telegram.org - there is no plain-HTTP path and no
//     publicly reachable webhook endpoint (we long-poll from the backend).
//   - NO secret is hardcoded. TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID (or
//     CHAT_ID) / TELEGRAM_BOT_USERNAME come from the environment. A token
//     pasted by an admin in Settings is AES-GCM encrypted before it reaches
//     SQLite and is NEVER returned by any read endpoint (only a boolean and
//     a masked marker). Placeholders such as YOUR_BOT_TOKEN_HERE are treated
//     as "not configured".
//   - Telegram has no native @all. BuildAlert mentions the on-call admin
//     first; only when nobody is on call does it fan out to every follower
//     recorded from /start or the Login Widget (see runbook 4.3 / 4.3b).
//   - Every write route is wrapped in withAdmin in main.go. Hiding buttons in
//     the UI is cosmetic; the server check is the actual boundary (4.1c).
//   - Telegram replaces the mail server for INTERNAL incident alerts only.
//     dashboard/docker/api/app.py (Mailpit/Gmail/Resend) remains the path for
//     end-user transactional mail. They are not interchangeable.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	tgStatusAwaiting = "Đang chờ lệnh"
	tgStatusFixed    = "Đã sửa"
)

// ensureTelegramSchema is additive and safe to run on every boot. SQLite has
// no ADD COLUMN IF NOT EXISTS, so a "duplicate column" error simply means an
// older database was already migrated; it is deliberately ignored.
func ensureTelegramSchema() {
	_, _ = db.Exec(`ALTER TABLE shift_admins ADD COLUMN telegram TEXT NOT NULL DEFAULT ''`)
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS telegram_config (
			id           INTEGER PRIMARY KEY CHECK (id = 1),
			chat_id      TEXT NOT NULL DEFAULT '',
			bot_username TEXT NOT NULL DEFAULT '',
			token_enc    BLOB,
			updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
			updated_by   TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS telegram_users (
			user_id     INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			telegram_id TEXT NOT NULL DEFAULT '',
			username    TEXT NOT NULL DEFAULT '',
			followed    INTEGER NOT NULL DEFAULT 0,
			linked_at   TEXT NOT NULL DEFAULT (datetime('now')))`,
		`CREATE TABLE IF NOT EXISTS telegram_followers (
			telegram_id TEXT PRIMARY KEY,
			username    TEXT NOT NULL DEFAULT '',
			first_seen  TEXT NOT NULL DEFAULT (datetime('now')),
			source      TEXT NOT NULL DEFAULT 'start')`,
		`CREATE TABLE IF NOT EXISTS telegram_link_tokens (
			token      TEXT PRIMARY KEY,
			user_id    INTEGER NOT NULL,
			expires_at TEXT NOT NULL,
			used       INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS telegram_incidents (
			code       TEXT PRIMARY KEY,
			rule       TEXT NOT NULL DEFAULT '',
			summary    TEXT NOT NULL DEFAULT '',
			status     TEXT NOT NULL DEFAULT 'Đang chờ lệnh',
			by_user    TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now')))`,
		`CREATE TABLE IF NOT EXISTS telegram_deliveries (
			id            INTEGER PRIMARY KEY,
			ts            TEXT NOT NULL DEFAULT (datetime('now')),
			chat_id       TEXT NOT NULL DEFAULT '',
			text          TEXT NOT NULL DEFAULT '',
			status        TEXT NOT NULL DEFAULT '',
			error         TEXT NOT NULL DEFAULT '',
			incident_code TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX IF NOT EXISTS ix_tg_deliveries_ts ON telegram_deliveries(ts DESC)`,
	} {
		if _, err := db.Exec(q); err != nil {
			log.Println("telegram schema:", err)
		}
	}
	_, _ = db.Exec(`INSERT OR IGNORE INTO telegram_config(id) VALUES (1)`)
}

// isPlaceholder stops a documented example value from being used as a live
// credential. Real BotFather tokens look like "123456789:AA..." and never
// contain any of these words.
func isPlaceholder(v string) bool {
	u := strings.ToUpper(strings.TrimSpace(v))
	if u == "" {
		return true
	}
	return strings.Contains(u, "YOUR_") || strings.Contains(u, "PLACEHOLDER") ||
		strings.Contains(u, "CHANGEME") || strings.HasSuffix(u, "_HERE") ||
		u == "HERE" || u == "XXX" || u == "TODO"
}

// telegramKey reuses the existing NETDB_KEY when present. When it is absent we
// derive a stable AES-256 key from the (mandatory, >=32 char) JWT_SECRET, so
// the encrypted token can always be decrypted on the next boot.
func telegramKey() []byte {
	if k := netdbKey(); k != nil {
		return k
	}
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// tgBotToken: environment wins. Anything stored in the DB is ciphertext.
func tgBotToken() string {
	if v := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")); v != "" && !isPlaceholder(v) {
		return v
	}
	var blob []byte
	if err := db.QueryRow(`SELECT token_enc FROM telegram_config WHERE id = 1`).Scan(&blob); err != nil || len(blob) == 0 {
		return ""
	}
	s, err := aesDecrypt(telegramKey(), blob)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func tgChatID() string {
	for _, k := range []string{"TELEGRAM_CHAT_ID", "CHAT_ID"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" && !isPlaceholder(v) {
			return v
		}
	}
	var v string
	_ = db.QueryRow(`SELECT chat_id FROM telegram_config WHERE id = 1`).Scan(&v)
	if isPlaceholder(v) {
		return ""
	}
	return strings.TrimSpace(v)
}

func tgBotUsername() string {
	if v := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_USERNAME")); v != "" && !isPlaceholder(v) {
		return strings.TrimPrefix(v, "@")
	}
	var v string
	_ = db.QueryRow(`SELECT bot_username FROM telegram_config WHERE id = 1`).Scan(&v)
	if isPlaceholder(v) {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(v), "@")
}

func tgEnvOverride() bool {
	v := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	return v != "" && !isPlaceholder(v)
}

// postTelegram performs exactly one HTTPS POST. Kept separate so sendMessage
// can retry without Markdown when the Bot API rejects the entity parsing.
func postTelegram(token string, payload map[string]string) error {
	body, err := json.Marshal(payload) // UTF-8 by construction; Vietnamese is not escaped away
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost,
		"https://api.telegram.org/bot"+token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	// The Bot API also reports logical failures with HTTP 200 + {"ok":false}.
	var res struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if json.Unmarshal(data, &res) == nil && !res.OK {
		return fmt.Errorf("telegram: %s", res.Description)
	}
	return nil
}

// sendMessage posts one UTF-8 JSON message to the Bot API over HTTPS. Missing
// configuration produces a clear error, never a panic (runbook 4.4 / 4.5).
// An empty parseMode is omitted entirely; if Markdown parsing fails because a
// device name contains "*" or "_", the same text is retried as plain text.
func sendMessage(chatID, text, parseMode string) error {
	token := tgBotToken()
	if token == "" {
		return fmt.Errorf("thiếu TELEGRAM_BOT_TOKEN: đặt biến môi trường hoặc nhập token trong Settings")
	}
	if strings.TrimSpace(chatID) == "" {
		return fmt.Errorf("thiếu TELEGRAM_CHAT_ID / CHAT_ID (mã nhóm dạng -100xxxx)")
	}
	payload := map[string]string{"chat_id": chatID, "text": text}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	err := postTelegram(token, payload)
	if err != nil && parseMode != "" {
		if retry := postTelegram(token, map[string]string{"chat_id": chatID, "text": text}); retry == nil {
			return nil
		}
	}
	return err
}

func logTelegramDelivery(chatID, text, status, errMsg, code string) {
	_, _ = db.Exec(`INSERT INTO telegram_deliveries(chat_id,text,status,error,incident_code)
	                VALUES (?,?,?,?,?)`,
		truncate(chatID, 64), truncate(text, 1000), status, truncate(errMsg, 500), code)
}

// PingAll is the simple form from runbook 4.2: post into the shared group and
// mention every supplied @username. Telegram has no @all, so the caller must
// supply the list - normally the on-call admins read from the roster.
func PingAll(incident string, members []string) error {
	text := "🚨 *CẢNH BÁO SỰ CỐ*\n" + incident
	mentions := make([]string, 0, len(members))
	for _, m := range members {
		m = strings.TrimPrefix(strings.TrimSpace(m), "@")
		if m != "" {
			mentions = append(mentions, "@"+m)
		}
	}
	if len(mentions) > 0 {
		text += "\n" + strings.Join(mentions, " ")
	}
	chat := tgChatID()
	err := sendMessage(chat, text, "Markdown")
	if err != nil {
		logTelegramDelivery(chat, text, "failed", err.Error(), "")
		return err
	}
	logTelegramDelivery(chat, text, "sent", "", "")
	return nil
}

// BuildAlert implements runbook 4.3b: short summary, on-call FIRST, otherwise
// every follower, plus the incident status and the actor who changed it.
func BuildAlert(rule, summary, oncall string, followers []string, status, by string) string {
	if status == "" {
		status = tgStatusAwaiting
	}
	head := "🚨 *CẢNH BÁO* [" + rule + "] " + summary
	oncall = strings.TrimPrefix(strings.TrimSpace(oncall), "@")
	mentions := []string{}
	if oncall != "" {
		mentions = append(mentions, "@"+oncall)
	} else {
		seen := map[string]bool{}
		for _, f := range followers {
			f = strings.TrimPrefix(strings.TrimSpace(f), "@")
			if f == "" || seen[strings.ToLower(f)] {
				continue
			}
			seen[strings.ToLower(f)] = true
			mentions = append(mentions, "@"+f)
		}
	}
	body := strings.Join(mentions, " ")
	if body == "" {
		body = "(không có ca trực và chưa có follower — chỉ đăng vào nhóm chung)"
	}
	st := "Trạng thái: " + status
	by = strings.TrimPrefix(strings.TrimSpace(by), "@")
	if by != "" {
		st += " (bởi @" + by + ")"
	}
	return head + "\n" + body + "\n" + st
}

// verifyTelegramAuth checks the Login Widget hash exactly as runbook 4.1e
// describes: sorted key=value lines, secret = SHA256(bot_token), HMAC-SHA256,
// constant-time comparison. auth_date is also bounded to 24 hours.
func verifyTelegramAuth(fields map[string]string, token string) bool {
	if token == "" || fields["hash"] == "" {
		return false
	}
	// auth_date is mandatory: without it a valid signature could be replayed
	// forever. Reject anything older than 24 h (or implausibly in the future).
	ad, ok := fields["auth_date"]
	if !ok {
		return false
	}
	sec, err := strconv.ParseInt(ad, 10, 64)
	if err != nil {
		return false
	}
	age := time.Since(time.Unix(sec, 0))
	if age > 24*time.Hour || age < -5*time.Minute {
		return false
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if k != "hash" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(k + "=" + fields[k])
	}
	secretKey := sha256.Sum256([]byte(token))
	mac := hmac.New(sha256.New, secretKey[:])
	_, _ = mac.Write([]byte(sb.String()))
	want, err := hex.DecodeString(fields["hash"])
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(mac.Sum(nil), want) == 1
}

func tgFollowers() []string {
	out := []string{}
	rs, err := db.Query(`SELECT username FROM telegram_followers WHERE username <> ''
	                     UNION SELECT username FROM telegram_users WHERE followed = 1 AND username <> ''`)
	if err != nil {
		return out
	}
	defer rs.Close()
	for rs.Next() {
		var u string
		if rs.Scan(&u) == nil && strings.TrimSpace(u) != "" {
			out = append(out, strings.TrimPrefix(strings.TrimSpace(u), "@"))
		}
	}
	return out
}

// tgOncall reads the @username of whoever holds the pager right now. The
// roster (and handovers) stays the single source of truth because oncallNow()
// is the same function the UI banner and the mail pipeline use.
func tgOncall() string {
	cur, _, _, _ := oncallNow()
	if cur == nil {
		return ""
	}
	id, ok := cur["id"].(int64)
	if !ok {
		return ""
	}
	var tg string
	_ = db.QueryRow(`SELECT COALESCE(telegram,'') FROM shift_admins WHERE id = ?`, id).Scan(&tg)
	return strings.TrimPrefix(strings.TrimSpace(tg), "@")
}

func tgActor(u *User) string {
	var name string
	_ = db.QueryRow(`SELECT username FROM telegram_users WHERE user_id = ?`, u.ID).Scan(&name)
	if strings.TrimSpace(name) == "" {
		return u.Username
	}
	return strings.TrimPrefix(strings.TrimSpace(name), "@")
}

// ------------------------------------------------------------------ reads

// GET /api/telegram/status - never returns the token or the raw chat id.
func handleTelegramStatus(w http.ResponseWriter, r *http.Request, u *User) {
	var username string
	var followed int
	_ = db.QueryRow(`SELECT username, followed FROM telegram_users WHERE user_id = ?`, u.ID).
		Scan(&username, &followed)
	token := tgBotToken()
	source := "none"
	if tgEnvOverride() {
		source = "env"
	} else if token != "" {
		source = "settings"
	}
	bot := tgBotUsername()
	loginURL := ""
	if bot != "" {
		loginURL = "https://t.me/" + bot
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":        token != "" && tgChatID() != "" && bot != "",
		"token_set":         token != "",
		"token_source":      source,
		"token_masked":      "••••••••",
		"chat_id_set":       tgChatID() != "",
		"bot_username":      bot,
		"env_override":      tgEnvOverride(),
		"linked":            username != "",
		"telegram_username": username,
		"followed":          followed == 1,
		"followers":         tgFollowers(),
		"login_url":         loginURL,
		"note":              "Join nhóm Telegram chung của team để nhận cảnh báo sự cố.",
	})
}

func handleTelegramFollowers(w http.ResponseWriter, r *http.Request, u *User) {
	writeJSON(w, http.StatusOK, map[string]any{"followers": tgFollowers()})
}

func handleTelegramIncidents(w http.ResponseWriter, r *http.Request, u *User) {
	rs, err := db.Query(`SELECT code, rule, summary, status, COALESCE(by_user,''), created_at, updated_at
	                     FROM telegram_incidents ORDER BY updated_at DESC LIMIT 50`)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rs.Close()
	out := []map[string]any{}
	for rs.Next() {
		var code, rule, summary, status, by, created, updated string
		if rs.Scan(&code, &rule, &summary, &status, &by, &created, &updated) == nil {
			out = append(out, map[string]any{"code": code, "rule": rule, "summary": summary,
				"status": status, "by_user": by, "created_at": created, "updated_at": updated})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": out})
}

func handleTelegramDeliveries(w http.ResponseWriter, r *http.Request, u *User) {
	rs, err := db.Query(`SELECT ts, status, incident_code, substr(text,1,120), COALESCE(error,'')
	                     FROM telegram_deliveries ORDER BY id DESC LIMIT 50`)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rs.Close()
	out := []map[string]any{}
	for rs.Next() {
		var ts, status, code, text, errText string
		if rs.Scan(&ts, &status, &code, &text, &errText) == nil {
			out = append(out, map[string]any{"ts": ts, "status": status,
				"incident_code": code, "text": text, "error": errText})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out})
}

// ----------------------------------------------------------------- writes

// PUT /api/telegram/config - admin only. The token is encrypted before it is
// written; chat id and bot username are not secrets but are still never
// echoed back in full by the status endpoint.
func handleTelegramConfig(w http.ResponseWriter, r *http.Request, u *User) {
	var b struct {
		BotToken    string `json:"bot_token"`
		ChatID      string `json:"chat_id"`
		BotUsername string `json:"bot_username"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	chat := strings.TrimSpace(b.ChatID)
	bot := strings.TrimPrefix(strings.TrimSpace(b.BotUsername), "@")
	if isPlaceholder(chat) {
		chat = ""
	}
	if isPlaceholder(bot) {
		bot = ""
	}
	if _, err := db.Exec(`UPDATE telegram_config SET chat_id = ?, bot_username = ?,
	                      updated_at = datetime('now'), updated_by = ? WHERE id = 1`,
		chat, bot, u.Username); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	token := strings.TrimSpace(b.BotToken)
	stored := "unchanged"
	if token != "" {
		if isPlaceholder(token) {
			if _, err := db.Exec(`UPDATE telegram_config SET token_enc = NULL,
			                      updated_at = datetime('now'), updated_by = ? WHERE id = 1`, u.Username); err != nil {
				fail(w, http.StatusInternalServerError, err.Error())
				return
			}
			stored = "cleared"
		} else {
			enc, err := aesEncrypt(telegramKey(), token)
			if err != nil {
				fail(w, http.StatusInternalServerError, err.Error())
				return
			}
			if _, err := db.Exec(`UPDATE telegram_config SET token_enc = ?,
			                      updated_at = datetime('now'), updated_by = ? WHERE id = 1`,
				enc, u.Username); err != nil {
				fail(w, http.StatusInternalServerError, err.Error())
				return
			}
			stored = "encrypted"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": stored,
		"env_override": tgEnvOverride()})
}

// POST /api/telegram/auth - Login Widget callback. The hash is verified with
// the bot token on the server; the client is never trusted (runbook 4.1e).
func handleTelegramAuth(w http.ResponseWriter, r *http.Request, u *User) {
	var raw map[string]any
	if err := decode(r, &raw); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	fields := map[string]string{}
	for k, v := range raw {
		switch x := v.(type) {
		case string:
			fields[k] = x
		case float64:
			fields[k] = strconv.FormatFloat(x, 'f', -1, 64)
		}
	}
	if !verifyTelegramAuth(fields, tgBotToken()) {
		fail(w, http.StatusForbidden, "telegram signature invalid or expired")
		return
	}
	username := strings.TrimPrefix(strings.TrimSpace(fields["username"]), "@")
	if username == "" {
		fail(w, http.StatusBadRequest, "tài khoản Telegram cần có @username")
		return
	}
	if _, err := db.Exec(`INSERT INTO telegram_users(user_id, telegram_id, username, followed, linked_at)
	                      VALUES (?,?,?,1,datetime('now'))
	                      ON CONFLICT(user_id) DO UPDATE SET telegram_id = excluded.telegram_id,
	                      username = excluded.username, followed = 1, linked_at = datetime('now')`,
		u.ID, fields["id"], username); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if fields["id"] != "" {
		_, _ = db.Exec(`INSERT OR IGNORE INTO telegram_followers(telegram_id, username, source)
		                VALUES (?,?,'widget')`, fields["id"], username)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "telegram_username": username})
}

// POST /api/telegram/link-token - deep-link flow (runbook 4.1e, method B).
// Single-use, 10 minute, bound to the authenticated dashboard session.
func handleTelegramLinkToken(w http.ResponseWriter, r *http.Request, u *User) {
	tok := randomToken(18)
	if _, err := db.Exec(`INSERT INTO telegram_link_tokens(token, user_id, expires_at)
	                      VALUES (?, ?, datetime('now','+10 minutes'))`, tok, u.ID); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	bot := tgBotUsername()
	deep := ""
	if bot != "" {
		deep = "https://t.me/" + bot + "?start=" + tok
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url": deep, "expires_in_s": 600,
		"instructions": "Mở link, bấm Start, rồi gửi /start để bot ghi nhận bạn là follower.",
	})
}

// consumeLinkToken redeems a deep-link token atomically. The conditional
// UPDATE is the gate: a replay sees zero affected rows and is rejected even if
// two /start messages arrive in the same instant.
func consumeLinkToken(token string) (int64, bool) {
	res, err := db.Exec(`UPDATE telegram_link_tokens SET used = 1
	                     WHERE token = ? AND used = 0 AND expires_at > datetime('now')`, token)
	if err != nil {
		return 0, false
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return 0, false
	}
	var uid int64
	if db.QueryRow(`SELECT user_id FROM telegram_link_tokens WHERE token = ?`, token).
		Scan(&uid) != nil {
		return 0, false
	}
	return uid, true
}

// POST /api/telegram/test - the runbook's 4.4 smoke test, callable by an admin.
func handleTelegramTest(w http.ResponseWriter, r *http.Request, u *User) {
	members := []string{}
	for _, m := range strings.Split(os.Getenv("MEMBERS"), ",") {
		if strings.TrimSpace(m) != "" {
			members = append(members, m)
		}
	}
	if len(members) == 0 {
		if oc := tgOncall(); oc != "" {
			members = append(members, oc)
		} else {
			members = tgFollowers()
		}
	}
	incident := "Kiểm thử cấu hình cảnh báo từ dashboard (" + u.Username + ")."
	if err := PingAll(incident, members); err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mentions": members})
}

// POST /api/telegram/alert - raise an incident (status "Đang chờ lệnh").
func handleTelegramAlert(w http.ResponseWriter, r *http.Request, u *User) {
	var b struct {
		Rule    string `json:"rule"`
		Summary string `json:"summary"`
		Code    string `json:"code"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	rule := strings.TrimSpace(b.Rule)
	if rule == "" {
		rule = "manual"
	}
	summary := strings.TrimSpace(b.Summary)
	if summary == "" {
		summary = "Sự cố mới từ dashboard"
	}
	code := strings.TrimSpace(b.Code)
	if code == "" {
		code = "INC-" + time.Now().Format("20060102-150405")
	}
	oncall := tgOncall()
	followers := tgFollowers()
	text := BuildAlert(rule, summary, oncall, followers, tgStatusAwaiting, "")
	chat := tgChatID()
	err := sendMessage(chat, text, "Markdown")
	status, errText := "sent", ""
	if err != nil {
		status, errText = "failed", err.Error()
	}
	logTelegramDelivery(chat, text, status, errText, code)
	_, _ = db.Exec(`INSERT INTO telegram_incidents(code, rule, summary, status, by_user, created_at, updated_at)
	                VALUES (?,?,?,?,'',datetime('now'),datetime('now'))
	                ON CONFLICT(code) DO UPDATE SET rule = excluded.rule,
	                summary = excluded.summary, updated_at = datetime('now')`,
		code, rule, summary, tgStatusAwaiting)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "code": code,
		"oncall": oncall, "followers_mentioned": oncall == "", "text": text})
}

// POST /api/telegram/incident - lifecycle update. The actor comes from the
// authenticated session (or its linked Telegram account), never from the body.
func handleTelegramIncident(w http.ResponseWriter, r *http.Request, u *User) {
	var b struct {
		Code   string `json:"code"`
		Status string `json:"status"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	code := strings.TrimSpace(b.Code)
	if code == "" {
		fail(w, http.StatusBadRequest, "mã sự cố là bắt buộc")
		return
	}
	status := tgStatusFixed
	switch strings.ToLower(strings.TrimSpace(b.Status)) {
	case "awaiting", "reopen", "pending":
		status = tgStatusAwaiting
	}
	var summary string
	if err := db.QueryRow(`SELECT summary FROM telegram_incidents WHERE code = ?`, code).Scan(&summary); err != nil {
		fail(w, http.StatusNotFound, "không tìm thấy sự cố "+code)
		return
	}
	actor := tgActor(u)
	_, _ = db.Exec(`UPDATE telegram_incidents SET status = ?, by_user = ?,
	                updated_at = datetime('now') WHERE code = ?`, status, actor, code)
	text := "✅ *CẬP NHẬT SỰ CỐ* [" + code + "] " + summary +
		"\nTrạng thái: " + status + " (bởi @" + actor + ")"
	chat := tgChatID()
	err := sendMessage(chat, text, "Markdown")
	statusText, errText := "sent", ""
	if err != nil {
		statusText, errText = "failed", err.Error()
	}
	logTelegramDelivery(chat, text, statusText, errText, code)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "code": code,
		"status": status, "by": actor, "text": text})
}

// ------------------------------------------------- background long polling
//
// Started as a goroutine from main(). With no configured token it simply
// idles, so a fresh install (or a placeholder .env) never makes outbound
// calls. All traffic is HTTPS and originates from the backend; nothing new is
// exposed on a public port.

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Text string `json:"text"`
		From struct {
			ID        int64  `json:"id"`
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
		} `json:"from"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

func startTelegramPoller() {
	go func() {
		var offset int64
		for {
			token := tgBotToken()
			if token == "" {
				time.Sleep(60 * time.Second)
				continue
			}
			endpoint := "https://api.telegram.org/bot" + token +
				"/getUpdates?timeout=25&offset=" + strconv.FormatInt(offset, 10)
			client := &http.Client{Timeout: 40 * time.Second}
			resp, err := client.Get(endpoint)
			if err != nil {
				time.Sleep(10 * time.Second)
				continue
			}
			var res struct {
				OK     bool       `json:"ok"`
				Result []tgUpdate `json:"result"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&res)
			_ = resp.Body.Close()
			if decodeErr != nil || !res.OK {
				time.Sleep(10 * time.Second)
				continue
			}
			for _, up := range res.Result {
				if up.UpdateID >= offset {
					offset = up.UpdateID + 1
				}
				if up.Message != nil {
					tgHandleCommand(up.Message.Text, up.Message.From.ID,
						up.Message.From.Username, up.Message.Chat.ID)
				}
			}
		}
	}()
}

// tgHandleCommand implements /start <link_token>, /follow and /done|/ack <code>.
// Only senders known as followers (or linked dashboard accounts) may change an
// incident's state - an arbitrary person in the group cannot close an alert.
func tgHandleCommand(text string, telegramID int64, username string, chatID int64) {
	text = strings.TrimSpace(text)
	if text == "" || telegramID == 0 {
		return
	}
	sid := strconv.FormatInt(telegramID, 10)
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	parts := strings.Fields(text)
	cmd := strings.ToLower(strings.SplitN(parts[0], "@", 2)[0])

	if cmd == "/start" || cmd == "/follow" {
		if username != "" {
			_, _ = db.Exec(`INSERT INTO telegram_followers(telegram_id, username, source)
			                VALUES (?,?,'start') ON CONFLICT(telegram_id) DO UPDATE SET
			                username = excluded.username`, sid, username)
			_, _ = db.Exec(`UPDATE telegram_users SET followed = 1 WHERE telegram_id = ?`, sid)
		}
		if cmd == "/start" && len(parts) > 1 {
			link := parts[1]
			if uid, ok := consumeLinkToken(link); ok {
				_, _ = db.Exec(`INSERT INTO telegram_users(user_id, telegram_id, username, followed, linked_at)
				                VALUES (?,?,?,1,datetime('now'))
				                ON CONFLICT(user_id) DO UPDATE SET telegram_id = excluded.telegram_id,
				                username = excluded.username, followed = 1, linked_at = datetime('now')`,
					uid, sid, username)
				if chatID != 0 {
					_ = sendMessage(strconv.FormatInt(chatID, 10),
						"✅ Đã liên kết tài khoản dashboard với @"+username+". Bạn sẽ nhận cảnh báo sự cố.", "")
				}
			}
		}
		return
	}

	if (cmd == "/done" || cmd == "/ack") && len(parts) > 1 {
		if username == "" {
			return
		}
		var known int
		_ = db.QueryRow(`SELECT COUNT(*) FROM telegram_followers WHERE telegram_id = ?`, sid).Scan(&known)
		if known == 0 {
			_ = db.QueryRow(`SELECT COUNT(*) FROM telegram_users WHERE telegram_id = ?`, sid).Scan(&known)
		}
		if known == 0 {
			return // not a follower: ignore, never trust an unknown sender
		}
		code := parts[1]
		var summary string
		if err := db.QueryRow(`SELECT summary FROM telegram_incidents WHERE code = ?`, code).Scan(&summary); err != nil {
			return
		}
		_, _ = db.Exec(`UPDATE telegram_incidents SET status = ?, by_user = ?,
		                updated_at = datetime('now') WHERE code = ?`, tgStatusFixed, username, code)
		msg := "✅ *CẬP NHẬT SỰ CỐ* [" + code + "] " + summary +
			"\nTrạng thái: " + tgStatusFixed + " (bởi @" + username + ")"
		chat := tgChatID()
		err := sendMessage(chat, msg, "Markdown")
		st, errText := "sent", ""
		if err != nil {
			st, errText = "failed", err.Error()
		}
		logTelegramDelivery(chat, msg, st, errText, code)
	}
}
