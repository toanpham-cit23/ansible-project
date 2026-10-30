"""Collector for the lab dashboard.

Three jobs, in this order every cycle:

  1. reach   - ask each device for its SNMP uptime. A reset means it rebooted.
  2. leases  - read every router's ARP table, so we know which IP is held by
               which MAC on which port, and keep that as history.
  3. notify  - turn the events we just wrote into mail for whoever is on call.

Everything is collected first and written in one short transaction: holding a
write lock across a dozen SNMP walks blocks the API for longer than its
busy_timeout, and logins then fail with "database is locked".
"""
import hashlib
import os
import re
import shutil
import smtplib
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
from email.message import EmailMessage
from email.utils import formatdate, make_msgid

DB = os.environ.get("NETDB", "/data/net.db")
# 5 s is the requirement; keep the env override so a demo can slow it down.
INTERVAL = int(os.environ.get("POLL_INTERVAL", "5"))
HOOK = os.environ.get("ALERT_WEBHOOK", "").strip()

SMTP_HOST = os.environ.get("SMTP_HOST", "").strip()
SMTP_PORT = int(os.environ.get("SMTP_PORT", "25"))
SMTP_USER = os.environ.get("SMTP_USER", "").strip()
SMTP_PASS = os.environ.get("SMTP_PASS", "").strip()
SMTP_TLS = os.environ.get("SMTP_TLS", "0") == "1"
MAIL_FROM = os.environ.get("MAIL_FROM", "lab-dashboard@localhost").strip()
# When no SMTP relay is configured we still want a visible trace, so mail is
# written to /data/outbox as .eml instead of being silently dropped.
MAIL_DIR = os.environ.get("MAIL_DIR", "/data/outbox")

O_UPTIME = "1.3.6.1.2.1.1.3.0"
O_SYSNAME = "1.3.6.1.2.1.1.5.0"
O_IFNUM = "1.3.6.1.2.1.2.1.0"
O_IFDESCR = "1.3.6.1.2.1.2.2.1.2"
O_CPU = "1.3.6.1.2.1.25.3.3.1.2"
O_ST_TYPE = "1.3.6.1.2.1.25.2.3.1.2"
O_ST_UNITS = "1.3.6.1.2.1.25.2.3.1.4"
O_ST_SIZE = "1.3.6.1.2.1.25.2.3.1.5"
O_ST_USED = "1.3.6.1.2.1.25.2.3.1.6"
O_IN = "1.3.6.1.2.1.31.1.1.1.6"
O_OUT = "1.3.6.1.2.1.31.1.1.1.10"
O_ARP = "1.3.6.1.2.1.4.22.1"          # ipNetToMediaTable
# ipNetToMediaEntry is .1, so the columns live one level deeper:
#   .2 = ipNetToMediaPhysAddress, .3 = ipNetToMediaNetAddress
# and the row index is ifIndex followed by the four address octets.
O_ARP_PHYS = O_ARP + ".2"
O_ARP_ADDR = O_ARP + ".3"
O_IFNAME = "1.3.6.1.2.1.31.1.1.1.1"
# Remote log source. Alpine writes everything the kernel and the daemons emit
# to /var/log/messages, which is what the classifiers are written against.
LOG_PATH = os.environ.get("LOG_PATH", "/var/log/messages")
# How many bytes of the tail to pull per device per cycle. A full file is
# needless traffic every 5 s; 8 KiB is roughly the last few minutes of a busy
# router and comfortably covers a burst of errors.
LOG_TAIL_BYTES = int(os.environ.get("LOG_TAIL_BYTES", "8192"))
# Run the SSH log scan once every N cycles. At the 5 s interval, 12 means every
# minute: fast enough to catch an error promptly, cheap enough that the SSH
# handshakes do not dominate the cycle budget.
LOG_EVERY_N = int(os.environ.get("LOG_EVERY_N", "12"))
LOG_TICK = 0

T_RAM = "1.3.6.1.2.1.25.2.1.2"
T_DISK = "1.3.6.1.2.1.25.2.1.4"

# A lease that the routers stop reporting is kept for this long before it is
# marked offline, so a PC that is simply asleep does not vanish from the table.
LEASE_GRACE = int(os.environ.get("LEASE_GRACE", "120"))
# Reachability samples are pruned beyond this window (at 5 s that is ~17k rows
# per device per day, which adds up).
REACH_KEEP_DAYS = int(os.environ.get("REACH_KEEP_DAYS", "7"))
# An uptime that is lower than the previous one by more than this is a restart,
# not a wrap of the counter.
REBOOT_DROP_S = 60
# If a device was unreachable for at least this long before it came back, call
# it a power loss rather than a quick restart.
POWER_GAP_S = 20
# Identical events inside this window are folded into one row instead of being
# re-filed every poll: a flapping link would otherwise add 12 rows a minute and
# mail on the same subject over and over once the rule cooldown expired.
EVENT_DEDUP_S = int(os.environ.get("EVENT_DEDUP_S", "300"))
# Cooldown used when an alert rule does not carry one of its own.
DEFAULT_COOLDOWN_S = 300

PREV = {}
LOCAL_PREV = {}

# Vendor prefixes, so an unnamed host still reads as something better than a
# bare MAC. Ordered longest-prefix-first by the lookup below.
OUI = {
    "00:0C:29": "VMware",
    "00:50:56": "VMware",
    "00:1C:14": "VMware",
    "00:05:69": "VMware",
    "08:00:27": "VirtualBox",
    "52:54:00": "QEMU/KVM",
    "00:15:5D": "Hyper-V",
    "02:00:00": "locally-administered",
    "B8:27:EB": "Raspberry Pi",
    "DC:A6:32": "Raspberry Pi",
    "E4:5F:01": "Raspberry Pi",
}


