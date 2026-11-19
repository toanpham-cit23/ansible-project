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

// The synthetic row a compliant device is stored as. Not a declared rule, but
// the writer emits it so the dashboard can show the device was checked.
const ruleAllOKRow = "All required rules"

// seedRun inserts one audit. findings is (host, status, rule) per row.
func seedRun(t *testing.T, id int64, checkedAt, status string, findings [][3]string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO compliance_runs
		(id,checked_at,status,devices_total,devices_compliant,devices_noncompliant,devices_error)
		VALUES (?,?,'ERROR',1,0,0,0)`, id, checkedAt); err != nil {
		t.Fatalf("seed run %d: %v", id, err)
	}
	for _, f := range findings {
		if _, err := db.Exec(`INSERT INTO compliance_findings (run_id,hostname,status,rule,detail)
			VALUES (?,?,?,?,'')`, id, f[0], f[1], f[2]); err != nil {
			t.Fatalf("seed finding: %v", err)
		}
	}
	_ = status
}

// A device that passed last audit and fails this one must be reported as
// changed. This is the case the page exists for: a working router that someone
// edited by hand. It regressed once - the writer stores only a single
// "All required rules" row for a compliant device, so the previous run had no
// per-rule row to compare against and `changed` stayed false for the exact
// transition the badge is meant to surface.
func TestCompliantToBrokenIsReportedAsChanged(t *testing.T) {
	withTestDB(t)

	// Run 1: r1 fully compliant, so the writer stored one summary row.
	seedRun(t, 1, "2026-11-19T08:01:00+00:00", "COMPLIANT", [][3]string{
		{"r1", "COMPLIANT", ruleAllOKRow},
	})
	// Run 2: one declared rule now fails.
	const broken = "^\\s*network 192\\.168\\.23\\.0/24 area 0$"
	seedRun(t, 2, "2026-11-19T08:02:00+00:00", "NON_COMPLIANT", [][3]string{
		{"r1", "NON_COMPLIANT", broken},
	})

	cur, err := loadDeviceRules(2, "r1")
	if err != nil || cur == nil {
		t.Fatalf("loadDeviceRules: %v %v", cur, err)
	}
	prev, err := loadDeviceRules(1, "r1")
	if err != nil || prev == nil {
		t.Fatalf("loadDeviceRules(prev): %v %v", prev, err)
	}
	if prev.Status != "COMPLIANT" {
		t.Fatalf("previous status = %q, want COMPLIANT", prev.Status)
	}

	before := map[string]string{}
	for _, f := range prev.Findings {
		before[f.Rule] = f.Status
	}
	// Fixture guard: a compliant run really does have no per-rule row for the
	// rule that later breaks. If the writer ever starts storing per-rule rows
	// for compliant devices, this assertion should be revisited deliberately.
	if _, seen := before[broken]; seen {
		t.Fatalf("fixture is wrong: %q should have no row in a compliant run", broken)
	}

	out, changed := compareRules(cur, prev, before)
	if changed != 1 {
		t.Fatalf("changed = %d, want 1: a compliant router that broke a rule must be flagged", changed)
	}
	if !out[0].Changed || out[0].Before != "COMPLIANT" {
		t.Fatalf("rule = %+v, want changed from COMPLIANT", out[0])
	}
}

// An unreachable device stores one "Configuration collection" row. Nothing was
// evaluated, so nothing may be reported as changed even though the previous
// run was compliant - otherwise a network outage would look like drift.
func TestUnreachableIsNotReportedAsChanged(t *testing.T) {
	withTestDB(t)
	seedRun(t, 1, "2026-11-19T08:01:00+00:00", "COMPLIANT", [][3]string{
		{"r3", "COMPLIANT", ruleAllOKRow},
	})
	seedRun(t, 2, "2026-11-19T08:02:00+00:00", "ERROR", [][3]string{
		{"r3", "ERROR", "Configuration collection"},
	})

	cur, err := loadDeviceRules(2, "r3")
	if err != nil || cur == nil {
		t.Fatalf("loadDeviceRules: %v %v", cur, err)
	}
	prev, err := loadDeviceRules(1, "r3")
	if err != nil || prev == nil {
		t.Fatalf("loadDeviceRules(prev): %v %v", prev, err)
	}

	out, changed := compareRules(cur, prev, map[string]string{})
	if changed != 0 {
		t.Fatalf("changed = %d, want 0: an unreachable device is not drift", changed)
	}
	if out[0].Changed || out[0].Status != "ERROR" {
		t.Fatalf("rule = %+v, want an unchanged ERROR row", out[0])
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
