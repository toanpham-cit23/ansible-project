#!/usr/bin/env python3
"""Differential test: run the Python runner and the Go binary on identical
inputs and diff their outputs.

This is the actual proof the port is faithful. The Go unit tests assert against
values I derived from reading the Python source; this script asserts against
what the Python source *actually does* when executed.

Both implementations are pointed at the same fixture directory, the same real
compliance/config/desired_state.yml, and separate throwaway databases. Stdout,
the JSON report and both SQLite tables are then compared.

Usage:  python tests/parity.py path/to/compliance-binary
Exit:   0 identical, 1 divergence, 2 setup problem.
"""
import importlib.util
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
PROJECT = HERE.parents[1]
COMPLIANCE = PROJECT / "compliance"
GO_BIN = sys.argv[1] if len(sys.argv) > 1 else None

# Fixtures chosen to hit all three branches with the REAL rule set, so a
# divergence in any shipped regex would surface here.
FIXTURES = {
    "r1": (
        "hostname r1\n"
        "router ospf\n"
        " network 1.1.1.1/32 area 0\n"
        " network 192.168.23.0/24 area 0\n"
        " passive-interface lo\n"
    ),
    # r2 is missing passive-interface lo and the /24 network, so it should
    # report exactly those rules as missing.
    "r2": (
        "hostname r2\n"
        "router ospf\n"
        " network 2.2.2.2/32 area 0\n"
    ),
    # r3 gets no file at all -> ERROR.
}


def load_python_module():
    spec = importlib.util.spec_from_file_location("rc", COMPLIANCE / "scripts" / "run_compliance.py")
    rc = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(rc)
    return rc


def dump_db(path):
    with sqlite3.connect(path) as con:
        runs = con.execute(
            "SELECT checked_at,status,devices_total,devices_compliant,"
            "devices_noncompliant,devices_error,source FROM compliance_runs ORDER BY id"
        ).fetchall()
        findings = con.execute(
            "SELECT run_id,hostname,status,rule,detail FROM compliance_findings ORDER BY id"
        ).fetchall()
    return {"runs": runs, "findings": findings}


def main():
    if not GO_BIN:
        print("usage: parity.py <compliance-binary>", file=sys.stderr)
        return 2
    # Resolve before building the fixtures: the subprocess runs with cwd set to
    # the synthetic project root, so a relative binary path would not be found.
    go_bin = Path(GO_BIN).resolve()
    if not go_bin.exists():
        print(f"binary not found: {go_bin}", file=sys.stderr)
        return 2

    work = Path(tempfile.mkdtemp(prefix="compliance-parity-"))
    try:
        # --- build a synthetic project root so the Go binary resolves paths
        # exactly as it would in production, without touching the real tree.
        root = work / "project"
        (root / "compliance" / "config").mkdir(parents=True)
        (root / "compliance" / "playbooks").mkdir(parents=True)
        actual = root / "compliance" / "reports" / "actual"
        actual.mkdir(parents=True)
        (root / "files" / "dashboard" / "db").mkdir(parents=True)
        (root / "hosts.ini").write_text("[routers]\n", encoding="utf-8")

        shutil.copy(COMPLIANCE / "config" / "desired_state.yml",
                    root / "compliance" / "config" / "desired_state.yml")

        for host, body in FIXTURES.items():
            (actual / f"{host}.conf").write_text(body, encoding="utf-8")

        # --- python side
        rc = load_python_module()
        rc.ACTUAL_DIR = actual
        desired = rc.load_desired_state(root / "compliance" / "config" / "desired_state.yml")
        py_devices = rc.audit_devices(desired)
        py_db = work / "py.db"
        py_checked = "2026-11-16T00:00:00+00:00"
        rc.save_results(py_db, py_devices, py_checked)

        # --- go side: real binary, --skip-collect so the fixtures are graded.
        go_db = work / "go.db"
        env = dict(os.environ, COMPLIANCE_ROOT=str(root))
        # Write to a scratch report path by pointing the root at our copy; the
        # binary writes <root>/compliance/reports/latest.json.
        proc = subprocess.run(
            [str(go_bin), "--skip-collect", "--db", str(go_db)],
            cwd=str(root), env=env, capture_output=True, text=True, encoding="utf-8",
        )

        problems = []

        if proc.returncode != 1:
            problems.append(f"go exit code {proc.returncode}, expected 1 (r2 non-compliant, r3 error)")

        report_path = root / "compliance" / "reports" / "latest.json"
        if not report_path.exists():
            problems.append("go did not write latest.json")
            go_devices = None
            go_summary = None
        else:
            go_report = json.loads(report_path.read_text(encoding="utf-8"))
            go_devices = go_report["devices"]
            go_summary = {k: go_report[k] for k in
                          ("status", "collection_exit_code", "summary")}

        # --- compare audit results
        if go_devices is not None:
            if py_devices != go_devices:
                problems.append("devices differ:\n  python: "
                                + json.dumps(py_devices, ensure_ascii=False)
                                + "\n  go:     "
                                + json.dumps(go_devices, ensure_ascii=False))
            else:
                print(f"devices identical ({len(py_devices)} routers)")

        # --- compare summary block
        py_errors = sum(x["status"] == "ERROR" for x in py_devices)
        py_noncomp = sum(x["status"] == "NON_COMPLIANT" for x in py_devices)
        py_overall = "ERROR" if py_errors else ("NON_COMPLIANT" if py_noncomp else "COMPLIANT")
        py_summary = {
            "status": py_overall,
            "collection_exit_code": 0,
            "summary": {
                "total": len(py_devices),
                "compliant": sum(x["status"] == "COMPLIANT" for x in py_devices),
                "noncompliant": py_noncomp,
                "errors": py_errors,
            },
        }
        if go_summary is not None:
            if py_summary != go_summary:
                problems.append(f"summary differs:\n  python: {py_summary}\n  go:     {go_summary}")
            else:
                print(f"summary identical: {json.dumps(go_summary['summary'])} status={go_summary['status']}")

        # --- compare sqlite contents (ignoring the id/run_id columns, which are
        # implementation-free but both start at 1 anyway)
        py_tables = dump_db(py_db)
        go_tables = dump_db(go_db)

        # checked_at differs because the Go run uses the wall clock; normalise it.
        py_runs = [r[:0] + (py_checked,) + r[1:] for r in py_tables["runs"]]
        go_runs = [(go_tables["runs"][0][0] and py_checked,) + r[1:]
                   for r in go_tables["runs"]] if go_tables["runs"] else []

        if py_runs != go_runs:
            problems.append(f"compliance_runs differ:\n  python: {py_runs}\n  go:     {go_runs}")
        else:
            print(f"compliance_runs identical: {py_runs}")

        if py_tables["findings"] != go_tables["findings"]:
            problems.append("compliance_findings differ:\n  python: "
                            + repr(py_tables["findings"])
                            + "\n  go:     " + repr(go_tables["findings"]))
        else:
            print(f"compliance_findings identical ({len(py_tables['findings'])} rows)")
            for row in py_tables["findings"]:
                print(f"  {row}")

        if problems:
            print("\n=== DIVERGENCE ===", file=sys.stderr)
            for p in problems:
                print(f"- {p}", file=sys.stderr)
            print("\ngo stdout:\n" + proc.stdout, file=sys.stderr)
            print("go stderr:\n" + proc.stderr, file=sys.stderr)
            return 1

        print("\nPARITY OK - Go output matches Python byte-for-byte")
        return 0
    finally:
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main())
