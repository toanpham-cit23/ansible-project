package main

import (
	"database/sql"
	"net/url"
	"testing"
)

// The compliance module's contract, as stated by the owner:
//   - it checks only the declared rules for presence;
//   - status is compliant or non-compliant, and error means "could not reach";
//   - it never edits a router.
//
// These tests pin the parts of that contract that live in this file.

// withTestDB points the package-level handle at a throwaway database that has
// the compliance tables, so the read path is exercised for real rather than
// against a mock.
func withTestDB(t *testing.T) {
	t.Helper()
	prev := db
	conn, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	conn.SetMaxOpenConns(1)
	db = conn
	ensureComplianceSchema()
	t.Cleanup(func() {
		conn.Close()
		db = prev
	})
}

func TestLoadDeviceRulesMissingHostIsNotCompliant(t *testing.T) {
	// A host with no rows must read as "not found", never as compliant.
	// Conflating the two would report a device that was never audited as OK.
	withTestDB(t)
	got, err := loadDeviceRules(1, "__no_such_host__")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("loadDeviceRules for an unknown host = %+v, want nil", got)
	}
}

func TestLoadDeviceRulesReadsStatusAndOrder(t *testing.T) {
	withTestDB(t)
	if _, err := db.Exec(`INSERT INTO compliance_runs (id,checked_at,status,devices_total,devices_compliant,devices_noncompliant,devices_error) VALUES (1,'2026-11-19T00:00:00+00:00','NON_COMPLIANT',1,0,1,0)`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	for _, row := range [][3]string{
		{"r1", "NON_COMPLIANT", "^hostname\\s+r1$"},
		{"r1", "NON_COMPLIANT", "^router ospf$"},
	} {
		if _, err := db.Exec(`INSERT INTO compliance_findings (run_id,hostname,status,rule,detail) VALUES (1,?,?,?,'missing')`,
			row[0], row[1], row[2]); err != nil {
			t.Fatalf("seed finding: %v", err)
		}
	}
	got, err := loadDeviceRules(1, "r1")
	if err != nil {
		t.Fatalf("loadDeviceRules: %v", err)
	}
	if got == nil || len(got.Findings) != 2 {
		t.Fatalf("got %+v, want two rules", got)
	}
	if got.Status != "NON_COMPLIANT" {
		t.Fatalf("status = %q, want NON_COMPLIANT", got.Status)
	}
	if got.Findings[0].Rule != "^hostname\\s+r1$" {
		t.Fatalf("order lost: first rule = %q", got.Findings[0].Rule)
	}
}

func TestURLRoundTrip(t *testing.T) {
	host := "r1"
	if got := urlEscape(host); got != "r1" {
		t.Fatalf("urlEscape(%q) = %q", host, got)
	}
	// A hostname is never escaped in practice, but the query must survive
	// anything odd rather than silently dropping the device.
	if got := urlEscape("a b"); got != url.QueryEscape("a b") {
		t.Fatalf("urlEscape escaped differently from url.QueryEscape: %q", got)
	}
}
