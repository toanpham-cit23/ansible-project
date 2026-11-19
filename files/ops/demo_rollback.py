#!/usr/bin/env python3
"""Failure demos for files/ops/fw-guard.sh.

test_fw_guard.py proves the guard's timing logic is correct. This file answers a
different question: when something actually goes wrong, what does the guard do
for you? Each demo runs the real script against a stub firewall, breaks
something the way an operator would, and then reports whether the controller is
still reachable.

Reachability is not asserted - it is computed. The stub records every iptables
call, the log is replayed into a faithful LABFW chain, and a simulated SSH
packet from the management host is walked through it. "SSH lost" therefore means
the rule order really would deny it, not that a flag was set.

Run:  python3 files/ops/demo_rollback.py
Exit: 0 every scenario behaved as designed, 1 otherwise.
"""
from __future__ import annotations

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

MGMT_HOST = "192.168.23.10"   # the controller, the machine we must not lock out
CHAIN = "LABFW"

# The stub firewall. It records argv verbatim AND keeps just enough state to
# answer `-nL INPUT`, because the guard inspects the live chain to decide
# whether a confirmed firewall needs reinstalling. A stub that only logged would
# make the restore path look broken when it is not.
IPT = """#!/bin/sh
printf '%s\\n' "$*" >> "$STUB_LOG"
STATE="$STUB_STATE"
[ -f "$STATE" ] || printf 'attached=0\\n' > "$STATE"
get() { sed -n "s/^$1=//p" "$STATE"; }
setv() { sed -i.bak "s/^$1=.*/$1=$2/" "$STATE"; rm -f "$STATE.bak"; }
case "$1" in
  -N) setv attached 1 ;;
  -X|-F) setv attached 0 ;;
  -I) [ "$2" = "INPUT" ] && setv attached 1 ;;
  -D) [ "$2" = "INPUT" ] && setv attached 0 ;;
  -nL) [ "$(get attached)" = 1 ] && printf 'LABFW\\n' ;;
esac
exit 0
"""

RC = "#!/bin/sh\nexit 0\n"
PYSTUB = "#!/bin/sh\nexit 0\n"

# A rule that the real iptables would reject (an unknown match module). The
# guard's `add()` is supposed to catch this and roll everything back.
REJECTING_IPT = """#!/bin/sh
# Refuse anything mentioning a bogus match, the way iptables refuses an
# unknown module. Everything else is recorded normally.
case "$*" in
  *bogus-match*) printf '%s\\n' "$*" >> "$STUB_LOG"; printf 'iptables: No chain/target/match by that name.\\n' >&2; exit 1 ;;
esac
printf '%s\\n' "$*" >> "$STUB_LOG"
exit 0
"""


def write_exec(path: pathlib.Path, body: str) -> None:
    path.write_text(body, encoding="utf-8", newline="\n")
    path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


# --------------------------------------------------------------- rule model

def replay(log: pathlib.Path) -> list[tuple[str, list[str]]]:
    """Turn the iptables call log into the current LABFW rule list.

    Only what the guard actually uses is modelled: create, flush, delete-chain,
    append, insert-at-position and delete. Inserting the chain into INPUT
    (`-I INPUT 1 -j LABFW`) attaches the chain, so it does not add a rule here.
    """
    rules: list[tuple[str, list[str]]] = []
    if not log.exists():
        return rules
    for line in log.read_text(encoding="utf-8").splitlines():
        args = line.split()
        if not args:
            continue
        op = args[0]
        if op in ("-nL", "-S"):          # read-only probes
            continue
        if op in ("-N", "-F", "-X"):
            rules = []
        elif op == "-A":
            if args[1] == CHAIN and args[2:3] == ["-j"] and args[3:4] == [CHAIN]:
                continue                 # attaching is not a rule
            if args[1] == CHAIN:
                rules.append(("A", args[2:]))
        elif op == "-I":
            if args[1] == "INPUT":
                continue                 # attach, not a rule
            if args[1] == CHAIN:
                # "-I LABFW -j DROP" and "-I LABFW 1 -j DROP" both insert at
                # the top; the position is optional and defaults to 1.
                rest = args[2:]
                if rest and rest[0].isdigit():
                    rest = rest[1:]
                rules.insert(0, ("I", rest))
        elif op == "-D":
            if args[1] == "INPUT":
                rules = []               # detach clears the effective chain
            elif args[1] == CHAIN and rules:
                rules.pop(0)
    return rules


def chain_attached(log: pathlib.Path) -> bool:
    """True when the last attach/detach operation left LABFW on INPUT."""
    if not log.exists():
        return False
    attached = False
    for line in log.read_text(encoding="utf-8").splitlines():
        args = line.split()
        if len(args) >= 4 and args[0] == "-I" and args[1] == "INPUT" and CHAIN in args:
            attached = True
        elif len(args) >= 4 and args[0] == "-D" and args[1] == "INPUT":
            attached = False
        elif args and args[0] == "-X":
            attached = False
    return attached