def conn():
    c = sqlite3.connect(DB, timeout=10)
    c.row_factory = sqlite3.Row
    c.execute("PRAGMA busy_timeout=5000")
    c.execute("PRAGMA synchronous=NORMAL")
    c.execute("PRAGMA foreign_keys=ON")
    return c


def decrypt(blob):
    if not blob:
        return None
    key = os.environ.get("NETDB_KEY", "")
    if len(key) != 64:
        return None
    try:
        from cryptography.hazmat.primitives.ciphers.aead import AESGCM
        b = bytes(blob)
        return AESGCM(bytes.fromhex(key)).decrypt(b[:12], b[12:], None).decode()
    except Exception:
        return None


def auth(d):
    if d["snmp_version"] == "3":
        a = ["-v3", "-l", d["snmp_v3_level"], "-u", d["snmp_v3_user"] or "",
             "-a", "SHA", "-A", decrypt(d["snmp_v3_auth_enc"]) or ""]
        if d["snmp_v3_level"] == "authPriv":
            a += ["-x", "AES", "-X", decrypt(d["snmp_v3_priv_enc"]) or ""]
        return a
    return ["-v2c", "-c", decrypt(d["snmp_community_enc"]) or "public"]


def get(host, args, oid, timeout=2):
    try:
        p = subprocess.run(["snmpget", *args, "-Oqv", "-t", str(timeout), "-r", "1", host, oid],
                           capture_output=True, text=True, timeout=timeout + 4)
        v = p.stdout.strip().strip('"')
        return v if p.returncode == 0 and v and v != "NONE" else None
    except Exception:
        return None


def walk(host, args, oid, timeout=3):
    """-> {index: value}, numeric OIDs preserved so we can split the index."""
    try:
        p = subprocess.run(["snmpwalk", *args, "-On", "-t", str(timeout), "-r", "1", host, oid],
                           capture_output=True, text=True, timeout=timeout + 8)
    except Exception:
        return {}
    out = {}
    for line in p.stdout.splitlines():
        if "=" not in line:
            continue
        left, right = line.split("=", 1)
        key = left.strip().lstrip(".")
        val = right.strip()
        if ":" in val:
            val = val.split(":", 1)[1].strip()
        val = val.strip('"')
        try:
            out[key] = int(val)
        except ValueError:
            out[key] = val
    return out


def metric_metrics(host, args):
    m = {}

    cpu = [v for v in walk(host, args, O_CPU).values() if isinstance(v, int)]
    if cpu:
        m["cpu_pct"] = round(sum(cpu) / len(cpu), 1)

    types = walk(host, args, O_ST_TYPE)
    units = walk(host, args, O_ST_UNITS)
    size = walk(host, args, O_ST_SIZE)
    used = walk(host, args, O_ST_USED)

    def pct(kind):
        tot = got = 0
        for k, t in types.items():
            if not str(t).startswith(kind):
                continue
            idx = k.rsplit(".", 1)[-1]
            try:
                s_i = int(size.get(k, size.get(idx, 0)) or 0)
                g_i = int(used.get(k, used.get(idx, 0)) or 0)
            except (TypeError, ValueError):
                continue
            u = 1
            try:
                u = int(str(units.get(k, units.get(idx, 1))).split()[0])
            except (TypeError, ValueError, IndexError):
                pass
            tot += s_i * u
            got += g_i * u
        return round(100.0 * got / tot, 1) if tot else None

    m["mem_pct"] = pct(T_RAM)
    m["disk_pct"] = pct(T_DISK)

    rin = sum(v for k, v in walk(host, args, O_IN).items() if isinstance(v, int))
    rout = sum(v for k, v in walk(host, args, O_OUT).items() if isinstance(v, int))
    now = time.time()
    prev = PREV.get(host)
    if prev and now > prev[0] and (rin or rout):
        dt = now - prev[0]
        m["net_in_bps"] = round(max(0, rin - prev[1]) * 8 / dt, 1)
        m["net_out_bps"] = round(max(0, rout - prev[2]) * 8 / dt, 1)
    if rin or rout:
        PREV[host] = (now, rin, rout)
    return m


def local_metrics():
    """Metrics for the controller itself, read from /proc (no SNMP daemon)."""
    m = {}
    try:
        vals = [int(x) for x in open("/proc/stat").readline().split()[1:]]
        idle = vals[3] + vals[4]
        m["cpu_pct"] = round(100.0 * (1 - idle / sum(vals)), 1)
    except Exception:
        pass
    try:
        mem = {}
        for line in open("/proc/meminfo"):
            k, v = line.split(":")
            mem[k] = int(v.split()[0])
        avail = mem.get("MemAvailable", mem.get("MemFree", 0))
        m["mem_pct"] = round(100.0 * (1 - avail / mem["MemTotal"]), 1)
    except Exception:
        pass
    try:
        st = os.statvfs("/")
        m["disk_pct"] = round(100.0 * (1 - st.f_bavail / st.f_blocks), 1)
    except Exception:
        pass
    try:
        rx = tx = 0
        for line in open("/proc/net/dev").readlines()[2:]:
            name, rest = line.split(":")
            if name.strip() == "lo":
                continue
            f = rest.split()
            rx += int(f[0])
            tx += int(f[8])
        prev = LOCAL_PREV.get("net")
        now = time.time()
        if prev:
            dt = max(0.001, now - prev[0])
            m["net_in_bps"] = round(max(0, rx - prev[1]) * 8 / dt, 1)
            m["net_out_bps"] = round(max(0, tx - prev[2]) * 8 / dt, 1)
        LOCAL_PREV["net"] = (now, rx, tx)
    except Exception:
        pass
    return m


