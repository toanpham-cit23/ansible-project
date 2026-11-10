// ops.go — operational status surface.
//
// The controller owns every operation: firewall, certificates, retention and
// snapshots are all run by scripts on the host over SSH. This file only reads
// the status files those scripts leave in /data/ops, and writes small request
// markers that a host cron job picks up. Nothing here shells out.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const opsDir = "/data/ops"

func readOps(name string) map[string]any {
	b, err := os.ReadFile(filepath.Join(opsDir, name+".json"))
	if err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			return m
		}
	}
	return map[string]any{
		"state": "unknown", "ok": false,
		"message": "No status reported yet - the controller script has not run",
	}
}

func humanBytes(n int64) string {
	f := float64(n)
	for _, u := range []string{"B", "KB", "MB", "GB"} {
		if f < 1024 || u == "GB" {
			if u == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1024
	}
	return fmt.Sprintf("%d B", n)
}

// nextClearance mirrors the cron line `0 3 1 1,7 *` without needing a cron
// parser: the next 1 January or 1 July, 03:00 UTC.
func nextClearance() string {
	now := time.Now().UTC()
	y := now.Year()
	for _, cand := range []time.Time{
		time.Date(y, 1, 1, 3, 0, 0, 0, time.UTC),
		time.Date(y, 7, 1, 3, 0, 0, 0, time.UTC),
		time.Date(y+1, 1, 1, 3, 0, 0, 0, time.UTC),
	} {
		if cand.After(now) {
			return cand.Format("2006-01-02 15:04 UTC")
		}
	}
	return "unknown"
}

// handleOps is the single light call the top status bar makes on every view.
func handleOps(w http.ResponseWriter, r *http.Request, u *User) {
	dbSize := int64(0)
	if st, err := os.Stat(dbPath); err == nil {
		dbSize = st.Size()
	}
	inventory := []map[string]any{}
	// effective_kind is computed by the v_inventory view, not stored on
	// devices; querying the table directly for it is a hard SQL error.
	if rs, err := db.Query(`SELECT hostname, mgmt_ip, COALESCE(effective_kind,''),
	                               COALESCE(last_state,'') FROM v_inventory ORDER BY hostname`); err == nil {
		for rs.Next() {
			var h, ip, kind, state string
			if rs.Scan(&h, &ip, &kind, &state) == nil {
				inventory = append(inventory, map[string]any{
					"hostname": h, "ip": ip, "kind": kind, "state": state})
			}
		}
		_ = rs.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"duckdns":         readOps("duckdns"),
		"tls":             readOps("tls"),
		"network":         readOps("network"),
		"cloudflared":     readOps("cloudflared"),
		"firewall":        readOps("firewall"),
		"retention":       readOps("retention"),
		"backup":          readOps("backup"),
		"db_bytes":        dbSize,
		"db_size":         humanBytes(dbSize),
		"inventory":       inventory,
		"next_clearance":  nextClearance(),
		"retention_days":  180,
		"trusted_https":   certPresent(),
	})
}

func certPresent() bool {
	c := env("TLS_CERT", "")
	k := env("TLS_KEY", "")
	if c == "" || k == "" {
		return false
	}
	_, e1 := os.Stat(c)
	_, e2 := os.Stat(k)
	return e1 == nil && e2 == nil
}

// queueOps writes a marker the host worker consumes. The API has no shell and
// no SSH client, so this is the only honest way for a UI button to cause work.
func queueOps(name, content string) error {
	if err := os.MkdirAll(opsDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(opsDir, name+".request"), []byte(content), 0o644)
}

func handleOpsSnapshot(w http.ResponseWriter, r *http.Request, u *User) {
	if err := queueOps("snapshot", "now"); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "queued": true,
		"message": "Snapshot queued; the controller worker runs it within a minute"})
}

func handleOpsRetention(w http.ResponseWriter, r *http.Request, u *User) {
	mode := strings.TrimSpace(r.URL.Query().Get("mode"))
	if mode == "" {
		mode = "dry-run"
	}
	if mode != "dry-run" && mode != "apply" {
		fail(w, http.StatusBadRequest, "mode must be dry-run or apply")
		return
	}
	// An actual clearance is destructive: it needs the word APPLY in the body.
	if mode == "apply" {
		var b struct{ Confirm string `json:"confirm"` }
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.Confirm != "APPLY" {
			fail(w, http.StatusBadRequest, "send {\"confirm\":\"APPLY\"} to run a real clearance")
			return
		}
	}
	content := ""
	if mode == "apply" {
		content = "apply"
	}
	if err := queueOps("retention", content); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "queued": true, "mode": mode,
		"message": "Queued; the controller worker reports the result in the retention widget"})
}

