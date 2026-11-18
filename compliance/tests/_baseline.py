#!/usr/bin/env python3
"""Capture a precise behavioural baseline from the Python runner.

The goal is not "does it look right" but "what exactly does it emit", so the Go
port can be diffed against it byte-for-byte rather than eyeballed.
"""
import importlib.util
import json
import sqlite3
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
SCRIPT = HERE.parent / "scripts" / "run_compliance.py"
spec = importlib.util.spec_from_file_location("rc", SCRIPT)
rc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rc)

WORK = HERE / "baseline_work"
WORK.mkdir(exist_ok=True)
actual = WORK / "actual"
if actual.exists():
    for f in actual.glob("*"):
        f.unlink()
actual.mkdir(exist_ok=True)
rc.ACTUAL_DIR = actual

# Fixtures chosen to exercise every branch:
#   r1 compliant, r2 missing one rule, r3 no snapshot at all (ERROR)
#
# r1 uses the FRR 10.6 rendering of loopback passivation ("ip ospf passive"
# inside the interface block) rather than the legacy "passive-interface lo",
# so this baseline also pins that the widened rule accepts the real form.
(actual / "r1.conf").write_text(
    "hostname r1\nrouter ospf\n network 1.1.1.1/32 area 0\n"
    " network 192.168.23.0/24 area 0\ninterface lo\n ip address 1.1.1.1/32\n"
    " ip ospf passive\nexit\n", encoding="utf-8")
(actual / "r2.conf").write_text(
    "hostname r2\nrouter ospf\n network 2.2.2.2/32 area 0\n", encoding="utf-8")

# The loopback-passive rule, kept in sync with config/desired_state.yml.
PASSIVE = (r"^(?:[ \t]*passive-interface lo|interface lo\r?$\n"
           r"(?:[ \t]+[^\n]*\r?\n)*?[ \t]+ip ospf passive)$")

desired = {
    "routers": {
        "r1": {"required_patterns": [
            r"^hostname\s+r1$", r"^router ospf$",
            r"^\s*network 192\.168\.23\.0/24 area 0$",
            r"^\s*network 1\.1\.1\.1/32 area 0$",
            PASSIVE]},
        "r2": {"required_patterns": [
            r"^hostname\s+r2$", r"^router ospf$",
            r"^\s*network 192\.168\.23\.0/24 area 0$"]},
        "r3": {"required_patterns": [r"^hostname\s+r3$"]},
    }
}

results = rc.audit_devices(desired)
print("=== audit_devices output ===")
print(json.dumps(results, ensure_ascii=False, indent=2))

db = WORK / "baseline.db"
if db.exists():
    db.unlink()
run_id = rc.save_results(db, results, "2026-11-16T00:00:00+00:00")
print("\n=== run_id ===")
print(run_id)

with sqlite3.connect(db) as con:
    print("\n=== compliance_runs ===")
    for row in con.execute("SELECT id,checked_at,status,devices_total,devices_compliant,devices_noncompliant,devices_error,source FROM compliance_runs"):
        print(row)
    print("\n=== compliance_findings ===")
    for row in con.execute("SELECT id,run_id,hostname,status,rule,detail FROM compliance_findings ORDER BY id"):
        print(row)

print("\n=== summary block (as the report would print it) ===")
compliant = sum(x["status"] == "COMPLIANT" for x in results)
noncompliant = sum(x["status"] == "NON_COMPLIANT" for x in results)
errors = sum(x["status"] == "ERROR" for x in results)
overall = "ERROR" if errors else ("NON_COMPLIANT" if noncompliant else "COMPLIANT")
print(json.dumps({"total": len(results), "compliant": compliant,
                  "noncompliant": noncompliant, "errors": errors,
                  "status": overall}, indent=2))
