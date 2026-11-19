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

	prevByRule := map[string]string{}
	if prev != nil {
		for _, f := range prev.Findings {
			prevByRule[f.Rule] = f.Status
		}
	}

	out, changed := compareRules(cur, prev, prevByRule)

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

// ruleOut is one declared rule's result, with how it changed since the previous
// audit of this device.
type ruleOut struct {
	Rule    string `json:"rule"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
	Before  string `json:"before,omitempty"`
	Changed bool   `json:"changed"`
}

// compareRules pairs this run's rule results with the previous run's and marks
// the ones whose outcome moved. prevByRule is the previous run's rule->status
// map, passed in so the caller does not have to build it twice.
//
// A compliant device is stored as a single synthetic "All required rules" row
// rather than one row per rule, so a compliant previous run carries no per-rule
// evidence. Taken literally, that makes a rule which passed last time and fails
// now look brand new, "changed" stays false, and the one case the badge exists
// for - a working router that someone broke by hand - is the one case it never
// reports.
//
// The inference is sound because the writer only omits per-rule rows when the
// device was COMPLIANT, which by definition means every declared rule matched.
// A rule missing from a previous COMPLIANT run therefore passed.
//
// It is restricted to NON_COMPLIANT current rows on purpose: an ERROR device
// carries one synthetic "Configuration collection" row, which is not a rule at
// all - nothing was evaluated, so nothing can have changed.
//
// Caveat, stated rather than hidden: if a rule was added to desired_state.yml
// between the two runs, it was never evaluated before, and this reports it as a
// change. That is a rare, human-authored event, and the alternative - staying
// silent about real drift - is worse.
func compareRules(cur, prev *deviceRules, prevByRule map[string]string) ([]ruleOut, int) {
	prevWasClean := prev != nil && prev.Status == "COMPLIANT"

	out := make([]ruleOut, 0, len(cur.Findings))
	changed := 0
	for _, f := range cur.Findings {
		before, seen := prevByRule[f.Rule]
		if !seen && prevWasClean && f.Status == "NON_COMPLIANT" {
			before, seen = "COMPLIANT", true
		}
		ch := seen && before != f.Status
		if ch {
			changed++
		}
		out = append(out, ruleOut{Rule: f.Rule, Status: f.Status, Detail: f.Detail, Before: before, Changed: ch})
	}
	return out, changed
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