type nodeRow struct {
	ID       int64   `json:"id"`
	Hostname string  `json:"hostname"`
	IP       string  `json:"ip"`
	Kind     string  `json:"kind"`
	State    string  `json:"state"`
	CPU      *float64 `json:"cpu_pct"`
	Mem      *float64 `json:"mem_pct"`
	UptimeS  *int64  `json:"uptime_s"`
	IfUp     *int64  `json:"if_up"`
	IfTotal  *int64  `json:"if_total"`
	RTT      *int64  `json:"rtt_ms"`
	SSH      bool    `json:"ssh_reachable"`
	SSHMs    int64   `json:"ssh_probe_ms"`
}

// handleOpsTelemetry answers the gap-analysis finding that CPU/RAM/uptime were
// collected but never shown per node, and adds a real SSH reachability probe.
func handleOpsTelemetry(w http.ResponseWriter, r *http.Request, u *User) {
	rows := []nodeRow{}
	rs, err := db.Query(`SELECT id, hostname, mgmt_ip,
	                            COALESCE(effective_kind,''), COALESCE(last_state,'')
	                     FROM v_inventory ORDER BY hostname`)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	for rs.Next() {
		var n nodeRow
		var kind, state string
		if rs.Scan(&n.ID, &n.Hostname, &n.IP, &kind, &state) == nil {
			n.Kind, n.State = kind, state
			rows = append(rows, n)
		}
	}
	_ = rs.Close()

	for i := range rows {
		var cpu, mem sql.NullFloat64
		var up, ifUp, ifTotal sql.NullInt64
		_ = db.QueryRow(`SELECT cpu_pct, mem_pct, uptime_ticks, if_up, if_total
		                 FROM health_samples WHERE device_id=? ORDER BY ts DESC LIMIT 1`,
			rows[i].ID).Scan(&cpu, &mem, &up, &ifUp, &ifTotal)
		if cpu.Valid {
			v := cpu.Float64
			rows[i].CPU = &v
		}
		if mem.Valid {
			v := mem.Float64
			rows[i].Mem = &v
		}
		if up.Valid {
			v := up.Int64 / 100 // SNMP ticks are hundredths of a second
			rows[i].UptimeS = &v
		}
		if ifUp.Valid {
			v := ifUp.Int64
			rows[i].IfUp = &v
		}
		if ifTotal.Valid {
			v := ifTotal.Int64
			rows[i].IfTotal = &v
		}
		var rtt sql.NullInt64
		_ = db.QueryRow(`SELECT rtt_ms FROM reach_samples
		                 WHERE device_id=? ORDER BY ts DESC LIMIT 1`,
			rows[i].ID).Scan(&rtt)
		if rtt.Valid {
			v := rtt.Int64
			rows[i].RTT = &v
		}
	}

	// Parallel so four timeouts never add up to a slow page.
	var wg sync.WaitGroup
	for i := range rows {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			t0 := time.Now()
			c, e := net.DialTimeout("tcp", net.JoinHostPort(rows[idx].IP, "22"), 700*time.Millisecond)
			if e == nil {
				rows[idx].SSH = true
				_ = c.Close()
			}
			rows[idx].SSHMs = time.Since(t0).Milliseconds()
		}(i)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"nodes": rows})
}

