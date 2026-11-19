#!/usr/bin/env python3
"""Offline regression test for files/ops/fw-guard.sh.

The real guard runs on Alpine with iptables, crond and /root/lab. This harness
runs a copy of the script in a temp directory with stubbed iptables/rc-service/
python3 on PATH, so the 180-second deadline logic can be verified from Windows
without touching a firewall or the controller.

Run:  python3 files/ops/test_fw_guard.py
Exit: 0 all cases pass, 1 a case failed.
"""
import os
import pathlib
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import time

HERE = pathlib.Path(__file__).resolve().parent
GUARD = HERE / "fw-guard.sh"

IPT = """#!/bin/sh
# Minimal iptables stub: track the LABFW chain and the INPUT attachment.
STATE="$STUB_STATE"
LOG="$STUB_LOG"
printf '%s\\n' "$*" >> "$LOG"
[ -f "$STATE" ] || printf 'chain=0\\ninput=0\\n' > "$STATE"
get() { sed -n "s/^$1=//p" "$STATE"; }
setv() { sed -i.bak "s/^$1=.*/$1=$2/" "$STATE"; rm -f "$STATE.bak"; }
for a in "$@"; do
  case "$a" in
    -N) setv chain 1 ;;
    -X) setv chain 0; setv input 0 ;;
    -I) setv input 1 ;;
    -D) setv input 0 ;;
  esac
done
case "$1" in
  -nL) [ "$(get input)" = 1 ] && printf 'LABFW\\n' ;;
esac
exit 0
"""

RC = """#!/bin/sh
exit 0
"""

PYSTUB = """#!/bin/sh
# Record the status.py invocation instead of writing to /root/lab.
printf '%s\\n' "$*" >> "$STUB_LOG"
exit 0
"""


def write_exec(path, body):
    path.write_text(body, encoding="utf-8", newline="\n")
    path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


def main():
    problems = []
    work = pathlib.Path(tempfile.mkdtemp(prefix="fwguard-test-"))
    try:
        bin_ = work / "bin"
        bin_.mkdir()
        lab = work / "lab"
        ops = lab / "ops"
        ops.mkdir(parents=True)
        state = work / "iptables.state"
        log = work / "calls.log"

        write_exec(bin_ / "iptables", IPT)
        write_exec(bin_ / "rc-service", RC)
        write_exec(bin_ / "python3", PYSTUB)

        body = GUARD.read_text(encoding="utf-8")
        body = body.replace("LAB=/root/lab", "LAB=" + str(lab).replace("\\", "/"))
        # Bind the stubs by absolute path. PATH shims are not reliable when sh
        # is invoked from Windows through Git Bash.
        body = re.sub(r"\biptables\b", '"$STUB_BIN/iptables"', body)
        body = body.replace('python3 "$S"', '"$STUB_BIN/python3" "$S"')
        body = re.sub(r"\brc-service\b", '"$STUB_BIN/rc-service"', body)
        script = work / "fw-guard.sh"
        write_exec(script, body)

        env = dict(os.environ)
        env["STUB_BIN"] = str(bin_).replace("\\", "/")
        env["STUB_STATE"] = str(state).replace("\\", "/")
        env["STUB_LOG"] = str(log).replace("\\", "/")

        sh_path = shutil.which("sh")
        if not sh_path:
            raise SystemExit("sh not found; run this harness from Git Bash")

        def run(*args):
            # Windows cannot exec a .sh directly; go through the POSIX shell.
            return subprocess.run([sh_path, str(script), *args], env=env,
                                  capture_output=True, text=True)

        def deadline():
            p = lab / "db" / "ops" / "fw.deadline"
            return int(p.read_text()) if p.exists() else None

        def attached():
            r = subprocess.run([sh_path, "-c", '"$STUB_BIN/iptables" -nL INPUT'], env=env,
                               capture_output=True, text=True)
            return "LABFW" in r.stdout

        # 1. apply arms a 180 s deadline and attaches the chain.
        r = run("apply")
        if r.returncode != 0:
            problems.append(f"apply exited {r.returncode}: {r.stderr.strip()}")
        dl = deadline()
        now = int(time.time())
        if dl is None or not (now + 175 <= dl <= now + 181):
            problems.append(f"deadline {dl} not ~now+180 (now={now})")
        if not attached():
            problems.append("apply did not attach LABFW to INPUT")
        if "180" not in r.stdout:
            problems.append(f"apply output missing 180s hint: {r.stdout!r}")

        # 2. guard before expiry must not roll back.
        r = run("guard")
        if "ROLLED BACK" in r.stdout:
            problems.append("guard rolled back before the deadline")
        if not attached():
            problems.append("premature guard detached the chain")

        # 3. expiry rolls back and clears the deadline.
        p = lab / "db" / "ops" / "fw.deadline"
        p.write_text(str(now - 1), encoding="utf-8")
        r = run("guard")
        if "ROLLED BACK" not in r.stdout:
            problems.append(f"expired guard did not roll back: {r.stdout!r}")
        if deadline() is not None:
            problems.append("deadline survived rollback")
        if attached():
            problems.append("chain still attached after expiry rollback")

        # 4. confirm cancels the deadline; a confirmed chain is restored on the
        #    next guard tick without re-arming the 180 s window.
        r = run("apply")
        r = run("confirm")
        if "CONFIRMED" not in r.stdout:
            problems.append(f"confirm failed: {r.stdout!r}")
        if deadline() is not None:
            problems.append("confirm did not clear the deadline")
        subprocess.run([sh_path, "-c", '"$STUB_BIN/iptables" -D INPUT -j LABFW'], env=env,
                       capture_output=True, text=True)
        r = run("guard")
        if not attached():
            problems.append("confirmed chain was not restored by guard")
        if deadline() is not None:
            problems.append("restoring a confirmed chain re-armed the deadline")

        # 5. manual rollback is immediate and clears the confirmation.
        r = run("rollback")
        if attached():
            problems.append("manual rollback left the chain attached")
        if (lab / "db" / "ops" / "fw.confirmed").exists():
            problems.append("manual rollback left fw.confirmed in place")

        # 6. status with no rules is inactive and does not error.
        r = run("status")
        if r.returncode != 0:
            problems.append(f"status exited {r.returncode}")

        print(f"fw-guard harness: {6 - len(problems)}/6 checks passed")
        for p in problems:
            print("  FAIL:", p)
        return 1 if problems else 0
    finally:
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