# ---------------------------------------------------------------- leases


_HEX_TOKEN = re.compile(r"^[0-9A-Fa-f]{1,2}$")


def norm_mac(v):
    """Accept the three shapes net-snmp actually produces:

        '0:c:29:4a:66:66'                  human hex, no zero padding
        'Hex-STRING: 00 0C 29 4A 66 66'    OctetString rendering
        '00 0C 29 4A 66 66'                already-stripped octets

    and return '00:0C:29:4A:66:66', or None if it is not a usable unicast MAC.
    """
    if v is None:
        return None
    s = str(v).strip()
    if ":" in s and "Hex-STRING" in s:
        s = s.split(":", 1)[1].strip()
    s = s.replace("-", ":").replace(" ", ":").replace(".", ":")
    parts = [p for p in s.split(":") if p != ""]
    if len(parts) != 6 or not all(_HEX_TOKEN.match(p) for p in parts):
        return None
    try:
        out = [int(p, 16) for p in parts]
    except ValueError:
        return None
    if all(b == 0 for b in out):
        return None
    # bit 0 of the first octet set => multicast or broadcast, not a host
    if out[0] & 1:
        return None
    return ":".join("%02X" % b for b in out)


def parse_ticks(raw):
    """SNMP TimeTicks from -Oqv comes back as '0:0:19:49.58' (d:h:m:s.cc).
    Return whole seconds, or None if the string is not a TimeTicks."""
    if not raw:
        return None
    s = str(raw).strip()
    if ":" in s:
        try:
            parts = s.split(":")
            if len(parts) == 4:
                d, h, m, sec = parts
                return int(d) * 86400 + int(h) * 3600 + int(m) * 60 + int(float(sec))
            if len(parts) == 3:
                h, m, sec = parts
                return int(h) * 3600 + int(m) * 60 + int(float(sec))
        except (ValueError, TypeError):
            return None
        return None
    try:
        # a plain integer is hundredths of a second
        return int(float(s) / 100.0)
    except (ValueError, TypeError):
        return None


def oui_vendor(mac):
    return OUI.get(mac[:8], None)


def arp_entries(host, args, c):
    """Read ipNetToMediaTable -> [{ip, mac, if_index, if_name}]."""
    phys = walk(host, args, O_ARP_PHYS)
    if not phys:
        return []
    descr = {}
    for k, v in walk(host, args, O_IFDESCR).items():
        descr[k.rsplit(".", 1)[-1]] = v
    for k, v in walk(host, args, O_IFNAME).items():
        descr.setdefault(k.rsplit(".", 1)[-1], v)

    out = []
    for key, raw in phys.items():
        idx = str(key)[len(O_ARP_PHYS) + 1:]        # '2.192.168.23.1'
        bits = idx.split(".")
        if len(bits) != 5:
            continue
        if_index = bits[0]
        ip = ".".join(bits[1:])
        if not all(p.isdigit() for p in bits):
            continue
        mac = norm_mac(raw)
        if not mac:
            continue
        # ignore broadcast / multicast noise
        if ip.startswith("224.") or ip.startswith("239.") or ip == "255.255.255.255":
            continue
        if int(bits[4]) >= 255:
            continue
        out.append({
            "ip": ip,
            "mac": mac,
            "if_index": int(if_index) if if_index.isdigit() else None,
            "if_name": str(descr.get(if_index) or ""),
        })
    return out


def subnet_of(ip, subnet_rows):
    """Match an address to a configured subnet, longest prefix wins.

    Falls back to the /24 the address lives in when the `subnets` table is
    empty, which is the normal state of a small lab: the ARP table still
    points at a real network, so an unconfigured table should not leave the
    column blank.
    """
    best = None
    for s in subnet_rows:
        cidr = s["cidr"] or ""
        net = cidr.split("/")[0].rsplit(".", 1)[0]
        if ip.rsplit(".", 1)[0] == net:
            if best is None or len(cidr or "") > len(best["cidr"] or ""):
                best = s
    if best is not None:
        return best
    if ip.count(".") == 3:
        return {"cidr": ip.rsplit(".", 1)[0] + ".0/24", "vlan_id": None}
    return None


def device_for_ip(ip, devs):
    """Which inventoried box this address belongs to, if any.

    An ARP entry only carries an address and a MAC, so without this lookup the
    lease table would claim every host is unnamed - including the three routers
    whose hostname the dashboard already knows.
    """
    for d in devs:
        if d["mgmt_ip"] == ip:
            return d["hostname"]
    return None


