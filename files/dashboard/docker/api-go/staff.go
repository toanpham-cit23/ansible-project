package main

// Staff roster self-service.
//
// The old flow asked an admin to type a colleague's email and @username into a
// form. That is a claim, not a fact: nothing checked the person existed, and a
// typo silently produced an on-call entry that could never be pinged.
//
// This file makes the bot the source of truth. A person in the team group
// sends /addstaff <dashboard-user>; Telegram tells us their numeric id and
// @username, so the request carries proof of membership. An admin still
// approves - the bot only verifies identity, it does not grant a shift.

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
)

// ensureStaffSchema is idempotent and safe on an existing database.
func ensureStaffSchema() {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS staff_requests (
		   id           INTEGER PRIMARY KEY,
		   telegram_id  INTEGER NOT NULL,
		   username     TEXT NOT NULL DEFAULT '',
		   display_name TEXT NOT NULL DEFAULT '',
		   note         TEXT NOT NULL DEFAULT '',
		   status       TEXT NOT NULL DEFAULT 'pending'
		                CHECK (status IN ('pending','approved','declined')),
		   created_at   TEXT NOT NULL DEFAULT (datetime('now')),
		   decided_at   TEXT,
		   decided_by   TEXT)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ix_staff_req_tg
		   ON staff_requests(telegram_id) WHERE status = 'pending'`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			// A schema problem must not stop the dashboard from serving.
			println("staff schema:", err.Error())
		}
	}
}

// ensureDeviceProtection adds devices.protected and, on the single run that
// creates the column, marks everything already inventoried as protected.
//
// Why a one-shot migration rather than "protected = kind != 'unknown'": the
// lab's fixed infrastructure is exactly what existed when this ran. Anything an
// operator adds afterwards (the simulated nodes) stays deletable, which is the
// behaviour the Nodes page promises. Re-running the UPDATE on every boot would
// silently protect those simulated nodes too.
func ensureDeviceProtection() {
	var has bool
	if rs, err := db.Query(`PRAGMA table_info(devices)`); err == nil {
		for rs.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt sql.NullString
			if rs.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk) == nil && name == "protected" {
				has = true
			}
		}
		_ = rs.Close()
	}
	if has {
		return
	}
	if _, err := db.Exec(`ALTER TABLE devices ADD COLUMN protected INTEGER NOT NULL DEFAULT 0`); err != nil {
		println("device protection:", err.Error())
		return
	}
	if _, err := db.Exec(`UPDATE devices SET protected = 1`); err != nil {
		println("device protection seed:", err.Error())
	}
}

// knownTelegramUser answers "has this person ever spoken to our bot, and are
// they in the group we alert into". The bot cannot enumerate group members
// (Bot API limitation), so the follower table written from real updates is the
// only honest source. Returns the stored username when found.
func knownTelegramUser(username string) (string, bool) {
	u := strings.TrimPrefix(strings.TrimSpace(username), "@")
	if u == "" {
		return "", false
	}
	var found string
	err := db.QueryRow(`SELECT username FROM telegram_followers
	                    WHERE LOWER(username) = LOWER(?) AND username <> ''
	                    UNION SELECT username FROM telegram_users
	                    WHERE LOWER(username) = LOWER(?) AND username <> ''
	                    LIMIT 1`, u, u).Scan(&found)
	if err != nil || found == "" {
		return "", false
	}
	return found, true
}

// GET /api/staff/lookup?username=@someone
// Used by the roster form to refuse a name the bot has never seen.
func handleStaffLookup(w http.ResponseWriter, r *http.Request, u *User) {
	name := r.URL.Query().Get("username")
	found, ok := knownTelegramUser(name)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"exists": false, "username": strings.TrimPrefix(strings.TrimSpace(name), "@"),
			"reason": "Username not exist",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"exists": true, "username": found})
}

// GET /api/staff/requests - pending and decided requests, newest first.
func handleStaffRequests(w http.ResponseWriter, r *http.Request, u *User) {
	rs, err := db.Query(`SELECT id, telegram_id, username, display_name, note,
	                            status, created_at, COALESCE(decided_by,'')
	                     FROM staff_requests ORDER BY
	                       CASE status WHEN 'pending' THEN 0 ELSE 1 END, id DESC LIMIT 100`)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rs.Close()
	out := []map[string]any{}
	pending := 0
	for rs.Next() {
		var id, tg int64
		var uname, dname, note, status, created, by string
		if rs.Scan(&id, &tg, &uname, &dname, &note, &status, &created, &by) != nil {
			continue
		}
		if status == "pending" {
			pending++
		}
		out = append(out, map[string]any{
			"id": id, "telegram_id": tg, "username": uname, "display_name": dname,
			"note": note, "status": status, "created_at": created, "decided_by": by,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out, "pending": pending})
}

// POST /api/staff/requests/{id}  {"action":"approve"|"decline"}
//
// Approve creates a real shift_admin row. The email column is required by the
// schema and is no longer collected, so it is filled with a stable placeholder
// derived from the Telegram id - mail delivery is not how this person is
// reached any more, the bot is.
func handleStaffDecision(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Action string `json:"action"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	action := strings.ToLower(strings.TrimSpace(body.Action))
	if action != "approve" && action != "decline" {
		fail(w, http.StatusBadRequest, "action must be approve or decline")
		return
	}

	var tg int64
	var uname, dname string
	if err := db.QueryRow(`SELECT telegram_id, username, display_name FROM staff_requests
	                       WHERE id = ? AND status = 'pending'`, id).
		Scan(&tg, &uname, &dname); err != nil {
		fail(w, http.StatusNotFound, "request not found or already decided")
		return
	}
	if dname == "" {
		dname = uname
	}
	if dname == "" {
		dname = "Telegram user " + strconv.FormatInt(tg, 10)
	}

	status := "declined"
	if action == "approve" {
		// Re-check membership at approval time: someone may have left the group
		// between the request and the decision.
		if _, ok := knownTelegramUser(uname); !ok {
			fail(w, http.StatusConflict, "username not exist: người này chưa tương tác với bot")
			return
		}
		if _, err := db.Exec(`INSERT INTO shift_admins(name, email, phone, telegram, role)
		                      VALUES (?,?,?,?,'primary')`,
			dname, "tg"+strconv.FormatInt(tg, 10)+"@telegram.local", "", uname); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		status = "approved"
	}

	if _, err := db.Exec(`UPDATE staff_requests SET status = ?, decided_at = datetime('now'),
	                      decided_by = ? WHERE id = ?`, status, u.Username, id); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
}
