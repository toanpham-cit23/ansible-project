#!/usr/bin/env python3
"""Load the VLAN plan into the dashboard database.

Runs inside the poller container (it already has python3 and the database
mounted at /data), driven by playbooks/20_vlans.yml. Reads /data/design.json,
which Ansible renders from files/lab/vlans.yml.

Three tables, three jobs:

  vlans        what the segments are called
  subnets      which address range and gateway belongs to each
  host_design  which MAC sits in which VLAN, on which switch, behind which
               router - this is what the poller reads to stop reporting every
               host as VLAN 1 seen on r1

    python3 seed_design.py            load it
    python3 seed_design.py --report   print the balance, change nothing
"""

import json
import os
import sqlite3
import sys

DB = os.environ.get("NETDB", "/data/net.db")
PLAN = os.environ.get("DESIGN", "/data/design.json")


def load(db, plan):
    vlans = plan.get("vlans", [])
    hosts = plan.get("hosts", [])

    db.execute("""CREATE TABLE IF NOT EXISTS host_design (
                    mac      TEXT PRIMARY KEY,
                    hostname TEXT NOT NULL DEFAULT '',
                    vlan_id  INTEGER NOT NULL,
                    switch   TEXT NOT NULL DEFAULT '',
                    router   TEXT NOT NULL DEFAULT '',
                    site_id  INTEGER)""")

    # vlans has UNIQUE(site_id, vlan_id) and site_id is NULL here, and SQLite
    # treats NULLs as distinct - so ON CONFLICT would never fire and every run
    # would append a duplicate row. The table holds configuration, not history,
    # so replacing it wholesale is both safe and idempotent.
    db.execute("DELETE FROM vlans")
    for v in vlans:
        db.execute("INSERT INTO vlans(vlan_id, name, site_id) VALUES (?,?,NULL)",
                   (v["id"], v["name"]))
        db.execute("""INSERT INTO subnets(cidr, gateway, vlan_id, site_id, description)
                      VALUES (?,?,?,NULL,?)
                      ON CONFLICT(cidr) DO UPDATE SET
                        gateway=excluded.gateway,
                        vlan_id=excluded.vlan_id,
                        description=excluded.description""",
                   (v["cidr"], v["gateway"], v["id"], v["name"]))

    for h in hosts:
        db.execute("""INSERT INTO host_design(mac, hostname, vlan_id, switch, router, site_id)
                      VALUES (?,?,?,?,?,NULL)
                      ON CONFLICT(mac) DO UPDATE SET
                        hostname=excluded.hostname,
                        vlan_id=excluded.vlan_id,
                        switch=excluded.switch,
                        router=excluded.router""",
                   (h["mac"].upper(), h["name"], h["vlan"], h["switch"], h["router"]))

    # Apply it to the leases we already hold, so the change is visible on the
    # next page load rather than on the next poll.
    by_mac = {h["mac"].upper(): h for h in hosts}
    cidr_of = {v["id"]: v["cidr"] for v in vlans}
    touched = 0
    for row in db.execute("SELECT id, mac FROM ip_leases").fetchall():
        h = by_mac.get(str(row["mac"]).upper())
        if not h:
            continue
        db.execute("""UPDATE ip_leases
                      SET vlan_id = ?, subnet = COALESCE(?, subnet),
                          seen_on = COALESCE(NULLIF(?, ''), seen_on),
                          switch_name = COALESCE(NULLIF(?, ''), switch_name)
                      WHERE id = ?""",
                   (h["vlan"], cidr_of.get(h["vlan"]), h["router"], h["switch"], row["id"]))
        touched += 1
    # Anything on the management range that is not a designed host is
    # infrastructure - the controller, the routers, the gateway. The poller only
    # relabels leases it observes, so a lease that has gone offline keeps
    # whatever VLAN it had; that is where stray VLAN 1 rows come from.
    db.execute("""UPDATE ip_leases
                  SET vlan_id = 99, subnet = '192.168.23.0/24'
                  WHERE ip LIKE '192.168.23.%'
                    AND UPPER(mac) NOT IN (SELECT UPPER(mac) FROM host_design)""")
    return len(vlans), len(hosts), touched


def report(db):
    print("VLAN plan")
    rows = db.execute("""SELECT v.vlan_id, v.name, s.cidr, s.gateway,
                                COUNT(l.id) AS leases
                         FROM vlans v
                         LEFT JOIN subnets s ON s.vlan_id = v.vlan_id
                         LEFT JOIN ip_leases l ON l.vlan_id = v.vlan_id
                         GROUP BY v.vlan_id ORDER BY v.vlan_id""").fetchall()
    for r in rows:
        print("  %-4s %-20s %-18s gw %-15s %d lease(s)"
              % (r["vlan_id"], r["name"], r["cidr"] or "-", r["gateway"] or "-", r["leases"]))

    print("\nSeen on (load balance across the core)")
    # Joined against the design, not the whole lease table: infrastructure rows
    # are observed on whichever router answered first and would drown out the
    # thing being measured.
    rows = db.execute("""SELECT COALESCE(NULLIF(l.seen_on,''),'-') AS r, COUNT(*) AS n
                         FROM ip_leases l
                         JOIN host_design d ON UPPER(l.mac) = UPPER(d.mac)
                         GROUP BY r ORDER BY n DESC""").fetchall()
    total = sum(r["n"] for r in rows) or 1
    for r in rows:
        bar = "#" * max(1, round(24 * r["n"] / total))
        print("  %-4s %-26s %d" % (r["r"], bar, r["n"]))

    rows = db.execute("""SELECT vlan_id, COUNT(*) AS n FROM ip_leases
                         WHERE vlan_id IS NOT NULL GROUP BY vlan_id
                         ORDER BY vlan_id""").fetchall()
    print("\nLeases per VLAN: " + ", ".join("%s=%d" % (r["vlan_id"], r["n"]) for r in rows))


def main():
    db = sqlite3.connect(DB)
    db.row_factory = sqlite3.Row
    if "--report" in sys.argv:
        report(db)
        return
    if not os.path.exists(PLAN):
        raise SystemExit("no plan at %s - run playbooks/20_vlans.yml" % PLAN)
    with open(PLAN) as fh:
        plan = json.load(fh)
    v, h, t = load(db, plan)
    db.commit()
    print("%d VLANs, %d hosts, %d existing lease(s) relabelled" % (v, h, t))
    report(db)


if __name__ == "__main__":
    main()