def collect_leases(c, devs, arp_by_dev, subnet_rows):
    """Turn the ARP observations into ip_leases rows plus change events."""
    now = int(time.time())
    observed = {}          # (ip, mac) -> row
    for hostname, entries in arp_by_dev.items():
        for e in entries:
            key = (e["ip"], e["mac"])
            # first router to report a binding wins; prefer the one whose
            # interface actually faces that subnet
            if key not in observed:
                e = dict(e)
                e["seen_on"] = hostname
                observed[key] = e

    existing = {(r["ip"], r["mac"]): r for r in
                c.execute("SELECT * FROM ip_leases").fetchall()}

    changes = []
    for key, e in observed.items():
        sub = subnet_of(e["ip"], subnet_rows)
        vlan = sub["vlan_id"] if sub else None
        cidr = sub["cidr"] if sub else None

        row = existing.get(key)
        if row is None:
            # a MAC showing up at a new address is a move, not a new device
            prev = c.execute("SELECT ip, device_name FROM ip_leases WHERE mac=? "
                             "ORDER BY last_seen DESC LIMIT 1", (e["mac"],)).fetchone()
            moved_from = prev["ip"] if prev and prev["ip"] != e["ip"] else None
            name = ""
            if prev:
                name = prev["device_name"] or ""
            if not name:
                name = e.get("hostname") or ""
            if not name:
                name = device_for_ip(e["ip"], devs) or ""
            c.execute("""INSERT INTO ip_leases
                         (ip, mac, hostname, device_name, vlan_id, subnet, seen_on,
                          switch_name, if_index, source, is_static, online,
                          first_seen, last_seen, moved_from)
                         VALUES (?,?,?,?,?,?,?,?,?,?,?,?,datetime('now'),datetime('now'),?)""",
                      (e["ip"], e["mac"], e.get("hostname"), name, vlan, cidr,
                       e["seen_on"], e["if_name"], e["if_index"], "arp", 0, 1,
                       moved_from))
            changes.append(("new" if not prev else "moved", e["ip"], e["mac"],
                            "lan dau thay" if not prev else
                            "chuyen tu %s sang %s" % (moved_from, e["ip"])))
        else:
            # a device that was unnamed when first seen can acquire a name
            # later (it was added to the inventory, or the operator typed one
            # in); COALESCE keeps an operator-set name authoritative
            known = device_for_ip(e["ip"], devs)
            c.execute("""UPDATE ip_leases
                         SET last_seen=datetime('now'), online=1,
                             seen_on=?, switch_name=?, if_index=?,
                             vlan_id=COALESCE(?, vlan_id),
                             subnet=COALESCE(?, subnet),
                             device_name=CASE WHEN COALESCE(device_name,'')=''
                                              THEN COALESCE(?, '')
                                              ELSE device_name END
                         WHERE id=?""",
                      (e["seen_on"], e["if_name"], e["if_index"], vlan, cidr,
                       known, row["id"]))
            if row["online"] == 0:
                changes.append(("new", e["ip"], e["mac"], "tro lai truc tuyen"))

    # anything not seen this cycle: offline after the grace period, so a PC
    # that is briefly asleep does not disappear from the table
    for key, row in existing.items():
        if key in observed:
            continue
        if row["online"] == 1:
            age = now - int(time.mktime(time.strptime(row["last_seen"], "%Y-%m-%d %H:%M:%S")))
            if age > LEASE_GRACE:
                c.execute("UPDATE ip_leases SET online=0 WHERE id=?", (row["id"],))
                changes.append(("gone", row["ip"], row["mac"], "khong con thay"))

    for kind, ip, mac, detail in changes:
        c.execute("INSERT INTO lease_events(ip, mac, kind, detail) VALUES (?,?,?,?)",
                  (ip, mac, kind, detail))
    return len(observed), changes


# ---------------------------------------------------------------- log analysis


def sig_of(device_id, severity, message):
    """Same signature the Go API computes, so a Block decision applies here too."""
    raw = "%d|%s|%s" % (device_id or 0, severity, message)
    return hashlib.sha256(raw.encode()).hexdigest()[:16]


SEVERITY_RANK = {"info": 0, "warning": 1, "average": 2, "high": 3, "disaster": 4}


def emit(c, device_id, severity, message):
    sig = sig_of(device_id, severity, message)
    row = c.execute("SELECT 1 FROM log_decisions WHERE sig=? AND action='block'",
                    (sig,)).fetchone()
    if row:
        return None
    # Fold a repeat of an identical condition into the event we already have.
    # Without this a still-broken device files a fresh row every poll, and the
    # alert rule replays the same mail as soon as its cooldown rolls over.
    dup = c.execute("SELECT id FROM events WHERE sig=? AND ts > datetime('now', ?) "
                    "ORDER BY ts DESC LIMIT 1",
                    (sig, "-%d seconds" % EVENT_DEDUP_S)).fetchone()
    if dup:
        c.execute("UPDATE events SET ts=datetime('now') WHERE id=?", (dup["id"],))
        return None
    cur = c.execute("INSERT INTO events(device_id, severity, message, sig) VALUES (?,?,?,?)",
                    (device_id, severity, message, sig))
    return cur.lastrowid


def classify(text, patterns):
    """Return (pattern_row, severity, category, hint) for the first match."""
    low = text.lower()
    best = None
    for p in patterns:
        hit = False
        if p["regex"]:
            try:
                if re.search(p["regex"], text, re.IGNORECASE):
                    hit = True
            except re.error:
                pass
        if not hit:
            pat = (p["like_pat"] or "").replace("%", "")
            if pat and pat.lower() in low:
                hit = True
        if not hit:
            continue
        # when several patterns match, the most severe one wins
        if best is None or SEVERITY_RANK.get(p["severity"], 0) > SEVERITY_RANK.get(best["severity"], 0):
            best = p
    if not best:
        return None
    return best


def scan_remote_logs(host, user, password, port, path=LOG_PATH):
    """Read the tail of a device's syslog over SSH.

    Uses paramiko when it is installed, and otherwise falls back to the
    `ssh` client with a temporary askpass helper: the poller image is
    deliberately thin, and pulling in a compiler for a tail is not worth it.
    Returns the text, or "" when the device cannot be read.
    """
    cmd = "tail -c %d %s" % (LOG_TAIL_BYTES, path)
    try:
        import paramiko  # noqa: F401
    except Exception:
        return _scan_with_ssh_binary(host, user, password, port, cmd)
    try:
        import paramiko
        cl = paramiko.SSHClient()
        cl.set_missing_host_key_policy(paramiko.AutoAddPolicy())
        cl.connect(host, port=port or 22, username=user, password=password,
                   timeout=6, allow_agent=False, look_for_keys=False)
        try:
            _, out, _ = cl.exec_command(cmd, timeout=8)
            return out.read().decode("utf-8", "replace")
        finally:
            cl.close()
    except Exception:
        return ""


