#!/usr/bin/env python3
"""Ansible dynamic inventory backed by SQLite.

  ./inventory_sqlite.py --list
  ./inventory_sqlite.py --host r1
  ./inventory_sqlite.py --init
  ./inventory_sqlite.py --encrypt 'secret'      # print hex blob for seeding

Env:
  NETDB      path to sqlite file   (default: ../db/net.db from this script)
  NETDB_KEY  hex 32-byte AES key   (absent = secrets omitted, SSH keys used)
"""
import json
import os
import sqlite3
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
DB = os.environ.get("NETDB", os.path.join(ROOT, "db", "net.db"))
SCHEMA = os.path.join(ROOT, "db", "schema.sql")


def _aead():
    try:
        from cryptography.hazmat.primitives.ciphers.aead import AESGCM
        return AESGCM
    except ImportError:
        return None


def _key():
    raw = os.environ.get("NETDB_KEY", "").strip()
    if len(raw) != 64:
        return None
    try:
        return bytes.fromhex(raw)
    except ValueError:
        return None


def decrypt(blob):
    """nonce(12) || ciphertext  ->  str, or None when not decryptable."""
    AESGCM, key = _aead(), _key()
    if not blob or AESGCM is None or key is None:
        return None
    try:
        return AESGCM(key).decrypt(blob[:12], bytes(blob[12:]), None).decode()
    except Exception:
        return None


def encrypt(text):
    AESGCM, key = _aead(), _key()
    if AESGCM is None:
        sys.exit("cryptography not installed: apk add py3-cryptography")
    if key is None:
        sys.exit("NETDB_KEY must be 64 hex chars")
    nonce = os.urandom(12)
    return (nonce + AESGCM(key).encrypt(nonce, text.encode(), None)).hex()


def rows(sql, args=()):
    if not os.path.exists(DB):
        sys.exit("database not found: %s (run --init)" % DB)
    con = sqlite3.connect(DB)
    con.row_factory = sqlite3.Row
    try:
        return [dict(r) for r in con.execute(sql, args)]
    finally:
        con.close()


def hostvars(d):
    v = {
        "ansible_host": d["mgmt_ip"],
        "ansible_port": d["ssh_port"] or 22,
        "device_kind": d["kind_override"] or d["kind"],
        "kind_override": d["kind_override"] or "",
        "kind_confidence": d["kind_confidence"],
        "device_vendor": d["vendor"] or "",
        "device_model": d["model"] or "",
        "device_asset_tag": d["asset_tag"] or "",
        "site": d["site"] or "",
        "ssh_port": d["ssh_port"] or 22,
        "telnet_port": d["telnet_port"] or 23,
        "snmp_version": d["snmp_version"],
        "snmp_v3_user": d["snmp_v3_user"] or "",
        "snmp_v3_level": d["snmp_v3_level"],
    }
    if d["username"]:
        v["ansible_user"] = d["username"]

    for field, var in (("snmp_community_enc", "snmp_community"),
                       ("snmp_v3_auth_enc", "snmp_v3_auth"),
                       ("snmp_v3_priv_enc", "snmp_v3_priv"),
                       ("password_enc", "ansible_password")):
        val = decrypt(d[field])
        if val is not None:
            v[var] = val

    if "ansible_password" not in v:
        v["ansible_ssh_common_args"] = "-o PreferredAuthentications=publickey"
    return v


def build():
    devs = rows("""
        SELECT d.*, s.name AS site FROM devices d
        LEFT JOIN sites s ON s.id = d.site_id ORDER BY d.hostname
    """)
    groups, meta = {}, {}
    for d in devs:
        kind = d["kind_override"] or d["kind"]
        buckets = {
            "router": "routers", "l3switch": "l3switches",
            "switch": "switches", "unknown": "unclassified",
        }
        groups.setdefault(buckets.get(kind, "unclassified"), []).append(d["hostname"])
        if d["site"]:
            groups.setdefault("site_" + d["site"].replace(" ", "_"), []).append(d["hostname"])
        meta[d["hostname"]] = hostvars(d)

    # every managed device, regardless of class — playbooks target this group
    for g in ("routers", "l3switches", "switches"):
        groups.setdefault(g, [])
    groups["network_devices"] = {"children": ["routers", "l3switches", "switches"]}
    groups["all"] = {"children": sorted(g for g in groups if g != "all")}
    return {"_meta": {"hostvars": meta}, **groups}


def main():
    if "--init" in sys.argv:
        os.makedirs(os.path.dirname(DB), exist_ok=True)
        con = sqlite3.connect(DB)
        con.executescript(open(SCHEMA, encoding="utf-8").read())
        con.commit()
        con.close()
        print("initialised", DB)
        return

    if "--encrypt" in sys.argv:
        i = sys.argv.index("--encrypt")
        print(encrypt(sys.argv[i + 1]))
        return

    if _key() is None:
        sys.stderr.write("NETDB_KEY unset or invalid: secrets omitted\n")

    if "--host" in sys.argv:
        name = sys.argv[sys.argv.index("--host") + 1]
        got = rows("""
            SELECT d.*, s.name AS site FROM devices d
            LEFT JOIN sites s ON s.id = d.site_id WHERE d.hostname = ?
        """, (name,))
        print(json.dumps(hostvars(got[0]) if got else {}, indent=2))
        return

    print(json.dumps(build(), indent=2))


if __name__ == "__main__":
    main()
