#!/usr/bin/env python3
"""Compliance demos: what the dashboard reports when someone edits a router.

The module's contract is narrow on purpose. It checks whether the rules declared
in compliance/config/desired_state.yml are present in the running-config, records
each audit in SQLite, and shows the result with the time it ran. Configuration
outside that list is never judged, and the module never fixes anything. Status is
COMPLIANT or NON_COMPLIANT; ERROR means the device could not be read at all.

These demos answer the question the contract leaves open: when an operator edits
a router by hand, what does the dashboard actually say? They run the real
auditor (compliance/scripts/run_compliance.py) against real FRR-shaped
snapshots, write to a real SQLite database, and then read the result back with
the same queries the Go endpoint uses - so the panel printed here is what the
Compliance page would render, not a mock-up of it.

Run:  python3 files/ops/demo_compliance.py
Exit: 0 every scenario behaved as designed, 1 otherwise.
"""
from __future__ import annotations

import importlib.util
import pathlib
import shutil
import sqlite3
import sys
import tempfile

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parents[1]
RUNNER = ROOT / "compliance" / "scripts" / "run_compliance.py"

# The auditor is imported, not copied. A demo that reimplemented the matching
# would prove nothing about the shipped tool.
spec = importlib.util.spec_from_file_location("run_compliance", RUNNER)
rc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rc)


# ------------------------------------------------------------------ snapshots
# FRR 10.6 renders loopback passivation as `ip ospf passive` inside the
# interface block, not as `passive-interface lo`. The declared rule accepts both
# forms; these snapshots use the one the live routers actually print, so a pass
# here means the rule works against real output rather than against the old
# syntax the rule was originally written for.
def config(host: str, loopback: str) -> str:
    return (
        f"hostname {host}\n"
        "!\n"
        "interface lo\n"
        f" ip address {loopback}/32\n"
        " ip ospf passive\n"
        "exit\n"
        "!\n"
        "router ospf\n"
        " network 192.168.23.0/24 area 0\n"
        f" network {loopback}/32 area 0\n"
        "exit\n"
        "!\n"
    )


BASE = {
    "r1": config("r1", "1.1.1.1"),
    "r2": config("r2", "2.2.2.2"),
    "r3": config("r3", "3.3.3.3"),
}

# The real rule file, not a toy one. If desired_state.yml changes, these demos
# grade against the new list without being touched.
DESIRED = rc.load_desired_state(ROOT / "compliance" / "config" / "desired_state.yml")