def _scan_with_ssh_binary(host, user, password, port, cmd):
    """Fallback: ssh(1) driven by an askpass helper and a throwaway known_hosts."""
    tmpdir = tempfile.mkdtemp(prefix="lablog.")
    askpass = os.path.join(tmpdir, "askpass.sh")
    try:
        with open(askpass, "w") as f:
            f.write("#!/bin/sh\nprintf '%s\\n' \"$LAB_SSH_PASS\"\n")
        os.chmod(askpass, 0o700)
        env = dict(os.environ)
        env.update({
            "LAB_SSH_PASS": password or "",
            "SSH_ASKPASS": askpass,
            "SSH_ASKPASS_REQUIRE": "force",
            "DISPLAY": ":0",
        })
        p = subprocess.run(
            ["ssh", "-o", "StrictHostKeyChecking=no",
             "-o", "UserKnownHostsFile=" + os.path.join(tmpdir, "known_hosts"),
             "-o", "PreferredAuthentications=password",
             "-o", "PubkeyAuthentication=no",
             "-o", "NumberOfPasswordPrompts=1",
             "-p", str(port or 22),
             "%s@%s" % (user, host), cmd],
            capture_output=True, text=True, timeout=14, env=env,
            stdin=subprocess.DEVNULL)
        return p.stdout if p.returncode == 0 else ""
    except Exception:
        return ""
    finally:
        shutil.rmtree(tmpdir, ignore_errors=True)


def collect_logs(c, devs, patterns):
    """Fetch each device's syslog tail, classify it, and record what matters.

    Errors are grouped by (pattern, device, line) in `log_findings`, so a link
    that flaps a hundred times raises one row with hits=100 rather than a
    hundred rows. INFO-level matches (link up, adjacency formed) are dropped:
    the dashboard is for problems, and they would drown the list.
    """
    if not patterns:
        return 0
    found = 0
    for d in devs:
        if d["kind"] == "server":
            continue    # the controller logs are the poller's own
        uname = d["username"] if "username" in d.keys() and d["username"] else "root"
        blob = d["password_enc"] if "password_enc" in d.keys() else None
        pw = decrypt(blob) if blob else ""
        if not pw:
            continue
        text = scan_remote_logs(d["mgmt_ip"], uname, pw, d["ssh_port"] if "ssh_port" in d.keys() else 22)
        if not text:
            continue
        for line in text.splitlines():
            line = line.strip()
            if not line:
                continue
            pat = classify(line, patterns)
            if not pat:
                continue
            # recovery notices are not findings
            if SEVERITY_RANK.get(pat["severity"], 0) <= SEVERITY_RANK.get("info", 0):
                continue
            if record_finding(c, pat, d["id"], line):
                found += 1
    return found


def record_finding(c, pat, device_id, line, source="poller"):
    sig = hashlib.sha256(("%s|%s|%s" % (pat["id"], device_id or 0, line[:120])).encode()).hexdigest()[:16]
    row = c.execute("SELECT id, hits FROM log_findings WHERE sig=? AND acked=0", (sig,)).fetchone()
    if row:
        c.execute("UPDATE log_findings SET hits=hits+1, last_ts=datetime('now') WHERE id=?",
                  (row["id"],))
        return None
    c.execute("""INSERT INTO log_findings
                 (pattern_id, device_id, source, line, severity, category, hint, sig, hits)
                 VALUES (?,?,?,?,?,?,?,?,1)""",
              (pat["id"], device_id, source, line[:500], pat["severity"],
               pat["category"], pat["hint"], sig))
    return pat


# ---------------------------------------------------------------- on-call


def current_oncall(c):
    """Who is on call right now.

    The newest handover wins when it is still within its window; otherwise the
    roster's time windows decide. Returns dict or None.
    """
    admins = c.execute("SELECT * FROM shift_admins WHERE active=1 ORDER BY sort_order, id").fetchall()
    if not admins:
        return None

    now = time.localtime()
    dow = now.tm_wday          # 0 = Monday
    hhmm = "%02d:%02d" % (now.tm_hour, now.tm_min)
    bit = 1 << dow

    def in_window(a):
        if not (int(a["days_mask"]) & bit):
            return False
        s, e = a["starts_at"], a["ends_at"]
        if s <= e:
            return s <= hhmm < e
        return hhmm >= s or hhmm < e     # overnight shift

    handover = c.execute("SELECT * FROM shift_handovers ORDER BY ts DESC, id DESC LIMIT 1").fetchone()
    if handover:
        # A handover is authoritative only while it is still current: for the
        # rest of the day it was recorded, plus enough slack to carry an
        # overnight shift past midnight. A forgotten overtime handover from two
        # days ago must NOT keep swallowing alerts forever - that is exactly
        # the case the next person on shift has to be able to trigger past.
        try:
            win_h = int(c.execute("SELECT value FROM settings WHERE key='handover_valid_h'")
                        .fetchone()[0])
        except Exception:
            win_h = 12
        fresh = c.execute("SELECT ? > datetime('now', ?)",
                          (handover["ts"], "-%d hours" % win_h)).fetchone()[0]
        due = handover["due_at"]
        if (fresh or (due and due > time.strftime("%Y-%m-%d %H:%M:%S"))) \
                and handover["reason"] in ("manual", "overtime", "escalation", "auto"):
            a = c.execute("SELECT * FROM shift_admins WHERE id=?", (handover["to_admin"],)).fetchone()
            if a and int(a["active"]):
                return a

    prim = [a for a in admins if in_window(a) and a["role"] == "primary"]
    if prim:
        return prim[0]
    back = [a for a in admins if in_window(a) and a["role"] == "backup"]
    if back:
        return back[0]
    # nobody is formally on shift: fall back to the first active primary so
    # alerts are never silently discarded
    first = [a for a in admins if a["role"] == "primary"]
    return first[0] if first else admins[0]


