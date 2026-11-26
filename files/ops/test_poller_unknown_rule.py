#!/usr/bin/env python3
"""Regression guard for the poller's "unidentified device" deletion rule.

The rule exists to stop a box we can neither name nor reach from filling the
inventory with question marks. It fired on reachable-but-SNMP-silent devices
too, so the six simulator workstations were deleted every 180 s, each deletion
posting a Telegram alert, and the next cycle re-created them - a delete/notify
loop that also aborted the cycle with FOREIGN KEY constraint failed because a
later section wrote a log_finding for the row that had just been removed.

Run: python3 test_poller_unknown_rule.py
"""
import ast
import re
import sys

SRC = "files/dashboard/docker/poller/poller.py"


def main():
    src = open(SRC, encoding="utf-8").read()
    bad = []

    # 1. The guard must consider reachability, not just SNMP state.
    m = re.search(r'if d\["kind"\] == "unknown".*?\n(.*?)\n\s*else:', src, re.S)
    if not m:
        bad.append("could not find the unidentified-device guard")
    else:
        cond = m.group(1)
        if 'reachable == "up"' not in cond:
            bad.append("the unknown-device rule ignores reachability - a device that "
                       "answers ICMP will be deleted as 'unidentified' every TTL")

    # 2. A deletion must be recorded so later sections skip the row.
    if "removed = set()" not in src:
        bad.append("no `removed` set: sections after the delete loop can still "
                   "write children of a deleted device and abort the cycle")
    if "removed.add(d[\"id\"])" not in src:
        bad.append("deletions are not recorded in `removed`")

    # 3. The finding loop must skip removed devices.
    if 'if d["id"] in removed:' not in src:
        bad.append("the finding loop does not skip removed devices - "
                   "log_findings has ON DELETE CASCADE and will raise "
                   "FOREIGN KEY constraint failed, rolling back the whole cycle")

    # 4. The log scan must skip them too.
    if 'd["id"] not in removed' not in src:
        bad.append("collect_logs still receives removed devices")

    # 5. The delete's IntegrityError path must not `continue` before writing
    #    last_reachable; it has to leave the row consistent.
    if "last_reachable='down'" not in src:
        bad.append("the failed-delete path does not record last_reachable, "
                   "leaving the row half-written")

    try:
        ast.parse(src)
    except SyntaxError as e:
        bad.append("poller.py does not parse: %s" % e)

    if bad:
        print("POLLER RULE: %d problem(s)" % len(bad))
        for b in bad:
            print("  -", b)
        return 1
    print("POLLER RULE: OK (reachability respected, deletions recorded and skipped)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
