package main

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// handleComplianceDevice returns one device's audit evidence: its status, the
// result of each declared rule, and how that rule result changed since the
// previous audit.
//
// Scope note: this module only checks whether the rules declared in
// desired_state.yml are present in the running-config. Configuration outside
// that list is never judged, and the stored evidence is the rule outcome, not
// the router's configuration text. A "drift" here therefore means a declared
// rule started or stopped matching - not that some arbitrary line changed.
func handleComplianceDevice(w http.ResponseWriter, r *http.Request, u *User) {
	host := strings.TrimSpace(r.URL.Query().Get("host"))
	if host == "" {
		fail(w, http.StatusBadRequest, "host is required")
		return
	}

	var runID int64
	var checkedAt, status string
	err := db.QueryRow(`SELECT id,checked_at FROM compliance_runs ORDER BY id DESC LIMIT 1`).
		Scan(&runID, &checkedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"hostname": host, "found": false})
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "compliance run unavailable")
		return
	}

	cur, err := loadDeviceRules(runID, host)
	if err != nil {
		fail(w, http.StatusInternalServerError, "compliance findings unavailable")
		return
	}
	if cur == nil {
		// The host was not part of the latest run (removed from the rule file
		// since). Report that plainly instead of inventing a status.
		writeJSON(w, http.StatusOK, map[string]any{"hostname": host, "found": false})
		return
	}
	status = cur.Status

	// Previous run that audited this host. Comparing with the immediate
	// previous *run* (not the previous run that happened to differ) is what
	// lets the page say "this rule changed since the last check".
	var prevRun int64
	var prevAt string
	prev := (*deviceRules)(nil)
	if err := db.QueryRow(`SELECT DISTINCT run_id FROM compliance_findings
		WHERE hostname=? AND run_id<? ORDER BY run_id DESC LIMIT 1`, host, runID).Scan(&prevRun); err == nil {
		_ = db.QueryRow(`SELECT checked_at FROM compliance_runs WHERE id=?`, prevRun).Scan(&prevAt)
		prev, _ = loadDeviceRules(prevRun, host)
	}

	type ruleOut struct {
		Rule    string `json:"rule"`
		Status  string `json:"status"`
		Detail  string `json:"detail"`
		Before  string `json:"before,omitempty"`
		Changed bool   `json:"changed"`
	}

	prevByRule := map[string]string{}
	if prev != nil {
		for _, f := range prev.Findings {
			prevByRule[f.Rule] = f.Status
		}
	}

	out := make([]ruleOut, 0, len(cur.Findings))
	changed := 0
	for _, f := range cur.Findings {
		before, seen := prevByRule[f.Rule]
		ch := seen && before != f.Status
		if ch {
			changed++
		}
		out = append(out, ruleOut{Rule: f.Rule, Status: f.Status, Detail: f.Detail, Before: before, Changed: ch})
	}

	resp := map[string]any{
		"hostname":   host,
		"found":      true,
		"status":     status,
		"checked_at": checkedAt,
		"run_id":     runID,
		"rules":      out,
		"changed":    changed,
	}
	if prev != nil {
		resp["previous_at"] = prevAt
		resp["previous_status"] = prev.Status
	}
	writeJSON(w, http.StatusOK, resp)
}

type deviceRules struct {
	Status   string
	Findings []ruleRow
}

type ruleRow struct {
	Rule   string
	Status string
	Detail string
}

// loadDeviceRules reads one host's finding rows for a run. It returns nil when
// the host has no rows in that run, which is different from "no problems".
func loadDeviceRules(runID int64, host string) (*deviceRules, error) {
	rows, err := db.Query(`SELECT status,rule,detail FROM compliance_findings
		WHERE run_id=? AND hostname=? ORDER BY id`, runID, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &deviceRules{Findings: make([]ruleRow, 0, 8)}
	for rows.Next() {
		var st, rule, detail string
		if err := rows.Scan(&st, &rule, &detail); err != nil {
			return nil, err
		}
		out.Findings = append(out.Findings, ruleRow{Rule: rule, Status: st, Detail: detail})
		// Every finding row for a device shares the device's status.
		out.Status = st
	}
	if len(out.Findings) == 0 {
		return nil, nil
	}
	return out, nil
}

// urlEscape is used by tests to build query strings the same way the browser
// does; keeps the handler's parsing honest for hostnames with odd characters.
func urlEscape(s string) string { return url.QueryEscape(s) }