def oncall_state(c):
    """Everything the UI needs to show the banner: who, and whether overdue."""
    a = current_oncall(c)
    handover = c.execute("SELECT * FROM shift_handovers ORDER BY ts DESC, id DESC LIMIT 1").fetchone()
    overdue = False
    due_at = None
    if handover and handover["due_at"]:
        due_at = handover["due_at"]
        overdue = handover["ts"] < handover["due_at"]
    return a, handover, overdue, due_at


def send_mail(to_addr, subject, body):
    """Return (ok, error, transport)."""
    msg = EmailMessage()
    msg["From"] = MAIL_FROM
    msg["To"] = to_addr
    msg["Subject"] = subject
    msg["Date"] = formatdate(localtime=True)
    msg["Message-ID"] = make_msgid(domain="lab.local")
    msg.set_content(body)

    if SMTP_HOST:
        try:
            if SMTP_TLS:
                s = smtplib.SMTP_SSL(SMTP_HOST, SMTP_PORT, timeout=6)
            else:
                s = smtplib.SMTP(SMTP_HOST, SMTP_PORT, timeout=6)
            try:
                if not SMTP_TLS:
                    try:
                        s.starttls()
                    except Exception:
                        pass
                if SMTP_USER:
                    s.login(SMTP_USER, SMTP_PASS)
                s.send_message(msg)
            finally:
                s.quit()
            return True, None, "smtp"
        except Exception as e:
            return False, str(e)[:300], "smtp"

    # no relay configured: keep a real message on disk so the flow is provable
    try:
        os.makedirs(MAIL_DIR, exist_ok=True)
        fn = os.path.join(MAIL_DIR, "%d-%s.eml" % (int(time.time() * 1000),
                                                  re.sub(r"[^A-Za-z0-9]+", "_", to_addr)[:40]))
        with open(fn, "w", encoding="utf-8") as f:
            f.write(msg.as_string())
        return True, None, "file"
    except Exception as e:
        return False, str(e)[:300], "file"


def consider_alerts(c, new_event_ids):
    """Match the events just written against the rules, mail the on-call admin."""
    if not new_event_ids:
        return
    rules = c.execute("SELECT * FROM alert_rules WHERE enabled=1").fetchall()
    if not rules:
        return
    admin = current_oncall(c)

    qs = ",".join("?" * len(new_event_ids))
    events = c.execute("SELECT id, device_id, severity, message, ts FROM events WHERE id IN (%s)" % qs,
                       tuple(new_event_ids)).fetchall()

    devname = {}
    for d in c.execute("SELECT id, hostname FROM devices").fetchall():
        devname[d["id"]] = d["hostname"]

    for ev in events:
        ev_rank = SEVERITY_RANK.get(ev["severity"], 0)
        for r in rules:
            if ev_rank < SEVERITY_RANK.get(r["min_severity"], 0):
                continue
            if r["match_like"] and r["match_like"].replace("%", "").lower() not in ev["message"].lower():
                continue
            # throttle: one mail per rule per cooldown window
            cool = r["cooldown_s"]
            cool = DEFAULT_COOLDOWN_S if cool is None else int(cool)
            recent = c.execute(
                "SELECT COUNT(*) FROM alert_deliveries WHERE rule_id=? AND status='sent' "
                "AND ts > datetime('now', ?)", (r["id"], "-%d seconds" % cool)).fetchone()[0]
            if recent:
                c.execute("""INSERT INTO alert_deliveries(rule_id, event_id, admin_id, to_addr,
                             subject, status, error) VALUES (?,?,?,?,?,?,?)""",
                          (r["id"], ev["id"], admin["id"] if admin else None, "", r["name"],
                           "suppressed", "cooldown %ss" % cool))
                continue

            recips = []
            if r["recipients"] == "oncall":
                if admin:
                    recips = [admin["email"]]
            elif r["recipients"] == "all":
                recips = [a["email"] for a in c.execute(
                    "SELECT email FROM shift_admins WHERE active=1").fetchall()]
            else:
                recips = [x.strip() for x in r["recipients"].split(",") if x.strip()]

            if not recips:
                c.execute("""INSERT INTO alert_deliveries(rule_id, event_id, admin_id, to_addr,
                             subject, status, error) VALUES (?,?,?,?,?,?,?)""",
                          (r["id"], ev["id"], None, "", r["name"], "failed",
                           "no on-call admin configured"))
                continue

            where = devname.get(ev["device_id"], "lab")
            subject = "[%s] %s: %s" % (ev["severity"].upper(), where, r["name"])
            body = (
                "Rule:      %s\n"
                "Device:    %s\n"
                "Severity:  %s\n"
                "Time:      %s\n"
                "\n%s\n"
                "\nOn call:   %s <%s>\n"
                "\n-- lab dashboard\n" % (
                    r["name"], where, ev["severity"], ev["ts"], ev["message"],
                    admin["name"] if admin else "(none)", admin["email"] if admin else "")
            )
            for to in recips:
                ok, err, transport = send_mail(to, subject, body)
                c.execute("""INSERT INTO alert_deliveries(rule_id, event_id, admin_id, to_addr,
                             subject, status, error, tries) VALUES (?,?,?,?,?,?,?,?)""",
                          (r["id"], ev["id"], admin["id"] if admin else None, to, subject,
                           "sent" if ok else "failed", err if err else transport, 1))


