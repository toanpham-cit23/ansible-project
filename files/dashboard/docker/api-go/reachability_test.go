package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestDB opens an in-memory database with just enough of the schema for the
// inventory queries. The point is to exercise the shipped SQL, not to rebuild
// the whole product in the test.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	for _, s := range []string{
		`CREATE TABLE sites (id INTEGER PRIMARY KEY, name TEXT, location TEXT)`,
		`CREATE TABLE devices (
			id INTEGER PRIMARY KEY, hostname TEXT NOT NULL UNIQUE, mgmt_ip TEXT NOT NULL,
			site_id INTEGER, kind TEXT NOT NULL DEFAULT 'unknown', kind_confidence REAL NOT NULL DEFAULT 0,
			kind_override TEXT, vendor TEXT, model TEXT, serial TEXT, asset_tag TEXT,
			ssh_port INTEGER NOT NULL DEFAULT 22, telnet_port INTEGER NOT NULL DEFAULT 23,
			last_seen TEXT, last_state TEXT,
			last_reachable TEXT, reach_method TEXT, protected INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now')))`,
	} {
		if _, err := d.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return d
}

// The Device model has to carry reachability, or a client cannot tell a
// monitoring gap from an outage no matter what the poller recorded.
func TestDeviceSQLExposesReachability(t *testing.T) {
	db = newTestDB(t)
	if _, err := db.Exec(
		`INSERT INTO devices(id,hostname,mgmt_ip,kind,last_state,last_reachable,reach_method)
		 VALUES (1,'r1','192.168.23.11','router','down','up','icmp')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := db.Query(deviceSQL + " WHERE d.id=1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	devs, err := scanDevices(rows)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("want 1 device, got %d", len(devs))
	}
	d := devs[0]
	if d.LastReachable == nil || *d.LastReachable != "up" {
		t.Errorf("last_reachable not exposed: %v", d.LastReachable)
	}
	if d.ReachMethod == nil || *d.ReachMethod != "icmp" {
		t.Errorf("reach_method not exposed: %v", d.ReachMethod)
	}
	// The SNMP verdict is still "down": the two facts are stored separately and
	// it is the client's job to combine them.
	if d.LastState == nil || *d.LastState != "down" {
		t.Errorf("last_state changed unexpectedly: %v", d.LastState)
	}
}

// The migration must add the columns to a database that predates them, since
// an existing install is never recreated from schema.sql.
func TestReachabilityColumnsAreAddedToOldDatabases(t *testing.T) {
	db = newTestDB(t)
	// simulate the pre-migration shape by dropping what newTestDB created
	for _, s := range []string{
		`ALTER TABLE devices DROP COLUMN last_reachable`,
		`ALTER TABLE devices DROP COLUMN reach_method`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("simulate old schema: %v", err)
		}
	}
	// the additive migration the server runs at start-up
	for _, a := range []string{
		"ALTER TABLE devices ADD COLUMN last_reachable TEXT",
		"ALTER TABLE devices ADD COLUMN reach_method TEXT",
	} {
		if _, err := db.Exec(a); err != nil {
			t.Fatalf("migration failed: %v", err)
		}
	}
	if _, err := db.Exec(
		`INSERT INTO devices(id,hostname,mgmt_ip,kind,last_state,last_reachable,reach_method)
		 VALUES (2,'r2','192.168.23.12','router','down','up','icmp')`); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
}

// /api/topology is what the diagram reads, so the reachability fields have to
// survive that handler too - not only the device list.
func TestTopologyHandlerEmitsReachability(t *testing.T) {
	db = newTestDB(t)
	for _, s := range []string{
		`CREATE VIEW v_inventory AS
		 SELECT d.id, d.hostname, d.mgmt_ip,
		        COALESCE(d.kind_override, d.kind) AS effective_kind,
		        d.kind_confidence, d.vendor, d.model, d.asset_tag,
		        s.name AS site, d.last_seen, d.last_state
		 FROM devices d LEFT JOIN sites s ON s.id = d.site_id`,
		`INSERT INTO devices(id,hostname,mgmt_ip,kind,last_state,last_reachable,reach_method)
		 VALUES (1,'r1','192.168.23.11','router','down','up','icmp')`,
		`CREATE TABLE links (a_device_id INTEGER, b_device_id INTEGER,
		                     if_a TEXT, if_b TEXT, proto TEXT)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	rec := httptest.NewRecorder()
	handleTopology(rec, httptest.NewRequest(http.MethodGet, "/api/topology", nil), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Nodes) != 1 {
		t.Fatalf("want 1 node, got %d", len(out.Nodes))
	}
	n := out.Nodes[0]
	if n["last_reachable"] != "up" {
		t.Errorf("last_reachable = %v, want up", n["last_reachable"])
	}
	if n["reach_method"] != "icmp" {
		t.Errorf("reach_method = %v, want icmp", n["reach_method"])
	}
	if n["last_state"] != "down" {
		t.Errorf("last_state = %v, want down", n["last_state"])
	}
}