def ssh_reachable(log: pathlib.Path) -> tuple[bool, str]:
    """Would a TCP SYN from the controller to :22 be accepted?

    Rules are evaluated in order, first match wins, exactly as iptables does.
    When the chain is not attached to INPUT there is no filter, so the answer
    is yes - which is the whole point of the guard: an unattached chain means
    the lockout is gone.
    """
    if not chain_attached(log):
        return True, "no lab chain on INPUT, so nothing filters port 22"

    for _, rule in replay(log):
        proto = _val(rule, "-p")
        dport = _val(rule, "--dport")
        src = _val(rule, "-s")
        iface = _val(rule, "-i")
        target = _val(rule, "-j")

        if proto == "icmp":
            continue                      # our packet is TCP
        if iface and iface != "eth0":     # controller arrives on eth0
            continue
        if proto == "tcp" and dport == "22":
            if src and not _in_net(MGMT_HOST, src):
                continue
            if target == "ACCEPT":
                return True, "matched ACCEPT for tcp/22 from " + MGMT_HOST
            if target == "DROP":
                return False, "matched DROP for tcp/22 from " + MGMT_HOST
            continue
        if proto is None and not dport and not src and target == "DROP":
            return False, "fell through to a blanket DROP"
    return False, "reached the end of the chain with no ACCEPT, so the default denies"


def _val(rule: list[str], flag: str) -> str | None:
    return rule[rule.index(flag) + 1] if flag in rule else None


def _in_net(ip: str, net: str) -> bool:
    if "/" not in net:
        return ip == net
    base, bits = net.split("/")
    bits = int(bits)
    a = [int(x) for x in ip.split(".")]
    b = [int(x) for x in base.split(".")]
    mask = (0xFFFFFFFF << (32 - bits)) & 0xFFFFFFFF
    to_i = lambda p: (p[0] << 24) | (p[1] << 16) | (p[2] << 8) | p[3]
    return (to_i(a) & mask) == (to_i(b) & mask)


# --------------------------------------------------------------- harness

class Lab:
    """A throwaway controller: stubbed binaries, the real guard, a fake /root."""

    def __init__(self, ipt_body: str = IPT) -> None:
        self.work = pathlib.Path(tempfile.mkdtemp(prefix="fw-demo-"))
        self.bin = self.work / "bin"
        self.bin.mkdir()
        self.lab = self.work / "lab"
        (self.lab / "ops").mkdir(parents=True)
        self.log = self.work / "calls.log"
        self.log.touch()

        write_exec(self.bin / "iptables", ipt_body)
        write_exec(self.bin / "rc-service", RC)
        write_exec(self.bin / "python3", PYSTUB)

        body = GUARD.read_text(encoding="utf-8")
        body = body.replace("LAB=/root/lab", "LAB=" + str(self.lab).replace("\\", "/"))
        body = re.sub(r"\biptables\b", '"$STUB_BIN/iptables"', body)
        body = body.replace('python3 "$S"', '"$STUB_BIN/python3" "$S"')
        body = re.sub(r"\brc-service\b", '"$STUB_BIN/rc-service"', body)
        self.script = self.work / "fw-guard.sh"
        write_exec(self.script, body)

        self.env = dict(os.environ)
        self.env["STUB_BIN"] = str(self.bin).replace("\\", "/")
        self.env["STUB_LOG"] = str(self.log).replace("\\", "/")
        self.env["STUB_STATE"] = str(self.work / "ipt.state").replace("\\", "/")
        self.sh = shutil.which("sh") or ""

    def run(self, *args: str) -> subprocess.CompletedProcess:
        return subprocess.run([self.sh, str(self.script), *args],
                              env=self.env, capture_output=True, text=True)

    def deadline(self) -> int | None:
        p = self.lab / "db" / "ops" / "fw.deadline"
        return int(p.read_text()) if p.exists() else None

    def expire(self) -> None:
        """Move the deadline into the past without waiting three minutes."""
        p = self.lab / "db" / "ops" / "fw.deadline"
        p.write_text(str(int(time.time()) - 1), encoding="utf-8")

    def reachable(self) -> tuple[bool, str]:
        return ssh_reachable(self.log)

    def close(self) -> None:
        shutil.rmtree(self.work, ignore_errors=True)


# --------------------------------------------------------------- scenarios