# ------------------------------------------------------------------- harness
class Lab:
    """A throwaway database plus a snapshot directory the auditor reads."""

    def __init__(self) -> None:
        self.tmp = pathlib.Path(tempfile.mkdtemp(prefix="cmpdemo-"))
        self.db = self.tmp / "net.db"
        self.actual = self.tmp / "actual"
        self.actual.mkdir()
        # audit_devices() reads this module-level path, exactly as the real run
        # does when it grades compliance/reports/actual/*.conf.
        rc.ACTUAL_DIR = self.actual
        self.clock = 0

    def close(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def write(self, name: str, body: str | None) -> None:
        """Place (or remove) one router's snapshot. None = the router is gone."""
        path = self.actual / f"{name}.conf"
        if body is None:
            path.unlink(missing_ok=True)
        else:
            path.write_text(body, encoding="utf-8")

    def audit(self) -> list[dict]:
        """One full audit: grade every snapshot, then persist it."""
        self.clock += 1
        stamp = f"2026-11-19T08:{self.clock:02d}:00+00:00"
        results = rc.audit_devices(DESIRED)
        rc.save_results(self.db, results, stamp)
        return results


def dashboard_device(db: pathlib.Path, host: str) -> dict:
    """Read one device the way GET /api/compliance/device does.

    The queries are copied from files/dashboard/docker/api-go/compliance_device.go
    deliberately: the point of the demo is that the page's answer is derivable
    from the stored rows, so the same SQL has to produce it. `before` is the
    rule's result in the previous run that audited this host, and `changed` is
    set only when that result moved.
    """
    con = sqlite3.connect(db)
    try:
        row = con.execute(
            "SELECT id,checked_at FROM compliance_runs ORDER BY id DESC LIMIT 1"
        ).fetchone()
        if row is None:
            return {"hostname": host, "found": False}
        run_id, checked_at = row

        cur = con.execute(
            "SELECT status,rule,detail FROM compliance_findings"
            " WHERE run_id=? AND hostname=? ORDER BY id",
            (run_id, host),
        ).fetchall()
        if not cur:
            # Not part of the latest run - removed from the rule file, or the
            # run never reached it. Report that instead of inventing a status.
            return {"hostname": host, "found": False}

        prev = con.execute(
            "SELECT DISTINCT run_id FROM compliance_findings"
            " WHERE hostname=? AND run_id<? ORDER BY run_id DESC LIMIT 1",
            (host, run_id),
        ).fetchone()

        prev_at = None
        prev_status = None
        before: dict[str, str] = {}
        if prev is not None:
            prev_run = prev[0]
            prev_at = con.execute(
                "SELECT checked_at FROM compliance_runs WHERE id=?", (prev_run,)
            ).fetchone()[0]
            for st, rule in con.execute(
                "SELECT status,rule FROM compliance_findings"
                " WHERE run_id=? AND hostname=? ORDER BY id",
                (prev_run, host),
            ):
                before[rule] = st
                prev_status = st  # every row for a device shares its status

        rules = []
        changed = 0
        prev_was_clean = prev is not None and prev_status == "COMPLIANT"
        for status, rule, detail in cur:
            was = before.get(rule)
            # A compliant device is stored as one synthetic "All required rules"
            # row rather than one row per rule, so a rule that passed last time
            # has no row to compare against. It still passed - that is what
            # COMPLIANT means - so infer it, or the compliant-to-broken
            # transition would be the one case the badge never reports.
            if was is None and prev_was_clean and status == "NON_COMPLIANT":
                was = "COMPLIANT"
            moved = was is not None and was != status
            if moved:
                changed += 1
            rules.append(
                {"rule": rule, "status": status, "detail": detail,
                 "before": was, "changed": moved}
            )

        return {
            "hostname": host,
            "found": True,
            "status": cur[0][0],
            "checked_at": checked_at,
            "previous_at": prev_at,
            "previous_status": prev_status,
            "rules": rules,
            "changed": changed,
        }
    finally:
        con.close()


def render(res: dict, width: int = 66) -> str:
    """The device panel, in the shape the Compliance page draws it."""
    if not res["found"]:
        return f"  {res['hostname']}  (not part of the latest audit)"

    head = f"  {res['hostname']}   {res['status']}"
    if res.get("previous_status"):
        head += f"   previously {res['previous_status']}"
    out = [head, "  " + "-" * width]
    for r in res["rules"]:
        mark = "OK  " if r["status"] == "COMPLIANT" else "FAIL"
        rule = r["rule"]
        if len(rule) > 44:
            rule = rule[:41] + "..."
        badge = ""
        if r["changed"]:
            badge = f"   <-- CHANGED (was {r['before']})"
        out.append(f"    [{mark}] {rule}{badge}")
    tail = f"    {res['changed']} rule(s) changed"
    if res.get("previous_at"):
        tail += f" since {res['previous_at']}"
    out.append(tail)
    return "\n".join(out)


def banner(n: int, title: str) -> None:
    print()
    print(f"{'=' * 72}")
    print(f"DEMO {n}/3 - {title}")
    print("=" * 72)


# ----------------------------------------------------------------- scenarios
def demo_deleted_line() -> bool:
    """An operator deletes a required OSPF line on r1, outside the pipeline."""
    banner(1, "Hand edit breaks a declared rule")
    lab = Lab()
    try:
        lab.write("r1", BASE["r1"])
        lab.write("r2", BASE["r2"])
        lab.write("r3", BASE["r3"])
        first = lab.audit()
        print("\nBaseline audit - all three routers as the playbook built them:")
        for r in first:
            print(f"    {r['hostname']}  {r['status']}")
        if any(r["status"] != "COMPLIANT" for r in first):
            print("  !! baseline is not clean; the fixture no longer matches the rules")
            return False

        # The edit. One line, exactly what `no network 192.168.23.0/24 area 0`
        # leaves behind. Nothing else changes.
        edited = BASE["r1"].replace(" network 192.168.23.0/24 area 0\n", "")
        lab.write("r1", edited)
        print("\nOperator on r1:  conf t / router ospf / no network 192.168.23.0/24 area 0")
        print("No commit to the repository, no Ansible run, no notification.")

        lab.audit()
        r1 = dashboard_device(lab.db, "r1")
        r2 = dashboard_device(lab.db, "r2")
        print("\nNext audit - what the Compliance page shows for r1:")
        print(render(r1))
        print("\nSame audit - r2, which nobody touched:")
        print(render(r2))

        ok = (
            r1["status"] == "NON_COMPLIANT"
            and r1["changed"] == 1
            and any(
                r["changed"] and "192\\.168\\.23\\.0/24" in r["rule"] for r in r1["rules"]
            )
            and r2["status"] == "COMPLIANT"
            and r2["changed"] == 0
        )
        print("\nResult: " + ("PASS" if ok else "FAIL"))
        print("  The deleted line is a declared rule, so its result moved from")
        print("  compliant to non-compliant. The dashboard names the rule and marks")
        print("  it as changed; r2 is untouched and reports no change. The module")
        print("  reports the drift - it does not put the line back.")
        print()
        print("  Note on what is listed: r1 shows one rule because the audit stores")
        print("  the rules that failed, plus a single \"All required rules\" row for a")
        print("  device that passed. It is not a per-rule checklist of the router.")
        return ok
    finally:
        lab.close()


def demo_extra_config() -> bool:
    """The operator adds configuration the rule file never declared."""
    banner(2, "Hand edit outside the rule list is not judged")
    lab = Lab()
    try:
        for name, body in BASE.items():
            lab.write(name, body)
        lab.audit()

        added = (
            "banner motd Hand-edited by the night shift\n"
            "ip route 10.99.0.0/16 192.168.23.1\n"
            "interface eth9\n"
            " ip address 172.16.9.1/24\n"
            "exit\n"
        )
        lab.write("r2", BASE["r2"] + added)
        print("\nOperator on r2 added three things the rule file says nothing about:")
        for line in added.rstrip("\n").split("\n"):
            print(f"    + {line}")

        lab.audit()
        r2 = dashboard_device(lab.db, "r2")
        print("\nNext audit - what the Compliance page shows for r2:")
        print(render(r2))

        ok = r2["status"] == "COMPLIANT" and r2["changed"] == 0
        print("\nResult: " + ("PASS" if ok else "FAIL"))
        print("  r2 is still COMPLIANT and nothing is marked changed, because no")
        print("  declared rule depends on those lines. This is the scope boundary")
        print("  working as specified: the audit answers \"are the required rules")
        print("  present\", not \"is this configuration good\". The banner, the static")
        print("  route and the stray interface are real changes to the router that")
        print("  this module will never report - they need review by other means.")
        return ok
    finally:
        lab.close()


def demo_lost_connection() -> bool:
    """The collector cannot read r3: ERROR, never a compliance verdict."""
    banner(3, "Lost connection is ERROR, not non-compliant")
    lab = Lab()
    try:
        for name, body in BASE.items():
            lab.write(name, body)
        lab.audit()

        # The collector deletes snapshots before collecting, so an unreachable
        # router leaves no file rather than a stale one. Reproduce that state.
        lab.write("r3", None)
        print("\nSSH to r3 fails. The collector removes the old snapshot first, so")
        print("there is nothing to grade - which is the point: a stale file would")
        print("produce a confident verdict about a router nobody could read.")
        lab.audit()

        r3 = dashboard_device(lab.db, "r3")
        print("\nNext audit - what the Compliance page shows for r3:")
        print(render(r3))

        ok_error = (
            r3["status"] == "ERROR"
            and r3["previous_status"] == "COMPLIANT"
            and r3["changed"] == 0
        )
        print("\n  ERROR is not a verdict. It says the device could not be checked,")
        print("  and the page keeps the previous known result alongside it rather")
        print("  than pretending the router became non-compliant. No rule is marked")
        print("  changed, because no rule was evaluated this time.")

        # Recovery.
        lab.write("r3", BASE["r3"])
        lab.audit()
        back = dashboard_device(lab.db, "r3")
        print("\nSSH restored, next audit:")
        print(render(back))

        ok_recover = back["status"] == "COMPLIANT" and back["changed"] == 0
        print("\n  Recovery is COMPLIANT again. It is not flagged as a change: the")
        print("  failed run stored no rule results to compare against, so there is")
        print("  no rule-level baseline for the comparison. Reporting a change here")
        print("  would mean comparing against the run before the outage, which is")
        print("  older than the last check and would misdate the evidence.")

        ok = ok_error and ok_recover
        print("\nResult: " + ("PASS" if ok else "FAIL"))
        return ok
    finally:
        lab.close()


def main() -> int:
    if not RUNNER.is_file():
        print(f"ERROR: auditor not found at {RUNNER}", file=sys.stderr)
        return 2
    results = [demo_deleted_line(), demo_extra_config(), demo_lost_connection()]
    passed = sum(results)
    print()
    print("=" * 72)
    print(f"SUMMARY: {passed}/{len(results)} demos behaved as designed")
    print("=" * 72)
    return 0 if passed == len(results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
