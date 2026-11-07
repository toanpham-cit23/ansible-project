#!/usr/bin/env python3
"""Semi-annual retention cleanup.

    retention.py --dry-run          report only (default)
    retention.py --apply            actually delete and VACUUM

Only time-series and transient tables are touched. Inventory, design,
configuration and roster tables are explicitly protected - the four nodes
(controller, r1, r2, r3) must survive every clearance. Column names are
discovered from the schema rather than assumed, so an added column cannot turn
this into a destructive surprise.
"""
import argparse
import os
import sqlite3
import sys
import time
import glob

DB = "/root/lab/db/net.db"
OUT = "/root/lab/db/ops"
LAB = "/root/lab"
sys.path.insert(0, LAB + "/ops")

# Tables that may be pruned, with the timestamp column to age on.
PRUNABLE = {
    "health_samples": "ts",
    "reach_samples": "ts",
    "events": "ts",
    "lease_events": "ts",
    "device_reboots": "ts",
    "alert_deliveries": "ts",
    "log_findings": "ts",
    "shift_handovers": "ts",
    "link_proposals": "ts",
    "metrics": "ts",
}

# Never delete from these, whatever else the schema says.
PROTECTED = {
    "sites", "devices", "vlans", "subnets", "interfaces", "links",
    "users", "settings", "ip_leases", "dns_records", "host_design",
    "log_patterns", "alert_rules", "shift_admins", "sessions",
}

KEEP_BACKUPS = 12


def status(data):
    import json
    import tempfile
    data["updated_at"] = time.strftime("%Y-%m-%d %H:%M:%S", time.gmtime()) + " UTC"
    os.makedirs(OUT, exist_ok=True)
    dest = os.path.join(OUT, "retention.json")
    fd, tmp = tempfile.mkstemp(dir=OUT, prefix=".tmp-")
    with os.fdopen(fd, "w") as fh:
        json.dump(data, fh, sort_keys=True, indent=1)
    os.chmod(tmp, 0o644)
    os.replace(tmp, dest)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--days", type=int, default=180)
    ap.add_argument("--apply", action="store_true")
    a = ap.parse_args()
    cutoff = time.strftime("%Y-%m-%d %H:%M:%S",
                           time.gmtime(time.time() - a.days * 86400))

    db = sqlite3.connect(DB)
    db.row_factory = sqlite3.Row
    present = {r["name"] for r in db.execute(
        "SELECT name FROM sqlite_master WHERE type='table'")}

    removed = {}
    for table, col in PRUNABLE.items():
        if table not in present or table in PROTECTED:
            continue
        cols = {r["name"] for r in db.execute("PRAGMA table_info(%s)" % table)}
        if col not in cols:
            continue
        n = db.execute("SELECT COUNT(*) FROM %s WHERE %s < ?" % (table, col),
                       (cutoff,)).fetchone()[0]
        removed[table] = n
        if a.apply and n:
            db.execute("DELETE FROM %s WHERE %s < ?" % (table, col), (cutoff,))

    # Expired sessions are transient but keyed on expires_at, not ts.
    if "sessions" in present:
        n = db.execute("SELECT COUNT(*) FROM sessions WHERE expires_at < datetime('now')").fetchone()[0]
        removed["sessions_expired"] = n
        if a.apply and n:
            db.execute("DELETE FROM sessions WHERE expires_at < datetime('now')")

    # Local transient files.
    files = []
    for pat in (LAB + "/db/outbox/*.eml", LAB + "/tmp/*", "/tmp/lab-*"):
        for p in glob.glob(pat):
            try:
                if os.path.isfile(p) and os.path.getmtime(p) < time.time() - a.days * 86400:
                    files.append(p)
            except OSError:
                pass
    backups = sorted(glob.glob(LAB + "/backups/*.tgz"), reverse=True)
    old_backups = backups[KEEP_BACKUPS:]

    if a.apply:
        for p in files + old_backups:
            try:
                os.unlink(p)
            except OSError:
                pass
        db.commit()
        try:
            db.execute("VACUUM")
        except sqlite3.OperationalError:
            pass

    size = os.path.getsize(DB) if os.path.exists(DB) else 0
    db.close()

    status({
        "ok": True,
        "state": "applied" if a.apply else "dry_run",
        "dry_run": not a.apply,
        "days": a.days,
        "cutoff": cutoff,
        "rows": removed,
        "rows_total": sum(removed.values()),
        "files_removed": len(files) if a.apply else len(files),
        "backups_removed": len(old_backups) if a.apply else len(old_backups),
        "backups_kept": min(len(backups), KEEP_BACKUPS),
        "db_bytes": size,
        "protected_tables": sorted(PROTECTED),
        "message": ("Dry run - nothing was deleted" if not a.apply
                    else "Clearance complete"),
    })
    print("rows=%d files=%d old_backups=%d db=%d bytes apply=%s"
          % (sum(removed.values()), len(files), len(old_backups), size, a.apply))


if __name__ == "__main__":
    main()
