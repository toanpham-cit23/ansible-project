#!/usr/bin/env python3
"""Read-only FRRouting configuration compliance audit for Alpine-based routers."""
from __future__ import annotations

import argparse
import datetime as dt
import json
import re
import sqlite3
import subprocess
import sys
from pathlib import Path
from typing import Any

import yaml

ROOT = Path(__file__).resolve().parents[2]
ADDON = Path(__file__).resolve().parents[1]
ACTUAL_DIR = ADDON / "reports" / "actual"
DEFAULT_DB = ROOT / "files" / "dashboard" / "db" / "net.db"

SCHEMA = """
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
"""


def load_desired_state(path: Path) -> dict[str, Any]:
    try:
        data = yaml.safe_load(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise ValueError(f"Không tìm thấy desired-state: {path}") from exc
    except yaml.YAMLError as exc:
        raise ValueError(f"YAML không hợp lệ trong {path}: {exc}") from exc

    if not isinstance(data, dict) or not isinstance(data.get("routers"), dict):
        raise ValueError("desired_state.yml phải có cấu trúc 'routers:' dạng mapping")
    if not data["routers"]:
        raise ValueError("Danh sách routers trong desired_state.yml đang trống")

    for hostname, spec in data["routers"].items():
        if not isinstance(hostname, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+", hostname):
            raise ValueError(f"Tên router không hợp lệ: {hostname!r}")
        if not isinstance(spec, dict) or not isinstance(spec.get("required_patterns"), list):
            raise ValueError(f"Router {hostname}: required_patterns phải là danh sách")
        for pattern in spec["required_patterns"]:
            if not isinstance(pattern, str):
                raise ValueError(f"Router {hostname}: mỗi required_pattern phải là chuỗi")
            try:
                re.compile(pattern, re.MULTILINE)
            except re.error as exc:
                raise ValueError(f"Regex lỗi trên {hostname}: {pattern!r}: {exc}") from exc
    return data


def collect_configs(inventory: Path, playbook: Path, skip_collect: bool) -> tuple[int, str]:
    ACTUAL_DIR.mkdir(parents=True, exist_ok=True)
    if skip_collect:
        return 0, "Bỏ qua bước Ansible theo --skip-collect"

    # Xóa bản thu thập cũ để router mất kết nối không bị chấm nhầm từ file cũ.
    for old_file in ACTUAL_DIR.glob("*.conf"):
        old_file.unlink()

    cmd = ["ansible-playbook", "-i", str(inventory), str(playbook)]
    print("[1/3] Thu thập cấu hình từ router bằng Ansible...", flush=True)
    try:
        proc = subprocess.run(cmd, cwd=ROOT, text=True, capture_output=True, check=False)
    except FileNotFoundError:
        return 127, "Không tìm thấy ansible-playbook. Hãy cài Ansible trên Controller."

    if proc.stdout:
        print(proc.stdout, end="" if proc.stdout.endswith("\n") else "\n")
    if proc.stderr:
        print(proc.stderr, file=sys.stderr, end="" if proc.stderr.endswith("\n") else "\n")
    return proc.returncode, f"Ansible exit code: {proc.returncode}"


def audit_devices(desired: dict[str, Any]) -> list[dict[str, Any]]:
    results: list[dict[str, Any]] = []
    for hostname, spec in desired["routers"].items():
        config_path = ACTUAL_DIR / f"{hostname}.conf"
        if not config_path.is_file():
            results.append({
                "hostname": hostname,
                "status": "ERROR",
                "findings": [{
                    "rule": "Configuration collection",
                    "detail": "Không có file cấu hình mới. Kiểm tra SSH, inventory, vtysh và quyền truy cập.",
                }],
            })
            continue

        actual = config_path.read_text(encoding="utf-8", errors="replace")
        findings = []
        for pattern in spec["required_patterns"]:
            if re.search(pattern, actual, re.MULTILINE) is None:
                findings.append({
                    "rule": pattern,
                    "detail": "Không tìm thấy cấu hình bắt buộc trong running-config.",
                })
        results.append({
            "hostname": hostname,
            "status": "NON_COMPLIANT" if findings else "COMPLIANT",
            "findings": findings,
        })
    return results


def save_results(db_path: Path, results: list[dict[str, Any]], checked_at: str) -> int:
    compliant = sum(item["status"] == "COMPLIANT" for item in results)
    noncompliant = sum(item["status"] == "NON_COMPLIANT" for item in results)
    errors = sum(item["status"] == "ERROR" for item in results)
    if errors:
        overall = "ERROR"
    elif noncompliant:
        overall = "NON_COMPLIANT"
    else:
        overall = "COMPLIANT"

    db_path.parent.mkdir(parents=True, exist_ok=True)
    # `with sqlite3.connect(...)` commits the transaction but does NOT close the
    # connection: sqlite3's context manager only wraps commit/rollback. The
    # handle therefore stays open until it is garbage collected, which on
    # Windows keeps an exclusive lock on the .db file and makes callers that
    # clean up a temp directory fail with WinError 32. Close it explicitly.
    con = sqlite3.connect(db_path, timeout=15)
    try:
        with con:
            con.execute("PRAGMA foreign_keys = ON")
            con.executescript(SCHEMA)
            cur = con.execute(
                """INSERT INTO compliance_runs
                (checked_at,status,devices_total,devices_compliant,devices_noncompliant,devices_error)
                VALUES (?,?,?,?,?,?)""",
                (checked_at, overall, len(results), compliant, noncompliant, errors),
            )
            run_id = int(cur.lastrowid)
            for device in results:
                findings = device["findings"] or [{
                    "rule": "All required rules",
                    "detail": "Tất cả cấu hình bắt buộc đều được tìm thấy.",
                }]
                for finding in findings:
                    con.execute(
                        """INSERT INTO compliance_findings
                        (run_id,hostname,status,rule,detail) VALUES (?,?,?,?,?)""",
                        (run_id, device["hostname"], device["status"], finding["rule"], finding["detail"]),
                    )
    finally:
        con.close()
    return run_id


def main() -> int:
    parser = argparse.ArgumentParser(description="Read-only FRR configuration compliance audit")
    parser.add_argument("--inventory", default="hosts.ini", help="Ansible inventory (default: hosts.ini)")
    parser.add_argument("--db", default=str(DEFAULT_DB), help="SQLite DB shared with Dashboard")
    parser.add_argument("--skip-collect", action="store_true", help="Audit existing .conf files only (testing)")
    args = parser.parse_args()

    inventory = Path(args.inventory).expanduser().resolve()
    desired_path = ADDON / "config" / "desired_state.yml"
    playbook = ADDON / "playbooks" / "collect_configs.yml"
    try:
        desired = load_desired_state(desired_path)
    except ValueError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 2

    if not args.skip_collect and not inventory.is_file():
        print(f"ERROR: Inventory không tồn tại: {inventory}", file=sys.stderr)
        return 2

    rc, collect_message = collect_configs(inventory, playbook, args.skip_collect)
    print(f"[2/3] {collect_message}")
    results = audit_devices(desired)
    checked_at = dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds")
    db_path = Path(args.db).expanduser().resolve()
    try:
        run_id = save_results(db_path, results, checked_at)
    except (OSError, sqlite3.Error) as exc:
        print(f"ERROR: Không lưu được lịch sử SQLite: {exc}", file=sys.stderr)
        return 2

    compliant = sum(x["status"] == "COMPLIANT" for x in results)
    noncompliant = sum(x["status"] == "NON_COMPLIANT" for x in results)
    errors = sum(x["status"] == "ERROR" for x in results)
    overall = "ERROR" if errors else ("NON_COMPLIANT" if noncompliant else "COMPLIANT")
    report = {
        "run_id": run_id,
        "checked_at": checked_at,
        "status": overall,
        "collection_exit_code": rc,
        "summary": {"total": len(results), "compliant": compliant,
                    "noncompliant": noncompliant, "errors": errors},
        "devices": results,
    }
    report_path = ADDON / "reports" / "latest.json"
    report_path.parent.mkdir(parents=True, exist_ok=True)
    report_path.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")

    print("[3/3] Kết quả Compliance:")
    print(json.dumps(report, ensure_ascii=False, indent=2))
    print(f"SQLite: {db_path}")
    print(f"JSON:   {report_path}")
    if rc != 0:
        print("WARNING: Ansible có lỗi; các router không thu thập được được tính là ERROR.", file=sys.stderr)
    return 0 if overall == "COMPLIANT" and rc == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