def demo_lockout() -> bool:
    print("\n" + "=" * 74)
    print("DEMO 1  An operator locks themselves out of the controller")
    print("=" * 74)
    lab = Lab()
    try:
        ok_before, why = lab.reachable()
        print(f"  SSH from {MGMT_HOST} before any change : "
              f"{'REACHABLE' if ok_before else 'BLOCKED'}  ({why})")

        print("\n  The operator applies the firewall, then edits it by hand and")
        print("  puts a DROP ahead of the SSH rule - the classic lockout.")
        r = lab.run("apply")
        print(f"  'apply' -> {r.stdout.strip().splitlines()[0] if r.stdout.strip() else '(no output)'}")
        dl = lab.deadline()
        left = (dl - int(time.time())) if dl else 0
        print(f"  deadline armed, {left}s left to confirm")

        # The mistake: a DROP inserted at the top of the chain.
        subprocess.run([lab.sh, "-c",
                        f'"$STUB_BIN/iptables" -I {CHAIN} -j DROP'],
                       env=lab.env, capture_output=True, text=True)
        ok_after, why = lab.reachable()
        print(f"  SSH after the bad rule              : "
              f"{'REACHABLE' if ok_after else 'BLOCKED'}  ({why})")

        print("\n  The operator never confirms. crond runs the guard tick...")
        lab.expire()
        r = lab.run("guard")
        print(f"  'guard' -> {r.stdout.strip()}")
        ok_final, why2 = lab.reachable()
        print(f"  SSH after the guard fires           : "
              f"{'REACHABLE' if ok_final else 'BLOCKED'}  ({why2})")

        pass_ = (not ok_after) and ok_final and lab.deadline() is None
        print(f"\n  RESULT: {'PASS' if pass_ else 'FAIL'} - the lockout was real and the "
              f"guard {'undid' if pass_ else 'did not undo'} it without anyone logging in.")
        return pass_
    finally:
        lab.close()


def demo_rejected_rule() -> bool:
    print("\n" + "=" * 74)
    print("DEMO 2  A rule is rejected halfway through applying the set")
    print("=" * 74)
    lab = Lab(REJECTING_IPT)
    try:
        print("  Here iptables refuses one rule in the middle of the set, the way")
        print("  it does when a match module is missing. A naive script would")
        print("  leave the rules before it applied and no SSH rule reached yet.")

        # Simulate the rejected rule by calling straight through the stub with
        # an argument the stub refuses, then let the guard's add() path run.
        body = lab.script.read_text(encoding="utf-8")
        body = body.replace(
            '    add -p udp --dport 53 -j ACCEPT',
            '    add -p udp --dport 53 -j ACCEPT\n'
            '    add -m bogus-match -j ACCEPT')
        lab.script.write_text(body, encoding="utf-8", newline="\n")

        r = lab.run("apply")
        blocked = "RULE FAILED - ROLLED BACK" in r.stdout or "RULE FAILED" in r.stderr
        ok, why = lab.reachable()
        attached = chain_attached(lab.log)
        print(f"  'apply' output  : {(r.stdout + r.stderr).strip().splitlines()[0] if (r.stdout + r.stderr).strip() else '(none)'}")
        print(f"  chain left on INPUT : {'yes' if attached else 'no'}")
        print(f"  SSH from {MGMT_HOST}      : {'REACHABLE' if ok else 'BLOCKED'}  ({why})")
        print(f"  deadline left armed : {'yes' if lab.deadline() else 'no'}")

        pass_ = blocked and ok and not attached and lab.deadline() is None
        print(f"\n  RESULT: {'PASS' if pass_ else 'FAIL'} - a half-applied firewall is the worst "
              f"outcome, and the guard {'avoided' if pass_ else 'produced'} it.")
        return pass_
    finally:
        lab.close()


def demo_self_heal() -> bool:
    print("\n" + "=" * 74)
    print("DEMO 3  A confirmed firewall disappears after a network restart")
    print("=" * 74)
    lab = Lab()
    try:
        lab.run("apply")
        lab.run("confirm")
        ok, why = lab.reachable()
        print(f"  Confirmed and reachable : {'yes' if ok else 'no'}  ({why})")

        print("\n  Docker restarts the network and the custom chain is gone - a real")
        print("  Alpine behaviour, and the reason the guard has a restore path.")
        subprocess.run([lab.sh, "-c",
                        f'"$STUB_BIN/iptables" -D INPUT -j {CHAIN}; '
                        f'"$STUB_BIN/iptables" -F {CHAIN}'],
                       env=lab.env, capture_output=True, text=True)
        ok2, why2 = lab.reachable()
        print(f"  After the restart      : {'REACHABLE' if ok2 else 'BLOCKED'}  ({why2})")
        print("  (reachable but unprotected is the danger: the host is open again)")

        r = lab.run("guard")
        ok3, why3 = lab.reachable()
        attached = chain_attached(lab.log)
        print(f"\n  'guard' -> {'silent restore' if r.returncode == 0 else 'error'}")
        print(f"  Chain restored         : {'yes' if attached else 'no'}")
        print(f"  Deadline re-armed      : {'yes (wrong)' if lab.deadline() else 'no (correct)'}")

        pass_ = attached and ok3 and lab.deadline() is None
        print(f"\n  RESULT: {'PASS' if pass_ else 'FAIL'} - the guard reinstalls a confirmed "
              f"firewall without starting a new {'countdown' if pass_ else 'timer'}.")
        return pass_
    finally:
        lab.close()


def main() -> int:
    if not shutil.which("sh"):
        print("sh not found; run this from Git Bash", file=sys.stderr)
        return 2
    results = [demo_lockout(), demo_rejected_rule(), demo_self_heal()]
    print("\n" + "=" * 74)
    print(f"SUMMARY  {sum(results)}/{len(results)} scenarios behaved as designed")
    print("=" * 74)
    return 0 if all(results) else 1


if __name__ == "__main__":
    sys.exit(main())