// handleOpsActivity feeds the dashboard audit widget: recent errors and the
// alert pipeline's most recent deliveries.
func handleOpsActivity(w http.ResponseWriter, r *http.Request, u *User) {
	events := []map[string]any{}
	if rs, err := db.Query(`SELECT e.ts, e.severity, e.message, COALESCE(d.hostname,'')
	                         FROM events e LEFT JOIN devices d ON d.id=e.device_id
	                         ORDER BY e.ts DESC LIMIT 12`); err == nil {
		for rs.Next() {
			var ts, sev, msg, host string
			if rs.Scan(&ts, &sev, &msg, &host) == nil {
				events = append(events, map[string]any{
					"ts": ts, "severity": sev, "message": msg, "device": host})
			}
		}
		_ = rs.Close()
	}
	deliveries := []map[string]any{}
	if rs, err := db.Query(`SELECT ts, COALESCE(kind,''), COALESCE(status,''),
	                               COALESCE(to_addr,'') FROM alert_deliveries
	                         ORDER BY ts DESC LIMIT 8`); err == nil {
		for rs.Next() {
			var ts, ch, st, to string
			if rs.Scan(&ts, &ch, &st, &to) == nil {
				deliveries = append(deliveries, map[string]any{
					"ts": ts, "channel": ch, "status": st, "recipient": to})
			}
		}
		_ = rs.Close()
	}
	rules := []map[string]any{}
	if rs, err := db.Query(`SELECT name, min_severity, enabled FROM alert_rules ORDER BY id`); err == nil {
		for rs.Next() {
			var name, sev string
			var on int
			if rs.Scan(&name, &sev, &on) == nil {
				rules = append(rules, map[string]any{
					"name": name, "severity": sev, "enabled": on == 1})
			}
		}
		_ = rs.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events": events, "deliveries": deliveries, "rules": rules})
}

// handleOpsGaps is the audit behind the separate Gap Analysis section. Each
// entry records what was missing and what was added to close it.
func handleOpsGaps(w http.ResponseWriter, r *http.Request, u *User) {
	cpuRows, memRows, sshRows := 0, 0, 0
	_ = db.QueryRow(`SELECT COUNT(*) FROM health_samples WHERE cpu_pct IS NOT NULL`).Scan(&cpuRows)
	_ = db.QueryRow(`SELECT COUNT(*) FROM health_samples WHERE mem_pct IS NOT NULL`).Scan(&memRows)
	_ = db.QueryRow(`SELECT COUNT(*) FROM reach_samples`).Scan(&sshRows)

	gaps := []map[string]any{
		{"area": "Per-node CPU / RAM", "before": "Collected in health_samples but only shown as an average gauge and three numbers per card",
			"after": "Per-node telemetry table with CPU, RAM, uptime, interface counts and RTT", "status": "closed",
			"evidence": fmt.Sprintf("%d CPU samples, %d RAM samples", cpuRows, memRows)},
		{"area": "SSH reachability", "before": "No indicator at all, despite the terminal feature depending on it",
			"after": "Live TCP/22 probe per node plus a Terminal Console button on each card", "status": "closed",
			"evidence": fmt.Sprintf("%d reachability samples stored", sshRows)},
		{"area": "Database size", "before": "Not visible anywhere", "after": "Shown in the top status bar",
			"status": "closed", "evidence": "read directly from the SQLite file"},
		{"area": "Backup / restore status", "before": "Backups existed on disk, nothing in the UI",
			"after": "Backup widget with last snapshot, size, count, last restore and a queued snapshot trigger",
			"status": "closed", "evidence": "host-side snapshot worker"},
		{"area": "DNS / TLS / firewall / retention", "before": "No operational state exposed",
			"after": "Top status bar and maintenance widget read controller status files", "status": "closed",
			"evidence": "/data/ops/*.json"},
		{"area": "Alert pipeline visibility", "before": "Rules and deliveries existed but were only reachable from the Alerts view",
			"after": "Live error and delivery feed on the main dashboard", "status": "closed",
			"evidence": "events + alert_deliveries"},
		{"area": "Interface error counters", "before": "Not collected by SNMP",
			"after": "Still not collected - needs an OID walk the routers do not currently expose", "status": "open",
			"evidence": "no column in health_samples"},
		{"area": "Per-node disk utilisation", "before": "Served from the health endpoint but not stored per node in health_samples",
			"after": "Shown where available; a dedicated per-node disk sample is still pending", "status": "partial",
			"evidence": "health endpoint supplies disk_pct"},
		{"area": "Public HTTPS", "before": "HTTP on :8080 only",
			"after": "TLS listener on :8443 is ready; a trusted certificate needs a DuckDNS token and subdomain",
			"status": "blocked", "evidence": "certPresent() == " + fmt.Sprint(certPresent())},
	}
	writeJSON(w, http.StatusOK, map[string]any{"gaps": gaps})
}
