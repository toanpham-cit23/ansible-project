#!/usr/bin/env python3
"""Assert that Leases and Monitor describe the same reality.

Both pages must answer "is this device alive?" the same way, or the operator
sees a green address on one page and a red device on the other and cannot
tell which one lied.

The rule this checks:

    lease.online  ==  (monitor says up OR monitor says blind)

* up    - answered SNMP. Definitely alive.
* blind - answered ICMP but not SNMP. Alive, just not monitored. Still online
          as far as an address table is concerned.
* down  - answered neither. The lease must not be online.
* unknown - never probed. Not comparable, so skipped.

Exit code 0 when they agree, 1 when they do not, 2 on a setup error.
"""
import json
import os
import sys
import urllib.error
import urllib.request
import http.cookiejar

BASE = os.environ.get("LAB_BASE", "http://127.0.0.1:8080")
USER = os.environ.get("ADMIN_USER", "admin")
PASS = os.environ.get("ADMIN_PASSWORD", "labadmin")


def login():
    cj = http.cookiejar.CookieJar()
    op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
    body = json.dumps({"username": USER, "password": PASS}).encode()
    req = urllib.request.Request(BASE + "/api/login", data=body,
                                 headers={"Content-Type": "application/json"})
    op.open(req, timeout=15).read()
    return "; ".join("%s=%s" % (c.name, c.value) for c in cj)


def api(path, cookie):
    req = urllib.request.Request(BASE + path)
    req.add_header("Cookie", cookie)
    with urllib.request.urlopen(req, timeout=25) as r:
        return json.load(r)


def rows_of(payload, *keys):
    """The API returns either a bare list or an object wrapping one."""
    if isinstance(payload, list):
        return payload
    for k in keys:
        if isinstance(payload.get(k), list):
            return payload[k]
    return []


def liveness(d):
    """Mirror the dashboard's devLiveness(): up / blind / down / unknown."""
    state = (d.get("last_state") or "").lower()
    reach = (d.get("last_reachable") or "").lower()
    if state == "up":
        return "up"
    if reach == "up":
        return "blind"
    if state or reach:
        return "down"
    return "unknown"


def main():
    try:
        cookie = login()
    except (urllib.error.URLError, OSError) as e:
        print("cannot reach %s: %s" % (BASE, e))
        return 2

    devs = rows_of(api("/api/devices", cookie), "devices")
    leases = rows_of(api("/api/leases", cookie), "leases", "addresses")

    by_ip = {}
    for d in devs:
        if d.get("mgmt_ip"):
            by_ip[d["mgmt_ip"]] = d

    mismatches = []
    compared = 0
    for lease in leases:
        ip = lease.get("ip")
        dev = by_ip.get(ip)
        if not dev:
            continue                      # an address with no inventory row
        live = liveness(dev)
        if live == "unknown":
            continue                      # never probed: nothing to compare
        compared += 1
        want = live in ("up", "blind")
        got = bool(lease.get("online"))
        if want != got:
            mismatches.append({
                "ip": ip,
                "device": dev.get("hostname") or lease.get("hostname"),
                "lease_online": got,
                "monitor": live,
                "last_seen": lease.get("last_seen"),
            })

    print("leases rows: %d  devices: %d  compared: %d"
          % (len(leases), len(devs), compared))
    print("MISMATCHES: %d" % len(mismatches))
    for m in mismatches:
        print("  %-15s %-18s lease_online=%-5s monitor=%-7s last_seen=%s"
              % (m["ip"], m["device"], m["lease_online"], m["monitor"], m["last_seen"]))
    if mismatches:
        print("\nLeases and Monitor disagree. They must derive `online` from the"
              " same probe result - see collect_leases() in poller.py.")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
