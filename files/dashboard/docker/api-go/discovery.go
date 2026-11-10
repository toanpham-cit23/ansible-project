// B.9 - New Device Discovery.
//
// HONEST STATEMENT OF CURRENT STATE (do not delete; the report relies on it):
//
//   Already present before this change:
//     * docker/poller/poller.py::arp_entries() walks ipNetToMediaTable on each
//       router and collect_leases() writes ip_leases + lease_events(kind='new').
//       So first_seen / last_seen / seen_on / MAC / OUI vendor ARE collected.
//     * files/ops/network-sync.sh re-syncs DuckDNS A records and the local DNS
//       override after an addressing change. It does NOT scan for devices.
//     * app.html had a "Discovery" menu entry, but it pointed at the Setup
//       view (#/setup) whose content is devices + OSPF link proposals + DNS.
//       There was no view dedicated to newly seen hosts.
//
//   What this file adds:
//     * A real ledger table (discovered_devices) that is fed, idempotently,
//       from ip_leases. "Unknown" = a binding whose IP is not in `devices`
//       and whose MAC is not in host_design (when that table exists).
//     * GET /api/discovery exposes that ledger, the "new" lease events and an
//       explicit list of what is still NOT implemented.
//     * Admin-only acknowledge / ignore / note actions.
//
//   Still NOT implemented (partial, on purpose):
//     * No LLDP/CDP neighbour walk, no mDNS/SSDP, no active port scanning and
//       no DHCP-snooping feed. Detection is passive, from ARP only.
//     * No automatic promotion into `devices`; a human still adds a box in
//       Setup. The Bot API likewise cannot enumerate group members, which is
//       why the roster stays the source of truth for mentions.
//     * network-sync.sh is a controller script outside the dashboard; it was
//       deliberately NOT modified here (dashboard-only change).
package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
)

