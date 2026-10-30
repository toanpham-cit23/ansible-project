// lab.go — the operations layer added on top of the original dashboard:
// address leases, shift roster and alerting, reboot history, log findings,
// and automatic topology discovery.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// ---------------------------------------------------------------- migration

// ensureLabSchema creates the tables the poller writes and this API reads.
// Everything is additive and idempotent: on an existing database the CREATEs
// are no-ops and the ALTERs fail with "duplicate column", which is the steady
// state, so errors are deliberately discarded.
func ensureLabSchema() {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS ip_leases (
			id          INTEGER PRIMARY KEY,
			ip          TEXT NOT NULL,
			mac         TEXT NOT NULL,
			hostname    TEXT,
			device_name TEXT,
			vlan_id     INTEGER,
			subnet      TEXT,
			seen_on     TEXT,
			switch_name TEXT,
			if_index    TEXT,
			source      TEXT NOT NULL DEFAULT 'arp',
			is_static   INTEGER NOT NULL DEFAULT 0,
			online      INTEGER NOT NULL DEFAULT 1,
			first_seen  TEXT NOT NULL DEFAULT (datetime('now')),
			last_seen   TEXT NOT NULL DEFAULT (datetime('now')),
			moved_from  TEXT,
			UNIQUE (ip, mac))`,
		`CREATE TABLE IF NOT EXISTS lease_events (
			id       INTEGER PRIMARY KEY,
			ts       TEXT NOT NULL DEFAULT (datetime('now')),
			ip       TEXT NOT NULL,
			mac      TEXT,
			kind     TEXT NOT NULL CHECK (kind IN ('new','gone','moved','renamed','static')),
			detail   TEXT)`,
		`CREATE TABLE IF NOT EXISTS device_reboots (
			id             INTEGER PRIMARY KEY,
			device_id      INTEGER REFERENCES devices(id) ON DELETE CASCADE,
			ts             TEXT NOT NULL DEFAULT (datetime('now')),
			prev_uptime_s  INTEGER,
			new_uptime_s   INTEGER,
			gap_s          INTEGER,
			kind           TEXT NOT NULL DEFAULT 'restart'
			               CHECK (kind IN ('restart','power-loss','crash','first-seen')),
			note           TEXT)`,
		`CREATE TABLE IF NOT EXISTS reach_samples (
			id        INTEGER PRIMARY KEY,
			device_id INTEGER REFERENCES devices(id) ON DELETE CASCADE,
			ts        TEXT NOT NULL DEFAULT (datetime('now')),
			reachable INTEGER NOT NULL,
			rtt_ms    INTEGER)`,
		`CREATE TABLE IF NOT EXISTS shift_admins (
			id         INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			email      TEXT NOT NULL,
			phone      TEXT,
			role       TEXT NOT NULL DEFAULT 'primary'
			           CHECK (role IN ('primary','backup')),
			days_mask  INTEGER NOT NULL DEFAULT 127,
			starts_at  TEXT NOT NULL DEFAULT '00:00',
			ends_at    TEXT NOT NULL DEFAULT '23:59',
			sort_order INTEGER NOT NULL DEFAULT 0,
			active     INTEGER NOT NULL DEFAULT 1)`,
		`CREATE TABLE IF NOT EXISTS shift_handovers (
			id         INTEGER PRIMARY KEY,
			ts         TEXT NOT NULL DEFAULT (datetime('now')),
			from_admin INTEGER REFERENCES shift_admins(id) ON DELETE SET NULL,
			to_admin   INTEGER REFERENCES shift_admins(id) ON DELETE CASCADE,
			by_user    TEXT,
			reason     TEXT NOT NULL DEFAULT 'manual'
			           CHECK (reason IN ('scheduled','overtime','manual','escalation','auto')),
			note       TEXT,
			due_at     TEXT)`,
		`CREATE TABLE IF NOT EXISTS alert_rules (
			id           INTEGER PRIMARY KEY,
			name         TEXT NOT NULL UNIQUE,
			enabled      INTEGER NOT NULL DEFAULT 1,
			min_severity TEXT NOT NULL DEFAULT 'average'
			             CHECK (min_severity IN ('info','warning','average','high','disaster')),
			match_like   TEXT,
			recipients   TEXT NOT NULL DEFAULT 'oncall',
			cooldown_s   INTEGER NOT NULL DEFAULT 300)`,
		`CREATE TABLE IF NOT EXISTS alert_deliveries (
			id         INTEGER PRIMARY KEY,
			ts         TEXT NOT NULL DEFAULT (datetime('now')),
			rule_id    INTEGER REFERENCES alert_rules(id) ON DELETE SET NULL,
			event_id   INTEGER,
			admin_id   INTEGER REFERENCES shift_admins(id) ON DELETE SET NULL,
			to_addr    TEXT NOT NULL,
			subject    TEXT NOT NULL,
			status     TEXT NOT NULL DEFAULT 'queued'
			           CHECK (status IN ('queued','sent','failed','suppressed')),
			error      TEXT,
			tries      INTEGER NOT NULL DEFAULT 0,
			kind       TEXT NOT NULL DEFAULT 'rule'
			           CHECK (kind IN ('rule','redispatch','test')))`,
		`CREATE TABLE IF NOT EXISTS log_patterns (
			id       INTEGER PRIMARY KEY,
			name     TEXT NOT NULL UNIQUE,
			like_pat TEXT,
			regex    TEXT,
			severity TEXT NOT NULL DEFAULT 'warning',
			category TEXT,
			hint     TEXT,
			enabled  INTEGER NOT NULL DEFAULT 1)`,
		`CREATE TABLE IF NOT EXISTS log_findings (
			id         INTEGER PRIMARY KEY,
			ts         TEXT NOT NULL DEFAULT (datetime('now')),
			last_ts    TEXT NOT NULL DEFAULT (datetime('now')),
			pattern_id INTEGER REFERENCES log_patterns(id) ON DELETE SET NULL,
			device_id  INTEGER REFERENCES devices(id) ON DELETE CASCADE,
			source     TEXT,
			line       TEXT,
			severity   TEXT NOT NULL DEFAULT 'warning',
			category   TEXT,
			hint       TEXT,
			sig        TEXT,
			hits       INTEGER NOT NULL DEFAULT 1,
			acked      INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS link_proposals (
			id         INTEGER PRIMARY KEY,
			ts         TEXT NOT NULL DEFAULT (datetime('now')),
			a_device_id INTEGER REFERENCES devices(id) ON DELETE CASCADE,
			b_device_id INTEGER REFERENCES devices(id) ON DELETE CASCADE,
			proto      TEXT,
			evidence   TEXT,
			confidence REAL NOT NULL DEFAULT 0,
			status     TEXT NOT NULL DEFAULT 'pending'
			           CHECK (status IN ('pending','accepted','rejected')),
			decided_by TEXT,
			decided_at TEXT)`,
		`CREATE TABLE IF NOT EXISTS dns_records (
			id      INTEGER PRIMARY KEY,
			name    TEXT NOT NULL UNIQUE,
			ip      TEXT NOT NULL,
			kind    TEXT NOT NULL DEFAULT 'A' CHECK (kind IN ('A','CNAME')),
			ttl     INTEGER NOT NULL DEFAULT 60,
			note    TEXT,
			updated TEXT NOT NULL DEFAULT (datetime('now')))`,
		`CREATE INDEX IF NOT EXISTS ix_leases_last ON ip_leases(last_seen DESC)`,
		`CREATE INDEX IF NOT EXISTS ix_leases_mac  ON ip_leases(mac)`,
		`CREATE INDEX IF NOT EXISTS ix_lease_ev_ts ON lease_events(ts DESC)`,
		`CREATE INDEX IF NOT EXISTS ix_reboot_ts   ON device_reboots(ts DESC)`,
		`CREATE INDEX IF NOT EXISTS ix_find_last   ON log_findings(last_ts DESC)`,
		`CREATE INDEX IF NOT EXISTS ix_deliv_ts    ON alert_deliveries(ts DESC)`,

		// seeds: INSERT OR IGNORE keeps an operator's edit intact on restart
		`INSERT OR IGNORE INTO settings(key,value) VALUES
			('poll_interval_s','5'),
			('dns_zone','lab.local'),
			('oncall_overdue_h','1'),
			('handover_valid_h','12')`,
		`INSERT OR IGNORE INTO alert_rules(name,min_severity,cooldown_s,match_like,recipients) VALUES
			('Device unreachable','average',300,'%unreachable%','oncall'),
			('Power loss / restart','average',120,'%restart%','oncall'),
			('Critical event','high',600,NULL,'oncall')`,
		// The classifiers the poller matches syslog against. schema.sql only
		// runs on a fresh database, so an upgrade of an existing lab would
		// otherwise leave log_patterns empty and silently detect nothing.
		`INSERT OR IGNORE INTO log_patterns(name,like_pat,regex,severity,category,hint) VALUES
			('link down','%link down%','(?i)link (is )?down','average','interface','Check the cable and the switch port; if it flaps, replace the patch lead.'),
			('link up','%link up%','(?i)link (is )?up','info','interface','Recovery notice, no action needed.'),
			('ospf neighbour down','%Nbr%down%','(?i)neighbor.*(down|Dead)','average','routing','Check the link to that peer and the OSPF timers on both sides.'),
			('ospf adjacency full','%Full/%',NULL,'info','routing','Adjacency formed, no action needed.'),
			('interface error','%errors%','(?i)(rx|tx)_errors|input errors','warning','interface','Suspect the cable or duplex mismatch; compare error counters over time.'),
			('auth failure','%authentication fail%','(?i)auth(entication)? fail','high','security','Someone is trying to log in with wrong credentials; check the source address.'),
			('dhcp exhausted','%no free leases%','(?i)no (free|available) leases','high','dhcp','The scope is full. Widen the pool or shorten the lease time.'),
			('disk full','%No space left%','(?i)no space left','disaster','system','Free space on the device or rotate logs; writes are already failing.'),
			('process died','%exited%','(?i)(process|service).*(exited|terminated)','high','system','A daemon stopped. Check why it died before restarting it.'),
			('kernel panic','%Kernel panic%','(?i)kernel panic|Oops','disaster','system','The kernel crashed. Capture the trace before rebooting.'),
			('snmp timeout','%Timeout:%','(?i)timeout: no response','warning','monitoring','The device did not answer SNMP. Check reachability and the community string.'),
			('config changed','%Configuration%','(?i)config(uration)? (changed|saved)','info','change','Someone edited the configuration. Confirm it was authorised.')`,
	}
	for _, s := range stmts {
		_, _ = db.Exec(s)
	}

	// additive columns on the pre-existing tables
	for _, a := range []string{
		"ALTER TABLE events ADD COLUMN acked INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE devices ADD COLUMN discovered_at TEXT",
		"ALTER TABLE devices ADD COLUMN community TEXT",
		"ALTER TABLE links ADD COLUMN discovered INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE links ADD COLUMN evidence TEXT",
	} {
		_, _ = db.Exec(a)
	}

	// Make sure the management network is a known subnet. Without a row here
	// every lease falls back to the inferred /24 with no VLAN, and the lease
	// table cannot say which segment an address belongs to.
	seedLanSubnet()

	// Correct the over-broad LIKE on installs seeded before it was narrowed:
	// '%error%' matched sshd "Read error from remote host" lines and filed
	// every dropped SSH session as an interface fault. The findings already
	// written under that rule describe a healthy SSH session, not a fault, so
	// they are cleared rather than left to be acknowledged by hand.
	_, _ = db.Exec(`UPDATE log_patterns SET like_pat='%errors%'
	                WHERE name='interface error' AND like_pat='%error%'`)
	_, _ = db.Exec(`DELETE FROM log_findings WHERE acked=0 AND (line LIKE '%Read error from remote host%'
	                OR line LIKE '%error: send_%' OR line LIKE '%Connection reset by peer%')`)
}

// seedLanSubnet inserts the /24 the routers are managed on, once.
func seedLanSubnet() {
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM subnets").Scan(&count); err != nil || count > 0 {
		return
	}
	var mgmt string
	if err := db.QueryRow(`SELECT mgmt_ip FROM devices WHERE mgmt_ip <> ''
	                       ORDER BY id LIMIT 1`).Scan(&mgmt); err != nil || mgmt == "" {
		return
	}
	parts := strings.Split(mgmt, ".")
	if len(parts) != 4 {
		return
	}
	cidr := strings.Join(parts[:3], ".") + ".0/24"
	var siteID sql.NullInt64
	_ = db.QueryRow("SELECT id FROM sites ORDER BY id LIMIT 1").Scan(&siteID)
	_, _ = db.Exec(`INSERT INTO subnets(cidr, gateway, vlan_id, site_id)
	                VALUES (?,?,?,?)`, cidr, strings.Join(parts[:3], ".")+".1", 1, siteID)
}

// ---------------------------------------------------------------- helpers

// setting returns a value from the settings table, or a default.
func setting(key, def string) string {
	var v string
	if err := db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v); err != nil || v == "" {
		return def
	}
	return v
}

// ---------------------------------------------------------------- leases

// handleLeases answers "which PC holds this address".
//
//	GET /api/leases?q=&online=1&limit=200
//
// The query string matches IP, MAC and device name at once, so the operator
// can paste any of the three identifiers they happen to have.
func handleLeases(w http.ResponseWriter, r *http.Request, u *User) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 2000 {
		limit = 200
	}
	online := r.URL.Query().Get("online")

	q := `SELECT id, ip, mac, COALESCE(hostname,''), COALESCE(device_name,''),
	             vlan_id, COALESCE(subnet,''), COALESCE(seen_on,''),
	             COALESCE(if_index,''), source, is_static, online,
	             first_seen, last_seen, COALESCE(moved_from,'')
	      FROM ip_leases`
	args := []any{}
	var where []string
	if s := strings.TrimSpace(r.URL.Query().Get("q")); s != "" {
		where = append(where, `(ip LIKE ? OR mac LIKE ? OR device_name LIKE ? OR hostname LIKE ?)`)
		like := "%" + strings.ToUpper(s) + "%"
		likeIP := "%" + s + "%"
		args = append(args, likeIP, like, like, like)
	}
	if online == "1" || online == "0" {
		where = append(where, "online = ?")
		args = append(args, online)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY online DESC, last_seen DESC LIMIT ?"
	args = append(args, limit)

	rs, err := db.Query(q, args...)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rs.Close()

	out := []map[string]any{}
	for rs.Next() {
		var id int64
		var ip, mac, host, name, subnet, seenOn, ifIdx, source, first, last, moved string
		var vlan sql.NullInt64
		var isStatic, on int
		if rs.Scan(&id, &ip, &mac, &host, &name, &vlan, &subnet, &seenOn,
			&ifIdx, &source, &isStatic, &on, &first, &last, &moved) != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "ip": ip, "mac": mac, "mac_vendor": ouiVendor(mac),
			"hostname": host, "device_name": name,
			"vlan_id": vlan.Int64, "subnet": subnet, "seen_on": seenOn,
			"if_index": ifIdx, "source": source, "is_static": isStatic == 1,
			"online": on == 1, "first_seen": first, "last_seen": last,
			"moved_from": moved,
		})
	}

	counts := map[string]any{}
	var total, up int
	if err := db.QueryRow("SELECT COUNT(*), COALESCE(SUM(online),0) FROM ip_leases").
		Scan(&total, &up); err == nil {
		counts["total"] = total
		counts["online"] = up
	}
	writeJSON(w, http.StatusOK, map[string]any{"leases": out, "counts": counts})
}

// ouiVendor names the manufacturer from the first three octets. Only the
// prefixes actually present in a small lab are worth hard-coding.
func ouiVendor(mac string) string {
	up := strings.ToUpper(mac)
	switch {
	case strings.HasPrefix(up, "00:0C:29"), strings.HasPrefix(up, "00:50:56"),
		strings.HasPrefix(up, "00:05:69"), strings.HasPrefix(up, "00:1C:14"):
		return "VMware"
	case strings.HasPrefix(up, "52:54:00"):
		return "QEMU/KVM"
	case strings.HasPrefix(up, "08:00:27"):
		return "VirtualBox"
	case strings.HasPrefix(up, "B8:27:EB"), strings.HasPrefix(up, "DC:A6:32"):
		return "Raspberry Pi"
	}
	return ""
}

// handleLeaseEvents returns the new/moved/gone history.
func handleLeaseEvents(w http.ResponseWriter, r *http.Request, u *User) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rs, err := db.Query(`SELECT ts, ip, COALESCE(mac,''), kind, COALESCE(detail,'')
	                     FROM lease_events ORDER BY ts DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rs.Close()
	out := []map[string]any{}
	for rs.Next() {
		var ts, ip, mac, kind, detail string
		if rs.Scan(&ts, &ip, &mac, &kind, &detail) == nil {
			out = append(out, map[string]any{"ts": ts, "ip": ip, "mac": mac,
				"kind": kind, "detail": detail})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePatchLease lets the operator name an unnamed device or pin it static.
func handlePatchLease(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		DeviceName *string `json:"device_name"`
		IsStatic   *bool   `json:"is_static"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.DeviceName != nil {
		_, err := db.Exec("UPDATE ip_leases SET device_name = ? WHERE id = ?", *body.DeviceName, id)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		// record the rename so the history reads sensibly
		var ip, mac string
		_ = db.QueryRow("SELECT ip, mac FROM ip_leases WHERE id = ?", id).Scan(&ip, &mac)
		_, _ = db.Exec(`INSERT INTO lease_events(ip, mac, kind, detail) VALUES (?,?,?,?)`,
			ip, mac, "renamed", "dat ten: "+*body.DeviceName)
	}
	if body.IsStatic != nil {
		v := 0
		if *body.IsStatic {
			v = 1
		}
		_, _ = db.Exec("UPDATE ip_leases SET is_static = ? WHERE id = ?", v, id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- reboot / power

// handleReboots reports restarts and the power-loss inferences.
func handleReboots(w http.ResponseWriter, r *http.Request, u *User) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rs, err := db.Query(`SELECT b.ts, b.device_id, COALESCE(d.hostname,''),
	                            COALESCE(b.prev_uptime_s,0), COALESCE(b.new_uptime_s,0),
	                            COALESCE(b.gap_s,0), b.kind, COALESCE(b.note,'')
	                     FROM device_reboots b LEFT JOIN devices d ON d.id = b.device_id
	                     ORDER BY b.ts DESC, b.id DESC LIMIT ?`, limit)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rs.Close()
	out := []map[string]any{}
	for rs.Next() {
		var ts, host, kind, note string
		var devID int64
		var prev, now, gap int
		if rs.Scan(&ts, &devID, &host, &prev, &now, &gap, &kind, &note) == nil {
			out = append(out, map[string]any{"ts": ts, "device_id": devID,
				"hostname": host, "prev_uptime_s": prev, "new_uptime_s": now,
				"gap_s": gap, "kind": kind, "note": note})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleReachability gives the per-device up/down timeline for the chart.
func handleReachability(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("device"), 10, 64)
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 || hours > 720 {
		hours = 24
	}
	rs, err := db.Query(`SELECT ts, reachable, COALESCE(rtt_ms,0) FROM reach_samples
	                     WHERE device_id = ? AND ts > datetime('now', ?)
	                     ORDER BY ts`, id, fmt.Sprintf("-%d hours", hours))
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rs.Close()
	out := []map[string]any{}
	for rs.Next() {
		var ts string
		var ok, rtt int
		if rs.Scan(&ts, &ok, &rtt) == nil {
			out = append(out, map[string]any{"ts": ts, "reachable": ok == 1, "rtt_ms": rtt})
		}
	}
	// availability as seen by the poller, which is what "was there a power cut"
	// actually means here
	var seen, up int
	_ = db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(reachable),0) FROM reach_samples
	                 WHERE device_id = ? AND ts > datetime('now', ?)`,
		id, fmt.Sprintf("-%d hours", hours)).Scan(&seen, &up)
	pct := 0.0
	if seen > 0 {
		pct = float64(up) / float64(seen) * 100
	}
	writeJSON(w, http.StatusOK, map[string]any{"samples": out,
		"availability_pct": pct, "window_h": hours})
}

// ---------------------------------------------------------------- shifts

// handleShifts lists the roster plus who is on call right now.
func handleShifts(w http.ResponseWriter, r *http.Request, u *User) {
	rs, err := db.Query(`SELECT id, name, email, COALESCE(phone,''), role, days_mask,
	                            starts_at, ends_at, sort_order, active
	                     FROM shift_admins ORDER BY active DESC, sort_order, id`)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rs.Close()
	out := []map[string]any{}
	for rs.Next() {
		var id, mask, order, active int64
		var name, email, phone, role, starts, ends string
		if rs.Scan(&id, &name, &email, &phone, &role, &mask, &starts, &ends,
			&order, &active) == nil {
			out = append(out, map[string]any{"id": id, "name": name, "email": email,
				"phone": phone, "role": role, "days_mask": mask,
				"starts_at": starts, "ends_at": ends, "sort_order": order,
				"active": active == 1,
				"days":   maskToDays(mask)})
		}
	}

	cur, handover, overdue, dueAt := oncallNow()
	var curOut any
	if cur != nil {
		curOut = cur
	}
	var hOut any
	if handover != nil {
		hOut = handover
	}
	writeJSON(w, http.StatusOK, map[string]any{"admins": out, "current": curOut,
		"handover": hOut, "overdue": overdue, "due_at": dueAt,
		"valid_h": setting("handover_valid_h", "12")})
}

var dayNames = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

func maskToDays(mask int64) []string {
	out := []string{}
	for i := 0; i < 7; i++ {
		if mask&(1<<uint(i)) != 0 {
			out = append(out, dayNames[i])
		}
	}
	return out
}

func daysToMask(days []string) int64 {
	var mask int64
	for _, d := range days {
		for i, n := range dayNames {
			if strings.EqualFold(d, n) || strings.EqualFold(d, n[:3]) {
				mask |= 1 << uint(i)
			}
		}
	}
	return mask
}

// oncallNow mirrors the poller's selection so the UI banner and the mail
// recipients can never disagree.
func oncallNow() (admin map[string]any, handover map[string]any, overdue bool, dueAt string) {
	type row struct {
		id    int64
		name  string
		email string
		role  string
		mask  int64
		s, e  string
	}
	load := func(q string, args ...any) []row {
		rs, err := db.Query(q, args...)
		if err != nil {
			return nil
		}
		defer rs.Close()
		out := []row{}
		for rs.Next() {
			var r row
			if rs.Scan(&r.id, &r.name, &r.email, &r.role, &r.mask, &r.s, &r.e) == nil {
				out = append(out, r)
			}
		}
		return out
	}
	admins := load(`SELECT id, name, email, role, days_mask, starts_at, ends_at
	                FROM shift_admins WHERE active = 1 ORDER BY sort_order, id`)
	if len(admins) == 0 {
		return nil, nil, false, ""
	}
	now := time.Now()
	bit := int64(1) << uint((int(now.Weekday())+6)%7) // Go: Sunday=0 -> Monday=0
	hhmm := now.Format("15:04")
	inWindow := func(r row) bool {
		if r.mask&bit == 0 {
			return false
		}
		if r.s <= r.e {
			return r.s <= hhmm && hhmm < r.e
		}
		return hhmm >= r.s || hhmm < r.e
	}

	var from, to sql.NullInt64
	var reason, ts, due sql.NullString
	err := db.QueryRow(`SELECT from_admin, to_admin, reason, ts, due_at
	                    FROM shift_handovers ORDER BY ts DESC, id DESC LIMIT 1`).
		Scan(&from, &to, &reason, &ts, &due)
	if err == nil {
		winH, _ := strconv.Atoi(setting("handover_valid_h", "12"))
		cutoff := now.Add(-time.Duration(winH) * time.Hour)
		fresh := ts.String > cutoff.Format("2006-01-02 15:04:05")
		if due.Valid && due.String != "" {
			if t, e := time.Parse("2006-01-02 15:04:05", due.String); e == nil && t.After(now) {
				fresh = true
			}
			if ts.String < due.String {
				overdue = true
			}
			dueAt = due.String
		}
		switch reason.String {
		case "manual", "overtime", "escalation", "auto", "scheduled":
			if fresh && to.Valid {
				for _, a := range admins {
					if a.id == to.Int64 {
						handover = map[string]any{"ts": ts.String, "reason": reason.String,
							"to_admin": to.Int64, "from_admin": from.Int64,
							"due_at": due.String}
						admin = map[string]any{"id": a.id, "name": a.name,
							"email": a.email, "role": a.role, "source": "handover"}
						return admin, handover, overdue, dueAt
					}
				}
			}
			handover = map[string]any{"ts": ts.String, "reason": reason.String,
				"to_admin": to.Int64, "from_admin": from.Int64,
				"due_at": due.String, "expired": true}
		}
	}

	pick := func(role string) *row {
		for i := range admins {
			if admins[i].role == role && inWindow(admins[i]) {
				return &admins[i]
			}
		}
		return nil
	}
	if a := pick("primary"); a != nil {
		admin = map[string]any{"id": a.id, "name": a.name, "email": a.email,
			"role": a.role, "source": "roster"}
		return admin, handover, overdue, dueAt
	}
	if a := pick("backup"); a != nil {
		admin = map[string]any{"id": a.id, "name": a.name, "email": a.email,
			"role": a.role, "source": "roster-backup"}
		return admin, handover, overdue, dueAt
	}
	for _, a := range admins {
		if a.role == "primary" {
			admin = map[string]any{"id": a.id, "name": a.name, "email": a.email,
				"role": a.role, "source": "fallback"}
			return admin, handover, overdue, dueAt
		}
	}
	admin = map[string]any{"id": admins[0].id, "name": admins[0].name,
		"email": admins[0].email, "role": admins[0].role, "source": "fallback"}
	return admin, handover, overdue, dueAt
}

// handlePutShift creates or updates a roster entry.
func handlePutShift(w http.ResponseWriter, r *http.Request, u *User) {
	var b struct {
		ID       int64    `json:"id"`
		Name     string   `json:"name"`
		Email    string   `json:"email"`
		Phone    string   `json:"phone"`
		Role     string   `json:"role"`
		Days     []string `json:"days"`
		StartsAt string   `json:"starts_at"`
		EndsAt   string   `json:"ends_at"`
		Active   *bool    `json:"active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(b.Name) == "" || !strings.Contains(b.Email, "@") {
		fail(w, http.StatusBadRequest, "name and a valid email are required")
		return
	}
	role := b.Role
	if role != "backup" {
		role = "primary"
	}
	mask := daysToMask(b.Days)
	if mask == 0 {
		mask = 127
	}
	starts := b.StartsAt
	if starts == "" {
		starts = "00:00"
	}
	ends := b.EndsAt
	if ends == "" {
		ends = "23:59"
	}
	active := 1
	if b.Active != nil && !*b.Active {
		active = 0
	}
	if b.ID > 0 {
		_, err := db.Exec(`UPDATE shift_admins SET name=?, email=?, phone=?, role=?,
		                   days_mask=?, starts_at=?, ends_at=?, active=? WHERE id=?`,
			b.Name, b.Email, b.Phone, role, mask, starts, ends, active, b.ID)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": b.ID})
		return
	}
	res, err := db.Exec(`INSERT INTO shift_admins(name,email,phone,role,days_mask,
	                     starts_at,ends_at,active) VALUES (?,?,?,?,?,?,?,?)`,
		b.Name, b.Email, b.Phone, role, mask, starts, ends, active)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, _ := res.LastInsertId()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// handleDeleteShift deactivates rather than deletes, so old handover and
// delivery rows keep a valid foreign key.
func handleDeleteShift(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	_, err := db.Exec("UPDATE shift_admins SET active = 0 WHERE id = ?", id)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleHandover records that the on-call duty moved to another admin.
//
// This is both the scheduled change and the "the previous admin worked
// overtime and never handed over" escape hatch: `reason=overtime` with
// `reassign_now` set makes the incoming person the recipient immediately.
func handleHandover(w http.ResponseWriter, r *http.Request, u *User) {
	var b struct {
		ToAdmin   int64  `json:"to_admin"`
		Reason    string `json:"reason"`
		Note      string `json:"note"`
		DueAt     string `json:"due_at"`
		Redispatch bool  `json:"redispatch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	if b.ToAdmin == 0 {
		fail(w, http.StatusBadRequest, "to_admin is required")
		return
	}
	var exists int
	if err := db.QueryRow("SELECT COUNT(*) FROM shift_admins WHERE id = ? AND active = 1",
		b.ToAdmin).Scan(&exists); err != nil || exists == 0 {
		fail(w, http.StatusBadRequest, "unknown or inactive admin")
		return
	}
	switch b.Reason {
	case "scheduled", "overtime", "manual", "escalation", "auto":
	default:
		b.Reason = "manual"
	}
	var from sql.NullInt64
	if a, _, _, _ := oncallNow(); a != nil {
		if id, ok := a["id"].(int64); ok {
			from = sql.NullInt64{Int64: id, Valid: true}
		}
	}
	_, err := db.Exec(`INSERT INTO shift_handovers(from_admin,to_admin,by_user,reason,note,due_at)
	                   VALUES (?,?,?,?,?,NULLIF(?,''))`,
		from, b.ToAdmin, u.Username, b.Reason, b.Note, b.DueAt)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	res := map[string]any{"ok": true}
	if b.Redispatch {
		n, to := redispatchMail(b.ToAdmin)
		res["mailed"] = n
		res["recipients"] = to
	}
	writeJSON(w, http.StatusOK, res)
}

// handleHandovers returns the history of who passed duty to whom.
func handleHandovers(w http.ResponseWriter, r *http.Request, u *User) {
	rs, err := db.Query(`SELECT h.ts, COALESCE(f.name,''), COALESCE(t.name,''),
	                            COALESCE(h.by_user,''), h.reason, COALESCE(h.note,''),
	                            COALESCE(h.due_at,'')
	                     FROM shift_handovers h
	                     LEFT JOIN shift_admins f ON f.id = h.from_admin
	                     LEFT JOIN shift_admins t ON t.id = h.to_admin
	                     ORDER BY h.ts DESC, h.id DESC LIMIT 100`)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rs.Close()
	out := []map[string]any{}
	for rs.Next() {
		var ts, fn, tn, by, reason, note, due string
		if rs.Scan(&ts, &fn, &tn, &by, &reason, &note, &due) == nil {
			out = append(out, map[string]any{"ts": ts, "from": fn, "to": tn,
				"by_user": by, "reason": reason, "note": note, "due_at": due})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- alerts

// handleAlerts lists the rules and the recent deliveries.
func handleAlerts(w http.ResponseWriter, r *http.Request, u *User) {
	rules := []map[string]any{}
	rr, err := db.Query(`SELECT id, name, enabled, min_severity, COALESCE(match_like,''),
	                            recipients, cooldown_s FROM alert_rules ORDER BY id`)
	if err == nil {
		defer rr.Close()
		for rr.Next() {
			var id, enabled, cool int64
			var name, sev, like, recips string
			if rr.Scan(&id, &name, &enabled, &sev, &like, &recips, &cool) == nil {
				rules = append(rules, map[string]any{"id": id, "name": name,
					"enabled": enabled == 1, "min_severity": sev, "match_like": like,
					"recipients": recips, "cooldown_s": cool})
			}
		}
	}
	deliv := []map[string]any{}
	dr, err := db.Query(`SELECT d.ts, COALESCE(r.name,'(manual)'), COALESCE(d.to_addr,''),
	                            d.status, COALESCE(d.subject,''), COALESCE(d.error,''),
	                            COALESCE(s.name,''), d.kind
	                     FROM alert_deliveries d
	                     LEFT JOIN alert_rules r ON r.id = d.rule_id
	                     LEFT JOIN shift_admins s ON s.id = d.admin_id
	                     ORDER BY d.ts DESC, d.id DESC LIMIT 60`)
	if err == nil {
		defer dr.Close()
		for dr.Next() {
			var ts, rule, to, status, subj, e, who, kind string
			if dr.Scan(&ts, &rule, &to, &status, &subj, &e, &who, &kind) == nil {
				deliv = append(deliv, map[string]any{"ts": ts, "rule": rule, "to": to,
					"status": status, "subject": subj, "error": e, "admin": who,
					"kind": kind})
			}
		}
	}
	var sent, failed, suppressed int
	_ = db.QueryRow(`SELECT
		COALESCE(SUM(status='sent'),0), COALESCE(SUM(status='failed'),0),
		COALESCE(SUM(status='suppressed'),0) FROM alert_deliveries`).
		Scan(&sent, &failed, &suppressed)
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules, "deliveries": deliv,
		"counts": map[string]any{"sent": sent, "failed": failed,
			"suppressed": suppressed}})
}

// handlePutAlertRule edits a rule's threshold, match or recipient policy.
func handlePutAlertRule(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var b struct {
		Enabled     *bool   `json:"enabled"`
		MinSeverity *string `json:"min_severity"`
		MatchLike   *string `json:"match_like"`
		Recipients  *string `json:"recipients"`
		CooldownS   *int    `json:"cooldown_s"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	if b.Enabled != nil {
		v := 0
		if *b.Enabled {
			v = 1
		}
		_, _ = db.Exec("UPDATE alert_rules SET enabled = ? WHERE id = ?", v, id)
	}
	if b.MinSeverity != nil {
		_, _ = db.Exec("UPDATE alert_rules SET min_severity = ? WHERE id = ?",
			*b.MinSeverity, id)
	}
	if b.MatchLike != nil {
		_, _ = db.Exec("UPDATE alert_rules SET match_like = NULLIF(?,'') WHERE id = ?",
			*b.MatchLike, id)
	}
	if b.Recipients != nil {
		_, _ = db.Exec("UPDATE alert_rules SET recipients = ? WHERE id = ?", *b.Recipients, id)
	}
	if b.CooldownS != nil {
		_, _ = db.Exec("UPDATE alert_rules SET cooldown_s = ? WHERE id = ?", *b.CooldownS, id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleTestAlert proves the mail path end to end without waiting for a fault.
func handleTestAlert(w http.ResponseWriter, r *http.Request, u *User) {
	admin, _, _, _ := oncallNow()
	if admin == nil {
		fail(w, http.StatusBadRequest, "no on-call admin configured")
		return
	}
	to, _ := admin["email"].(string)
	subject := "[TEST] lab dashboard alert channel"
	body := fmt.Sprintf("This is a test from the lab dashboard.\n\nOn call: %v <%s>\nTime: %s\n",
		admin["name"], to, time.Now().Format("2006-01-02 15:04:05"))
	ok, errText, transport := mailSend(to, subject, body)
	status := "sent"
	if !ok {
		status = "failed"
	}
	_, _ = db.Exec(`INSERT INTO alert_deliveries(rule_id,event_id,admin_id,to_addr,
	                subject,status,error,tries,kind) VALUES (NULL,NULL,?,?,?,?,?,1,'test')`,
		admin["id"], to, subject, status, errText)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "to": to,
		"transport": transport, "error": errText})
}

// handleRedispatch re-sends still-open alerts to whoever is on call now.
func handleRedispatch(w http.ResponseWriter, r *http.Request, u *User) {
	admin, _, _, _ := oncallNow()
	if admin == nil {
		fail(w, http.StatusBadRequest, "no on-call admin configured")
		return
	}
	id, _ := admin["id"].(int64)
	n, to := redispatchMail(id)
	writeJSON(w, http.StatusOK, map[string]any{"sent": n, "recipients": to})
}

// ---------------------------------------------------------------- log analysis

// handleLogAnalysis returns the grouped automated findings plus the patterns
// that produced them, so the UI can show "what keeps breaking".
func handleLogAnalysis(w http.ResponseWriter, r *http.Request, u *User) {
	since := r.URL.Query().Get("hours")
	if since == "" {
		since = "24"
	}
	hours, err := strconv.Atoi(since)
	if err != nil || hours <= 0 || hours > 720 {
		hours = 24
	}
	rs, err := db.Query(`SELECT f.id, f.ts, f.last_ts, COALESCE(d.hostname,''),
	                            COALESCE(f.line,''), f.severity, COALESCE(f.category,''),
	                            COALESCE(f.hint,''), f.hits, f.acked,
	                            COALESCE(p.name,'')
	                     FROM log_findings f
	                     LEFT JOIN devices d ON d.id = f.device_id
	                     LEFT JOIN log_patterns p ON p.id = f.pattern_id
	                     WHERE f.last_ts > datetime('now', ?)
	                     ORDER BY f.severity DESC, f.hits DESC, f.last_ts DESC
	                     LIMIT 200`, fmt.Sprintf("-%d hours", hours))
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rs.Close()
	findings := []map[string]any{}
	byCat := map[string]int{}
	bySev := map[string]int{}
	for rs.Next() {
		var id, hits, acked int64
		var ts, last, host, line, sev, cat, hint, pat string
		if rs.Scan(&id, &ts, &last, &host, &line, &sev, &cat, &hint, &hits,
			&acked, &pat) != nil {
			continue
		}
		byCat[cat]++
		bySev[sev]++
		findings = append(findings, map[string]any{"id": id, "ts": ts, "last_ts": last,
			"hostname": host, "line": line, "severity": sev, "category": cat,
			"hint": hint, "hits": hits, "acked": acked == 1, "pattern": pat})
	}
	writeJSON(w, http.StatusOK, map[string]any{"findings": findings,
		"by_category": byCat, "by_severity": bySev, "window_h": hours})
}

// handleAckFinding marks one grouped finding as dealt with.
func handleAckFinding(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	_, err := db.Exec("UPDATE log_findings SET acked = 1 WHERE id = ?", id)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAckEvent acknowledges an entry from the live event feed.
func handleAckEvent(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	_, err := db.Exec("UPDATE events SET acked = 1 WHERE id = ?", id)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- discovery

type discoveredLink struct {
	AName string `json:"a"`
	BName string `json:"b"`
	IfA   string `json:"if_a"`
	IfB   string `json:"if_b"`
	Proto string `json:"proto"`
	Note  string `json:"evidence"`
	Conf  float64 `json:"confidence"`
}

// parseOSPFNeighbors turns `show ip ospf neighbor` output into the peer's
// router-id and point-to-point address. FRR prints:
//
//	Neighbor ID     Pri State           Up Time         Address         Interface
//	2.2.2.2           1 Full/DR         00:12:31        10.0.0.2        eth1:10.0.0.1
//
// The router-id is the reliable identity; the address belongs to the /30 and
// is not the peer's management address.
type ospfPeer struct {
	RouterID string
	Addr     string
	State    string
}

func parseOSPFNeighbors(out string) []ospfPeer {
	ipRe := regexp.MustCompile(`^\d{1,3}(?:\.\d{1,3}){3}$`)
	peers := []ospfPeer{}
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "Neighbor ID") ||
			strings.HasPrefix(l, "Total number") {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 4 || !ipRe.MatchString(f[0]) {
			continue
		}
		p := ospfPeer{RouterID: f[0], State: f[2]}
		for _, cand := range f[3:] {
			// strip the "eth1:10.0.0.1" form down to the bare address
			if i := strings.LastIndex(cand, ":"); i >= 0 {
				cand = cand[:i]
			}
			if ipRe.MatchString(cand) && cand != f[0] {
				p.Addr = cand
				break
			}
		}
		if seen[p.RouterID] {
			continue
		}
		seen[p.RouterID] = true
		peers = append(peers, p)
	}
	return peers
}

// parseOSPFIdentity extracts the local router-id from `show ip ospf`. FRR
// prints it inline, as "OSPF Routing Process, Router ID: 1.1.1.1", so the
// address has to be pulled out with a regex rather than by field position.
func parseOSPFIdentity(out string) string {
	re := regexp.MustCompile(`Router ID:?\s+(\d{1,3}(?:\.\d{1,3}){3})`)
	if m := re.FindStringSubmatch(out); len(m) == 2 {
		return m[1]
	}
	return ""
}

// sshRun executes a command on a lab device with the stored credentials.
// Returns "" when the device cannot be reached — discovery then simply
// proposes less, rather than failing the whole request.
func sshRun(host, user, pass, cmd string, port int) (string, error) {
	if port == 0 {
		port = 22
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         8 * time.Second,
	}
	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", host, port), cfg)
	if err != nil {
		return "", err
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, _ := sess.CombinedOutput(cmd)
	return string(out), nil
}

// devicePassword decrypts the stored credential for a device row.
func devicePassword(blob []byte) string {
	key := netdbKey()
	if key == nil || len(blob) == 0 {
		return ""
	}
	p, err := aesDecrypt(key, blob)
	if err != nil {
		return ""
	}
	return p
}

// devicePasswordEnc encrypts a credential for storage.
func devicePasswordEnc(plain string) []byte {
	key := netdbKey()
	if key == nil || plain == "" {
		return nil
	}
	b, err := aesEncrypt(key, plain)
	if err != nil {
		return nil
	}
	return b
}

// handleDiscover walks the routing daemon on every reachable router, derives
// the adjacency graph, and stores what it finds as *proposals*. Nothing is
// committed to the `links` table until an operator accepts it, so a bad
// credential or a half-built lab cannot silently rewrite the topology.
//
// POST /api/topology/discover
func handleDiscover(w http.ResponseWriter, r *http.Request, u *User) {
	type dev struct {
		id       int64
		hostname string
		mgmtIP   string
		user     sql.NullString
		pass     []byte // password_enc is a BLOB and may be NULL
		port     int
	}
	rs, err := db.Query(`SELECT id, hostname, mgmt_ip, username, password_enc, ssh_port
	                     FROM devices WHERE COALESCE(kind_override, kind) IN ('router','l3switch','switch')`)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	var devs []dev
	for rs.Next() {
		var d dev
		if rs.Scan(&d.id, &d.hostname, &d.mgmtIP, &d.user, &d.pass, &d.port) == nil {
			devs = append(devs, d)
		}
	}
	rs.Close()

	if len(devs) == 0 {
		fail(w, http.StatusBadRequest, "no routers in the inventory")
		return
	}

	// Pass 1: learn each device's own OSPF router-id. The adjacency table only
	// names peers by router-id, so without this map there is nothing to resolve
	// the neighbour list against.
	self := map[string]string{}   // hostname -> router-id
	var found []discoveredLink
	var notes []string

	type observed struct {
		host   string
		peers  []ospfPeer
	}
	var seenPeers []observed

	for _, d := range devs {
		pass := devicePassword(d.pass)
		user := "admin"
		if d.user.Valid && d.user.String != "" {
			user = d.user.String
		}
		idOut, err := sshRun(d.mgmtIP, user, pass, "vtysh -c 'show ip ospf'", d.port)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v", d.hostname, err))
			continue
		}
		rid := parseOSPFIdentity(idOut)
		if rid != "" {
			self[d.hostname] = rid
		}
		nOut, err := sshRun(d.mgmtIP, user, pass,
			"vtysh -c 'show ip ospf neighbor'", d.port)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s neighbors: %v", d.hostname, err))
			continue
		}
		peers := parseOSPFNeighbors(nOut)
		if len(peers) == 0 {
			notes = append(notes, d.hostname+": no OSPF neighbours")
		}
		seenPeers = append(seenPeers, observed{d.hostname, peers})
	}

	ridToHost := map[string]string{}
	for host, rid := range self {
		ridToHost[rid] = host
	}

	// Pass 2: turn each adjacency into a host pair. Only Full adjacency counts
	// as a usable link; anything else is a flapping peer worth reporting.
	for _, o := range seenPeers {
		for _, p := range o.peers {
			peerHost, ok := ridToHost[p.RouterID]
			if !ok {
				notes = append(notes, fmt.Sprintf("%s: router-id %s not matched to a device",
					o.host, p.RouterID))
				continue
			}
			if peerHost == o.host {
				continue
			}
			if !strings.HasPrefix(p.State, "Full") {
				notes = append(notes, fmt.Sprintf("%s - %s: adjacency %s, not Full",
					o.host, peerHost, p.State))
				continue
			}
			dup := false
			for _, f := range found {
				if (f.AName == o.host && f.BName == peerHost) ||
					(f.AName == peerHost && f.BName == o.host) {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
			found = append(found, discoveredLink{AName: o.host, BName: peerHost,
				Proto: "ospf",
				Note:  fmt.Sprintf("OSPF Full via %s (router-id %s)", p.Addr, p.RouterID),
				Conf:  0.9})
		}
	}

	// persist as proposals
	ids := map[string]int64{}
	for _, d := range devs {
		ids[d.hostname] = d.id
	}
	pending := 0
	for _, f := range found {
		a, okA := ids[f.AName]
		b, okB := ids[f.BName]
		if !okA || !okB {
			continue
		}
		var existing int
		_ = db.QueryRow(`SELECT COUNT(*) FROM links
		                 WHERE (a_device_id=? AND b_device_id=?) OR (a_device_id=? AND b_device_id=?)`,
			a, b, b, a).Scan(&existing)
		if existing > 0 {
			continue
		}
		var dup int
		_ = db.QueryRow(`SELECT COUNT(*) FROM link_proposals
		                 WHERE status='pending' AND ((a_device_id=? AND b_device_id=?)
		                    OR (a_device_id=? AND b_device_id=?))`, a, b, b, a).Scan(&dup)
		if dup > 0 {
			continue
		}
		_, err := db.Exec(`INSERT INTO link_proposals(a_device_id,b_device_id,proto,evidence,confidence)
		                   VALUES (?,?,?,?,?)`, a, b, f.Proto, f.Note, f.Conf)
		if err == nil {
			pending++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"routers":   len(devs),
		"links":     found,
		"new":       pending,
		"notes":     notes,
		"scanned_at": time.Now().Format("2006-01-02 15:04:05"),
	})
}

// handleProposals lists what discovery wants to add or remove.
func handleProposals(w http.ResponseWriter, r *http.Request, u *User) {
	rs, err := db.Query(`SELECT p.id, p.ts, COALESCE(a.hostname,''), COALESCE(b.hostname,''),
	                            COALESCE(p.proto,''), COALESCE(p.evidence,''),
	                            p.confidence, p.status
	                     FROM link_proposals p
	                     LEFT JOIN devices a ON a.id = p.a_device_id
	                     LEFT JOIN devices b ON b.id = p.b_device_id
	                     WHERE p.status = 'pending'
	                     ORDER BY p.confidence DESC, p.id`)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rs.Close()
	out := []map[string]any{}
	for rs.Next() {
		var id int64
		var ts, a, b, proto, ev, status string
		var conf float64
		if rs.Scan(&id, &ts, &a, &b, &proto, &ev, &conf, &status) == nil {
			out = append(out, map[string]any{"id": id, "ts": ts, "a": a, "b": b,
				"proto": proto, "evidence": ev, "confidence": conf, "status": status})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDecideProposal accepts or rejects one proposal. Accepting writes the
// real `links` row, which is what makes the topology view redraw.
func handleDecideProposal(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var b struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	if b.Action != "accept" && b.Action != "reject" {
		fail(w, http.StatusBadRequest, "action must be accept or reject")
		return
	}
	var a, bid int64
	var proto, ev sql.NullString
	var conf float64
	if err := db.QueryRow(`SELECT a_device_id, b_device_id, proto, evidence, confidence
	                       FROM link_proposals WHERE id = ? AND status='pending'`, id).
		Scan(&a, &bid, &proto, &ev, &conf); err != nil {
		fail(w, http.StatusNotFound, "proposal not found or already decided")
		return
	}
	if b.Action == "accept" {
		_, err := db.Exec(`INSERT INTO links(a_device_id,b_device_id,proto,discovered,evidence)
		                   VALUES (?,?,?,1,?)`, a, bid, proto.String, ev.String)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	_, err := db.Exec(`UPDATE link_proposals SET status=?, decided_by=?, decided_at=datetime('now')
	                   WHERE id=?`,
		map[string]string{"accept": "accepted", "reject": "rejected"}[b.Action],
		u.Username, id)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": b.Action})
}

// handleAddDevice lets the operator introduce a box that discovery has not
// seen yet, which is the "add devices and verify automatic mapping" step.
func handleAddDevice(w http.ResponseWriter, r *http.Request, u *User) {
	var b struct {
		Hostname string `json:"hostname"`
		MgmtIP   string `json:"mgmt_ip"`
		Kind     string `json:"kind"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	b.Hostname = strings.TrimSpace(b.Hostname)
	b.MgmtIP = strings.TrimSpace(b.MgmtIP)
	if b.Hostname == "" || b.MgmtIP == "" {
		fail(w, http.StatusBadRequest, "hostname and mgmt_ip are required")
		return
	}
	kind := b.Kind
	switch kind {
	case "router", "l3switch", "switch", "server":
	default:
		kind = "router"
	}
	var pid sql.NullInt64
	_ = db.QueryRow("SELECT id FROM sites ORDER BY id LIMIT 1").Scan(&pid)

	var encPass any
	if b.Password != "" {
		if e := devicePasswordEnc(b.Password); e != nil {
			encPass = e
		}
	}
	res, err := db.Exec(`INSERT INTO devices(hostname, mgmt_ip, site_id, kind,
	                     kind_confidence, username, password_enc, discovered_at)
	                     VALUES (?,?,?,?,1.0,?,?,datetime('now'))`,
		b.Hostname, b.MgmtIP, pid, kind, b.Username, encPass)
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	id, _ := res.LastInsertId()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// handleDeleteDevice removes a box from the inventory. Discovered links and
// leases cascade away with it.
func handleDeleteDevice(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var host string
	_ = db.QueryRow("SELECT hostname FROM devices WHERE id = ?", id).Scan(&host)
	if host == "" {
		fail(w, http.StatusNotFound, "device not found")
		return
	}
	if _, err := db.Exec("DELETE FROM devices WHERE id = ?", id); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": host})
}

// handlePollInfo reports the configured cadence and how fresh the data is, so
// the UI can prove the 5-second interval instead of asserting it.
func handlePollInfo(w http.ResponseWriter, r *http.Request, u *User) {
	interval, _ := strconv.Atoi(setting("poll_interval_s", "5"))
	var last string
	var n int
	_ = db.QueryRow("SELECT COALESCE(MAX(ts),''), COUNT(*) FROM health_samples").Scan(&last, &n)
	var ageS int
	if t, err := time.Parse("2006-01-02 15:04:05", last); err == nil {
		ageS = int(time.Since(t).Seconds())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"interval_s": interval,
		"last_sample": last,
		"age_s":       ageS,
		"healthy":     ageS <= interval*3,
		"samples":     n,
	})
}

// ---------------------------------------------------------------- dns

// handleDNS lists the records that back the optional hostname access.
func handleDNS(w http.ResponseWriter, r *http.Request, u *User) {
	rs, err := db.Query(`SELECT id, name, ip, kind, ttl, COALESCE(note,''), updated
	                     FROM dns_records ORDER BY name`)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rs.Close()
	out := []map[string]any{}
	have := map[string]bool{}
	for rs.Next() {
		var id int64
		var name, ip, kind, note, upd string
		var ttl int
		if rs.Scan(&id, &name, &ip, &kind, &ttl, &note, &upd) == nil {
			out = append(out, map[string]any{"id": id, "name": name, "ip": ip,
				"kind": kind, "ttl": ttl, "note": note, "updated": upd})
			have[strings.ToLower(name)] = true
		}
	}

	// The inventory already knows every address; offer the ones that have no
	// name yet so the operator does not retype an IP the poller just learned.
	suggest := []map[string]any{}
	if sr, err := db.Query(`SELECT COALESCE(device_name, hostname), ip FROM ip_leases
	                        WHERE COALESCE(device_name, hostname, '') <> ''
	                        GROUP BY 1, 2 ORDER BY 2`); err == nil {
		defer sr.Close()
		for sr.Next() {
			var nm, ip string
			if sr.Scan(&nm, &ip) == nil && !have[strings.ToLower(nm)] {
				suggest = append(suggest, map[string]any{"name": nm, "ip": ip})
			}
		}
	}

	// Report the port the client should point at, not the internal one: the
	// container listens high and is published on 53.
	listen := strings.TrimSpace(os.Getenv("LAB_DNS_LISTEN"))
	public := strings.TrimSpace(os.Getenv("LAB_DNS_PUBLIC_PORT"))
	if listen != "" && public == "" {
		public = "53"
	}
	if listen == "" {
		public = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": out,
		"zone":      setting("dns_zone", "lab.local"),
		"listening": public,
		"suggest":   suggest})
}

// handlePutDNS creates or updates one record.
func handlePutDNS(w http.ResponseWriter, r *http.Request, u *User) {
	var b struct {
		Name string `json:"name"`
		IP   string `json:"ip"`
		TTL  int    `json:"ttl"`
		Note string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	b.Name = strings.ToLower(strings.TrimSpace(b.Name))
	if b.Name == "" || b.IP == "" {
		fail(w, http.StatusBadRequest, "name and ip are required")
		return
	}
	if b.TTL <= 0 {
		b.TTL = 60
	}
	_, err := db.Exec(`INSERT INTO dns_records(name, ip, ttl, note) VALUES (?,?,?,?)
	                   ON CONFLICT(name) DO UPDATE SET ip=excluded.ip, ttl=excluded.ttl,
	                   note=excluded.note, updated=datetime('now')`, b.Name, b.IP, b.TTL, b.Note)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// the zone is served from memory, so refresh it on every write
	reloadDNS()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeleteDNS removes a record.
func handleDeleteDNS(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, err := db.Exec("DELETE FROM dns_records WHERE id = ?", id); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reloadDNS()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- mail shim

// mailSend mirrors the poller's mail path: an SMTP relay when one is
// configured, otherwise a real .eml dropped in the shared /data/outbox. Both
// containers mount the same volume, so the operator can prove the flow works
// even with no mail server in the lab.
func mailSend(to, subject, body string) (ok bool, errText string, transport string) {
	from := env("MAIL_FROM", "lab@localhost")
	host := env("SMTP_HOST", "")
	if host != "" {
		port := envInt("SMTP_PORT", 25)
		addr := fmt.Sprintf("%s:%d", host, port)
		c, err := net.DialTimeout("tcp", addr, 6*time.Second)
		if err != nil {
			return false, "smtp dial: " + err.Error(), "smtp"
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(15 * time.Second))
		tp, err := smtp.NewClient(c, host)
		if err != nil {
			return false, "smtp hello: " + err.Error(), "smtp"
		}
		defer tp.Quit()
		if user := env("SMTP_USER", ""); user != "" {
			_ = tp.Auth(smtp.PlainAuth("", user, env("SMTP_PASS", ""), host))
		}
		if err := tp.Mail(from); err != nil {
			return false, "smtp mail: " + err.Error(), "smtp"
		}
		if err := tp.Rcpt(to); err != nil {
			return false, "smtp rcpt: " + err.Error(), "smtp"
		}
		wc, err := tp.Data()
		if err != nil {
			return false, "smtp data: " + err.Error(), "smtp"
		}
		msg := buildMIME(from, to, subject, body)
		if _, err := wc.Write([]byte(msg)); err != nil {
			return false, "smtp write: " + err.Error(), "smtp"
		}
		if err := wc.Close(); err != nil {
			return false, "smtp close: " + err.Error(), "smtp"
		}
		_ = tp.Quit()
		return true, "", "smtp"
	}

	dir := env("MAIL_DIR", "/data/outbox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, "mkdir: " + err.Error(), "file"
	}
	safe := regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(to, "_")
	if len(safe) > 40 {
		safe = safe[:40]
	}
	fn := filepath.Join(dir, fmt.Sprintf("%d-%s.eml", time.Now().UnixMilli(), safe))
	if err := os.WriteFile(fn, []byte(buildMIME(from, to, subject, body)), 0o644); err != nil {
		return false, "write: " + err.Error(), "file"
	}
	return true, "", "file"
}

// buildMIME renders a minimal RFC 5322 message the operator can open in any
// mail client.
func buildMIME(from, to, subject, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%d.lab@%s>\r\n", time.Now().UnixNano(),
		strings.SplitN(from, "@", 2)[len(strings.SplitN(from, "@", 2))-1])
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return b.String()
}

// redispatchMail re-sends still-open events to one admin.
func redispatchMail(adminID int64) (int, []string) {
	var name, email string
	if err := db.QueryRow("SELECT name, email FROM shift_admins WHERE id = ?",
		adminID).Scan(&name, &email); err != nil {
		return 0, nil
	}
	rs, err := db.Query(`SELECT e.ts, COALESCE(d.hostname,'lab'), e.severity, e.message
	                     FROM events e LEFT JOIN devices d ON d.id = e.device_id
	                     WHERE e.acked = 0 AND e.ts > datetime('now','-8 hours')
	                     ORDER BY e.id DESC LIMIT 40`)
	if err != nil {
		return 0, nil
	}
	defer rs.Close()
	var lines []string
	worst := "info"
	rank := map[string]int{"info": 0, "warning": 1, "average": 2, "high": 3, "disaster": 4}
	for rs.Next() {
		var ts, host, sev, msg string
		if rs.Scan(&ts, &host, &sev, &msg) == nil {
			lines = append(lines, fmt.Sprintf("%-19s %-10s %-9s %s", ts, host, sev, msg))
			if rank[sev] > rank[worst] {
				worst = sev
			}
		}
	}
	if len(lines) == 0 {
		return 0, nil
	}
	subject := fmt.Sprintf("[%s] %d open alert(s) reassigned to %s",
		strings.ToUpper(worst), len(lines), name)
	body := fmt.Sprintf("You are now the on-call administrator. %d unresolved alert(s):\n\n%s\n\n"+
		"The previous shift did not clear these, so they are re-sent to you.\n\n-- lab dashboard\n",
		len(lines), strings.Join(lines, "\n"))
	ok, errText, transport := mailSend(email, subject, body)
	status := "sent"
	if !ok {
		status = "failed"
	}
	_, _ = db.Exec(`INSERT INTO alert_deliveries(rule_id,event_id,admin_id,to_addr,
	                subject,status,error,tries,kind) VALUES (NULL,NULL,?,?,?,?,?,1,'redispatch')`,
		adminID, email, subject, status, errText)
	if !ok {
		return 0, []string{email}
	}
	_ = transport
	return 1, []string{email}
}
