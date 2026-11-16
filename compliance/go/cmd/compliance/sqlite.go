package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// schema is byte-identical to the Python SCHEMA constant and to
// ensureComplianceSchema in files/dashboard/docker/api-go/compliance.go. All
// three create the same tables, so whichever component starts first wins and
// the others find the tables already present.
//
// Keep this statement in sync with those two if it ever changes: the dashboard
// reads these columns by name.
const schema = `
CREATE TABLE IF NOT EXISTS compliance_runs (
    id INTEGER PRIMARY KEY,
    checked_at TEXT NOT NULL,
    status TEXT NOT NULL,
    devices_total INTEGER NOT NULL,
    devices_compliant INTEGER NOT NULL,
    devices_noncompliant INTEGER NOT NULL,
    devices_error INTEGER NOT NULL,
    source TEXT NOT NULL DEFAULT 'ansible'
);
CREATE TABLE IF NOT EXISTS compliance_findings (
    id INTEGER PRIMARY KEY,
    run_id INTEGER NOT NULL REFERENCES compliance_runs(id) ON DELETE CASCADE,
    hostname TEXT NOT NULL,
    status TEXT NOT NULL,
    rule TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_compliance_runs_time
    ON compliance_runs(checked_at DESC);
CREATE INDEX IF NOT EXISTS ix_compliance_findings_run
    ON compliance_findings(run_id);
`

// openComplianceDB opens the shared dashboard database.
//
// The pragmas mirror the Python connection (foreign_keys ON) and the Go API's
// DSN (busy_timeout, WAL). WAL is what makes it safe for the dashboard API and
// this collector to touch the same file from different processes: without it a
// concurrent read during a run returns SQLITE_BUSY.
func openComplianceDB(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single writer is enough here and avoids lock contention with the API.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, err
	}
	if err := ensureSchema(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func ensureSchema(conn *sql.DB) error {
	if _, err := conn.Exec(schema); err != nil {
		return fmt.Errorf("compliance schema: %w", err)
	}
	return nil
}

// saveResults inserts one run plus its findings in a single transaction and
// returns the new run id.
//
// A compliant device still gets a finding row ("All required rules"), matching
// the Python behaviour. That keeps the dashboard's findings list complete: an
// operator can see that r1 was checked, not merely that r1 had no problems.
func saveResults(dbPath string, devices []Device, checkedAt string) (int64, error) {
	conn, err := openComplianceDB(dbPath)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	sum := summarise(devices)
	overall := overallStatus(sum)

	tx, err := conn.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(
		`INSERT INTO compliance_runs
         (checked_at,status,devices_total,devices_compliant,devices_noncompliant,devices_error)
         VALUES (?,?,?,?,?,?)`,
		checkedAt, overall, len(devices), sum.Compliant, sum.NonCompliant, sum.Errors,
	)
	if err != nil {
		return 0, err
	}
	runID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	stmt, err := tx.Prepare(
		`INSERT INTO compliance_findings (run_id,hostname,status,rule,detail) VALUES (?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	for _, d := range devices {
		findings := d.Findings
		if len(findings) == 0 {
			findings = []Finding{{Rule: ruleAllOK, Detail: detailAllOK}}
		}
		for _, f := range findings {
			if _, err := stmt.Exec(runID, d.Hostname, d.Status, f.Rule, f.Detail); err != nil {
				return 0, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return runID, nil
}