func ensureDiscoverySchema() {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS discovered_devices (
			id          INTEGER PRIMARY KEY,
			mac         TEXT NOT NULL,
			ip          TEXT NOT NULL DEFAULT '',
			hostname    TEXT NOT NULL DEFAULT '',
			device_name TEXT NOT NULL DEFAULT '',
			vendor      TEXT NOT NULL DEFAULT '',
			seen_on     TEXT NOT NULL DEFAULT '',
			if_name     TEXT NOT NULL DEFAULT '',
			source      TEXT NOT NULL DEFAULT 'arp',
			first_seen  TEXT NOT NULL DEFAULT (datetime('now')),
			last_seen   TEXT NOT NULL DEFAULT (datetime('now')),
			online      INTEGER NOT NULL DEFAULT 1,
			known       INTEGER NOT NULL DEFAULT 0,
			state       TEXT NOT NULL DEFAULT 'new'
			            CHECK (state IN ('new','acknowledged','ignored')),
			note        TEXT NOT NULL DEFAULT '',
			reviewed_by TEXT NOT NULL DEFAULT '',
			reviewed_at TEXT,
			UNIQUE (mac, ip))`,
		`CREATE INDEX IF NOT EXISTS ix_discovered_seen ON discovered_devices(first_seen DESC)`,
		`CREATE INDEX IF NOT EXISTS ix_discovered_state ON discovered_devices(known, state)`,
	} {
		if _, err := db.Exec(q); err != nil {
			log.Println("discovery schema:", err)
		}
	}
}

func tableExists(name string) bool {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return n > 0
}

// syncDiscovery mirrors the poller's ARP-derived lease table into the B.9
// ledger. It is idempotent: an operator's state / note / reviewer are never
// overwritten by a later poll.
func syncDiscovery() error {
	knownExpr := `(SELECT COUNT(*) FROM devices d WHERE d.mgmt_ip = l.ip)`
	if tableExists("host_design") {
		knownExpr += ` + (SELECT COUNT(*) FROM host_design h WHERE UPPER(h.mac) = UPPER(l.mac))`
	}
	q := fmt.Sprintf(`SELECT l.ip, l.mac, COALESCE(l.hostname,''), COALESCE(l.device_name,''),
	                        COALESCE(l.seen_on,''), COALESCE(l.switch_name,''),
	                        COALESCE(l.source,'arp'), l.first_seen, l.last_seen,
	                        COALESCE(l.online,0), (%s) AS hits
	                 FROM ip_leases l`, knownExpr)
	rs, err := db.Query(q)
	if err != nil {
		return err
	}
	defer rs.Close()
	type row struct {
		ip, mac, host, name, seenOn, ifName, source, first, last string
		online, hits                                             int
	}
	rows := []row{}
	for rs.Next() {
		var r row
		if rs.Scan(&r.ip, &r.mac, &r.host, &r.name, &r.seenOn, &r.ifName,
			&r.source, &r.first, &r.last, &r.online, &r.hits) == nil {
			rows = append(rows, r)
		}
	}
	for _, r := range rows {
		known := 0
		if r.hits > 0 {
			known = 1
		}
		_, _ = db.Exec(`INSERT INTO discovered_devices
		                (mac,ip,hostname,device_name,vendor,seen_on,if_name,source,
		                 first_seen,last_seen,online,known)
		                VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		                ON CONFLICT(mac,ip) DO UPDATE SET
		                  hostname = excluded.hostname, device_name = excluded.device_name,
		                  vendor = excluded.vendor, seen_on = excluded.seen_on,
		                  if_name = excluded.if_name, last_seen = excluded.last_seen,
		                  online = excluded.online, known = excluded.known`,
			r.mac, r.ip, r.host, r.name, ouiVendor(r.mac), r.seenOn, r.ifName,
			r.source, r.first, r.last, r.online, known)
	}
	return nil
}

// GET /api/discovery - the dedicated B.9 view's only data source.
func handleDiscovery(w http.ResponseWriter, r *http.Request, u *User) {
	syncErr := syncDiscovery()
	rs, err := db.Query(`SELECT id, mac, ip, hostname, device_name, vendor, seen_on,
	                            if_name, source, first_seen, last_seen, online, known,
	                            state, COALESCE(note,''), COALESCE(reviewed_by,'')
	                     FROM discovered_devices
	                     ORDER BY (known = 0) DESC, first_seen DESC, id DESC LIMIT 300`)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rs.Close()
	all := []map[string]any{}
	for rs.Next() {
		var id, online, known int64
		var mac, ip, host, name, vendor, seenOn, ifName, source, first, last, state, note, by string
		if rs.Scan(&id, &mac, &ip, &host, &name, &vendor, &seenOn, &ifName, &source,
			&first, &last, &online, &known, &state, &note, &by) == nil {
			all = append(all, map[string]any{
				"id": id, "mac": mac, "ip": ip, "hostname": host, "device_name": name,
				"vendor": vendor, "seen_on": seenOn, "if_name": ifName, "source": source,
				"first_seen": first, "last_seen": last, "online": online == 1,
				"known": known == 1, "state": state, "note": note, "reviewed_by": by,
			})
		}
	}
	candidates := []map[string]any{}
	for _, d := range all {
		if d["known"] == false {
			candidates = append(candidates, d)
		}
	}

	events := []map[string]any{}
	if ers, e := db.Query(`SELECT ts, ip, mac, COALESCE(detail,'') FROM lease_events
	                       WHERE kind IN ('new','moved') ORDER BY id DESC LIMIT 25`); e == nil {
		defer ers.Close()
		for ers.Next() {
			var ts, ip, mac, detail string
			if ers.Scan(&ts, &ip, &mac, &detail) == nil {
				events = append(events, map[string]any{"ts": ts, "ip": ip,
					"mac": mac, "detail": detail})
			}
		}
	}

	syncNote := "ok"
	if syncErr != nil {
		syncNote = "sync error: " + syncErr.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"candidates": candidates,
		"ledger":     all,
		"events":     events,
		"counts": map[string]any{
			"unknown":       len(candidates),
			"ledger":        len(all),
			"new":           countDiscoveryState("new", false),
			"acknowledged":  countDiscoveryState("acknowledged", false),
			"ignored":       countDiscoveryState("ignored", false),
			"known_inventory": countDiscoveryState("", true),
		},
		"sync": syncNote,
		"sources": []string{
			"docker/poller/poller.py -> arp_entries() / collect_leases() -> ip_leases",
			"lease_events(kind='new'|'moved') for the appearance timeline",
			"devices.mgmt_ip and host_design.mac decide whether a host is already known",
			"files/ops/network-sync.sh refreshes DNS/addressing only; it is not a device scanner",
		},
		"limitations": []string{
			"Passive ARP only: no LLDP/CDP, mDNS, DHCP snooping or active probing.",
			"Automatic promotion into the devices inventory is not implemented; add a box manually in Setup.",
			"The legacy 'Run discovery' button in Setup reads OSPF adjacencies for LINK proposals, a different feature.",
			"A host that never emits ARP (or sits behind a NAT) cannot be detected by this pipeline.",
		},
	})
}

func countDiscoveryState(state string, known bool) int {
	var n int
	q := `SELECT COUNT(*) FROM discovered_devices WHERE known = ?`
	args := []any{0}
	if known {
		args = []any{1}
	}
	if state != "" {
		q += ` AND state = ?`
		args = append(args, state)
	}
	_ = db.QueryRow(q, args...).Scan(&n)
	return n
}

// POST /api/discovery/action - admin only (defense in depth, see 4.1c).
func handleDiscoveryAction(w http.ResponseWriter, r *http.Request, u *User) {
	var b struct {
		ID     int64  `json:"id"`
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	state := ""
	switch strings.ToLower(strings.TrimSpace(b.Action)) {
	case "ack", "acknowledge":
		state = "acknowledged"
	case "ignore":
		state = "ignored"
	case "reopen":
		state = "new"
	default:
		fail(w, http.StatusBadRequest, "action must be acknowledge, ignore or reopen")
		return
	}
	res, err := db.Exec(`UPDATE discovered_devices SET state = ?, note = ?,
	                     reviewed_by = ?, reviewed_at = datetime('now') WHERE id = ?`,
		state, truncate(strings.TrimSpace(b.Note), 500), u.Username, b.ID)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(w, http.StatusNotFound, "không tìm thấy bản ghi discovery")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": state})
}
