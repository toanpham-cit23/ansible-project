package main

import (
	"net/http"
)

// handleCompliance returns the latest audit and recent history. Authentication
// is enforced by the route registration in main.go.
func handleCompliance(w http.ResponseWriter, r *http.Request, u *User) {
	var latest struct {
		ID           int64  `json:"id"`
		CheckedAt    string `json:"checked_at"`
		Status       string `json:"status"`
		Total        int    `json:"devices_total"`
		Compliant    int    `json:"devices_compliant"`
		NonCompliant int    `json:"devices_noncompliant"`
		Errors       int    `json:"devices_error"`
	}
	err := db.QueryRow(`SELECT id,checked_at,status,devices_total,devices_compliant,devices_noncompliant,devices_error FROM compliance_runs ORDER BY id DESC LIMIT 1`).Scan(
		&latest.ID, &latest.CheckedAt, &latest.Status, &latest.Total, &latest.Compliant, &latest.NonCompliant, &latest.Errors)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"latest": nil, "history": []any{}, "findings": []any{}})
		return
	}
	rows, err := db.Query(`SELECT hostname,status,rule,detail FROM compliance_findings WHERE run_id=? ORDER BY hostname,id`, latest.ID)
	if err != nil {
		fail(w, http.StatusInternalServerError, "compliance findings unavailable")
		return
	}
	defer rows.Close()
	findings := make([]map[string]string, 0)
	for rows.Next() {
		var x mapScan
		if rows.Scan(&x.hostname, &x.status, &x.rule, &x.detail) == nil {
			findings = append(findings, map[string]string{"hostname": x.hostname, "status": x.status, "rule": x.rule, "detail": x.detail})
		}
	}
	hrows, err := db.Query(`SELECT id,checked_at,status,devices_total,devices_compliant,devices_noncompliant,devices_error FROM compliance_runs ORDER BY id DESC LIMIT 20`)
	if err != nil {
		fail(w, http.StatusInternalServerError, "compliance history unavailable")
		return
	}
	defer hrows.Close()
	history := make([]map[string]any, 0)
	for hrows.Next() {
		var id int64
		var ts, status string
		var total, okc, bad, errors int
		if hrows.Scan(&id, &ts, &status, &total, &okc, &bad, &errors) == nil {
			history = append(history, map[string]any{"id": id, "checked_at": ts, "status": status, "devices_total": total, "devices_compliant": okc, "devices_noncompliant": bad, "devices_error": errors})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"latest": latest, "findings": findings, "history": history})
}

type mapScan struct{ hostname, status, rule, detail string }

func ensureComplianceSchema() {
	const schema = `CREATE TABLE IF NOT EXISTS compliance_runs (id INTEGER PRIMARY KEY, checked_at TEXT NOT NULL, status TEXT NOT NULL, devices_total INTEGER NOT NULL, devices_compliant INTEGER NOT NULL, devices_noncompliant INTEGER NOT NULL, devices_error INTEGER NOT NULL, source TEXT NOT NULL DEFAULT 'ansible');
 CREATE TABLE IF NOT EXISTS compliance_findings (id INTEGER PRIMARY KEY, run_id INTEGER NOT NULL REFERENCES compliance_runs(id) ON DELETE CASCADE, hostname TEXT NOT NULL, status TEXT NOT NULL, rule TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '');
 CREATE INDEX IF NOT EXISTS ix_compliance_runs_time ON compliance_runs(checked_at DESC);
 CREATE INDEX IF NOT EXISTS ix_compliance_findings_run ON compliance_findings(run_id);`
	if _, err := db.Exec(schema); err != nil {
		panic("compliance schema: " + err.Error())
	}
}