# ---------------------------------------------------------------- cycle


def redispatch(c, hours=8):
    """Re-send the alerts that are still open to whoever is on call now.

    This is the "the shift changed but nobody updated the rota" path: the
    incoming admin presses one button and every unresolved event of the last
    `hours` is mailed to them, so nothing sits unread in a mailbox that is no
    longer being watched. Returns (sent, recipients).
    """
    admin = current_oncall(c)
    if not admin or not admin["email"]:
        return 0, []

    events = c.execute("""SELECT id, device_id, severity, message, ts FROM events
                          WHERE ts > datetime('now', ?) AND acked=0
                          ORDER BY id DESC LIMIT 40""", ("-%d hours" % int(hours),)).fetchall()
    if not events:
        return 0, []

    devname = {d["id"]: d["hostname"] for d in
               c.execute("SELECT id, hostname FROM devices").fetchall()}

    lines = []
    worst = "info"
    for ev in events:
        who = devname.get(ev["device_id"], "lab")
        if SEVERITY_RANK.get(ev["severity"], 0) > SEVERITY_RANK.get(worst, 0):
            worst = ev["severity"]
        lines.append("%-8s %-19s %-10s %s" % (ev["ts"], who, ev["severity"], ev["message"]))

    subject = "[%s] %d open alert(s) reassigned to %s" % (
        worst.upper(), len(events), admin["name"])
    body = (
        "You are now the on-call administrator. %d unresolved alert(s):\n\n"
        "%s\n\n"
        "Handover note: the previous shift did not clear these, so they are\n"
        "re-sent to you automatically. Acknowledge them in the dashboard.\n"
        "\n-- lab dashboard\n" % (len(events), "\n".join(lines)))

    ok, err, transport = send_mail(admin["email"], subject, body)
    # synthetic rule_id 0: this is a handover dump, not a rule match
    c.execute("""INSERT INTO alert_deliveries(rule_id, event_id, admin_id, to_addr,
                 subject, status, error, tries, kind)
                 VALUES (?,?,?,?,?,?,?,?,?)""",
              (None, events[0]["id"], admin["id"], admin["email"], subject,
               "sent" if ok else "failed", err if err else transport, 1, "redispatch"))
    return (1 if ok else 0), [admin["email"]]


def cycle(c):
    devs = c.execute("SELECT * FROM devices").fetchall()
    if not devs:
        return

    patterns = c.execute("SELECT * FROM log_patterns WHERE enabled=1").fetchall()
    subnet_rows = c.execute("SELECT cidr, vlan_id FROM subnets").fetchall()

    polled = []
    arp_by_dev = {}
    unanswered = []

    for d in devs:
        if d["kind"] == "server":
            # the controller monitors itself; no SNMP daemon required
            try:
                up_s = int(float(open("/proc/uptime").read().split()[0]))
            except Exception:
                up_s = None
            try:
                ifn = str(sum(1 for _ in open("/proc/net/dev")) - 2)
            except Exception:
                ifn = None
            polled.append((d, "up", local_metrics(), str(up_s * 100) if up_s else None, ifn, None))
            continue

        args = auth(d)
        t0 = time.time()
        up = get(d["mgmt_ip"], args, O_UPTIME)
        rtt = int((time.time() - t0) * 1000) if up else None
        ifn = get(d["mgmt_ip"], args, O_IFNUM)
        state = "up" if up else "down"
        m = metric_metrics(d["mgmt_ip"], args) if state == "up" else {}
        polled.append((d, state, m, up, ifn, rtt))
        if state == "up":
            # Key on the database hostname, never on SNMP sysName: two boxes
            # with a default sysName would otherwise overwrite each other's
            # ARP table. sysName is only kept as a rename hint.
            entries = arp_entries(d["mgmt_ip"], args, c)
            sys_name = get(d["mgmt_ip"], args, O_SYSNAME)
            for e in entries:
                e["hostname"] = None
                e["sys_name"] = sys_name
            arp_by_dev.setdefault(d["hostname"], []).extend(entries)
        else:
            unanswered.append(d)

    new_events = []

    # ---- 1. health samples, reachability, reboot detection
    for d, state, m, up, ifn, rtt in polled:
        up_s = parse_ticks(up)
        # The gap must be measured against the last successful poll, so capture
        # it BEFORE this cycle marks the device as reachable again.
        gap = int(time.time() - LAST_OK.get(d["id"], time.time())) if state == "up" else None

        c.execute("""INSERT INTO health_samples
                     (device_id, uptime_ticks, if_total, state,
                      cpu_pct, mem_pct, disk_pct, net_in_bps, net_out_bps)
                     VALUES (?,?,?,?,?,?,?,?,?)""",
                  (d["id"], int(up) if up and up.isdigit() else None,
                   int(ifn) if ifn and ifn.isdigit() else None, state,
                   m.get("cpu_pct"), m.get("mem_pct"), m.get("disk_pct"),
                   m.get("net_in_bps"), m.get("net_out_bps")))

        if d["kind"] != "server":
            c.execute("INSERT INTO reach_samples(device_id, reachable, rtt_ms) VALUES (?,?,?)",
                      (d["id"], 1 if state == "up" else 0, rtt))

        # Only write a state-change event on a real transition. `last_state` is
        # NULL on the very first poll, and that is not "came up" - it would
        # otherwise mail an alert on every fresh database.
        if d["last_state"] and state != d["last_state"]:
            ev = emit(c, d["id"], "average" if state == "down" else "info",
                      "state %s -> %s" % (d["last_state"], state))
            if ev:
                new_events.append(ev)
            if state == "down":
                ev2 = emit(c, d["id"], "average", "%s unreachable" % d["hostname"])
                if ev2:
                    new_events.append(ev2)

        # uptime went backwards => the box restarted
        if up_s is not None and d["kind"] != "server":
            prev = UPTIME.get(d["id"])
            if prev is not None and up_s + REBOOT_DROP_S < prev:
                g = gap if gap is not None else 0
                kind = "power-loss" if g >= POWER_GAP_S else "restart"
                c.execute("""INSERT INTO device_reboots
                             (device_id, prev_uptime_s, new_uptime_s, gap_s, kind, note)
                             VALUES (?,?,?,?,?,?)""",
                          (d["id"], prev, up_s, g, kind,
                           "uptime %ds -> %ds, mat %ds" % (prev, up_s, g)))
                ev = emit(c, d["id"], "high",
                          "%s %s: uptime %ds -> %ds (restart)" % (
                              d["hostname"], "power loss" if kind == "power-loss" else "restart",
                              prev, up_s))
                if ev:
                    new_events.append(ev)
            UPTIME[d["id"]] = up_s

        if state == "up":
            LAST_OK[d["id"]] = time.time()

        c.execute("UPDATE devices SET last_state=?, last_seen=datetime('now') WHERE id=?",
                  (state, d["id"]))

    # ---- 2. leases
    try:
        seen_count, changes = collect_leases(c, devs, arp_by_dev, subnet_rows)
    except Exception as e:
        print("lease error:", e, flush=True)
        seen_count, changes = 0, []

    # ---- 3. automated error detection over what we just wrote
    # `emit` already deduplicates by signature only against *blocked* events,
    # so failures have to be rate-limited here or the table grows by two rows
    # every 5 s for every broken device.
    for d, state, m, up, ifn, rtt in polled:
        if state != "up":
            pat = classify("SNMP timeout: no response", patterns)
            if pat:
                record_finding(c, pat, d["id"],
                               "SNMP timeout: no response from %s (%s)" % (d["mgmt_ip"], d["hostname"]))
        if m.get("disk_pct") and m["disk_pct"] >= 95:
            pat = classify("No space left on device", patterns)
            if pat:
                record_finding(c, pat, d["id"], "No space left on device (%.0f%%)" % m["disk_pct"])

    # ---- 3b. read the devices' own syslog and classify it
    # SNMP only says "up / down"; the syslog is where a link flap, a rejected
    # login or a full disk actually shows up. Only reachable devices are read,
    # and only every Nth cycle, because an SSH session costs far more than an
    # SNMP get and the log does not change enough to justify 5 s.
    global LOG_TICK
    # Fire on the first cycle, then every Nth.  A modulo written as
    # `(t+1) % n == 0` would skip the first scan entirely after a restart,
    # which is exactly when the operator is watching.
    due = LOG_TICK == 0
    LOG_TICK = (LOG_TICK + 1) % max(1, LOG_EVERY_N)
    if due:
        try:
            n = collect_logs(c, [d for d, st, *_ in polled if st == "up"], patterns)
            if n:
                print("log scan: %d new finding(s)" % n, flush=True)
        except Exception as e:
            print("log scan error:", e, flush=True)

    # ---- 4. mail whoever is on call
    try:
        consider_alerts(c, new_events)
    except Exception as e:
        print("alert error:", e, flush=True)

    if len(unanswered) == len([d for d in devs if d["kind"] != "server"]) and devs:
        ev = emit(c, None, "disaster", "ALL %d devices unreachable" % len(devs))
        if ev:
            new_events.append(ev)
            consider_alerts(c, [ev])
        if HOOK:
            try:
                import urllib.request
                urllib.request.urlopen(urllib.request.Request(
                    HOOK, data=b'{"level":"disaster","msg":"all devices down"}',
                    headers={"Content-Type": "application/json"}, method="POST"), timeout=5)
            except Exception as e:
                print("webhook failed:", e, flush=True)

    # ---- 5. housekeeping, once in a while
    if int(time.time()) % 600 < INTERVAL:
        c.execute("DELETE FROM reach_samples WHERE ts < datetime('now', ?)",
                  ("-%d days" % REACH_KEEP_DAYS,))
        c.execute("DELETE FROM health_samples WHERE ts < datetime('now', '-14 days')")

    c.commit()
    print("cycle: %d up / %d down | leases %d seen, %d changed"
          % (len(devs) - len(unanswered), len(unanswered), seen_count, len(changes)), flush=True)


UPTIME = {}
LAST_OK = {}


def seed_uptime(c):
    """Remember the last uptime we saw so a restart across a poller restart
    is still detected."""
    for r in c.execute("""SELECT device_id, uptime_ticks FROM health_samples h
                          WHERE ts = (SELECT MAX(ts) FROM health_samples
                                      WHERE device_id = h.device_id)
                          AND uptime_ticks IS NOT NULL""").fetchall():
        try:
            UPTIME[r["device_id"]] = int(r["uptime_ticks"] / 100)
        except Exception:
            pass


def main():
    while not os.path.exists(DB):
        print("waiting for", DB, flush=True)
        time.sleep(2)
    first = True
    while True:
        started = time.time()
        try:
            c = conn()
            if first:
                seed_uptime(c)
                first = False
            cycle(c)
            c.close()
        except Exception as e:
            print("cycle error:", e, flush=True)
        # keep the cadence stable: subtract the time the cycle took
        spent = time.time() - started
        time.sleep(max(0.5, INTERVAL - spent))


if __name__ == "__main__":
    sys.exit(main())
