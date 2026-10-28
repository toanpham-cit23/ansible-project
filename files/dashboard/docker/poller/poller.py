"""SNMP health poller. Writes health_samples + events. Never configures devices."""
import hashlib
import os
import sqlite3
import subprocess
import sys
import time
import urllib.request

DB = os.environ.get("NETDB", "/data/net.db")
INTERVAL = int(os.environ.get("POLL_INTERVAL", "60"))
HOOK = os.environ.get("ALERT_WEBHOOK", "").strip()

O_UPTIME = "1.3.6.1.2.1.1.3.0"
O_IFNUM = "1.3.6.1.2.1.2.1.0"
O_CPU = "1.3.6.1.2.1.25.3.3.1.2"
O_ST_TYPE = "1.3.6.1.2.1.25.2.3.1.2"
O_ST_UNITS = "1.3.6.1.2.1.25.2.3.1.4"
O_ST_SIZE = "1.3.6.1.2.1.25.2.3.1.5"
O_ST_USED = "1.3.6.1.2.1.25.2.3.1.6"
O_IN = "1.3.6.1.2.1.31.1.1.1.6"
O_OUT = "1.3.6.1.2.1.31.1.1.1.10"

T_RAM = "1.3.6.1.2.1.25.2.1.2"
T_DISK = "1.3.6.1.2.1.25.2.1.4"

PREV = {}


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


def get(host, args, oid):
    try:
        p = subprocess.run(["snmpget", *args, "-Oqv", "-t", "2", "-r", "1", host, oid],
                           capture_output=True, text=True, timeout=6)
        v = p.stdout.strip().strip('"')
        return v if p.returncode == 0 and v and v != "NONE" else None
    except Exception:
        return None


def walk(host, args, oid):
    """-> {index: value}, numeric OIDs."""
    try:
        p = subprocess.run(["snmpwalk", *args, "-On", "-t", "2", "-r", "1", host, oid],
                           capture_output=True, text=True, timeout=10)
    except Exception:
        return {}
    out = {}
    for line in p.stdout.splitlines():
        if "=" not in line:
            continue
        left, right = line.split("=", 1)
        idx = left.strip().rsplit(".", 1)[-1]
        val = right.strip()
        if ":" in val:
            val = val.split(":", 1)[1].strip()
        val = val.strip('"')
        try:
            out[idx] = int(val)
        except ValueError:
            out[idx] = val.lstrip(".")
    return out


def metrics(host, args):
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
        for i, t in types.items():
            if not str(t).startswith(kind):
                continue
            try:
                s_i = int(size.get(i, 0))
                g_i = int(used.get(i, 0))
            except (TypeError, ValueError):
                continue
            parts = str(units.get(i, 1)).split()
            try:
                u_i = int(parts[0]) if parts else 1
            except ValueError:
                u_i = 1
            tot += s_i * u_i
            got += g_i * u_i
        return round(100.0 * got / tot, 1) if tot else None

    m["mem_pct"] = pct(T_RAM)
    m["disk_pct"] = pct(T_DISK)

    rin = sum(v for k, v in walk(host, args, O_IN).items() if isinstance(v, int) and k != "1")
    rout = sum(v for k, v in walk(host, args, O_OUT).items() if isinstance(v, int) and k != "1")
    now = time.time()
    prev = PREV.get(host)
    if prev and now > prev[0] and (rin or rout):
        dt = now - prev[0]
        m["net_in_bps"] = round(max(0, rin - prev[1]) * 8 / dt, 1)
        m["net_out_bps"] = round(max(0, rout - prev[2]) * 8 / dt, 1)
    if rin or rout:
        PREV[host] = (now, rin, rout)
    return m



LOCAL_PREV = {}


def local_metrics():
    """Metrics for the controller itself, read from /proc (no SNMP daemon)."""
    import os
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


def sig_of(device_id, severity, message):
    """Same signature the Go API computes, so a Block decision applies here too."""
    raw = "%d|%s|%s" % (device_id or 0, severity, message)
    return hashlib.sha256(raw.encode()).hexdigest()[:16]


def emit(c, device_id, severity, message):
    sig = sig_of(device_id, severity, message)
    row = c.execute("SELECT 1 FROM log_decisions WHERE sig=? AND action='block'",
                    (sig,)).fetchone()
    if row:
        return
    c.execute("INSERT INTO events(device_id, severity, message, sig) VALUES (?,?,?,?)",
              (device_id, severity, message, sig))


def notify(text):
    if not HOOK:
        return
    try:
        urllib.request.urlopen(urllib.request.Request(
            HOOK, data=text.encode(), headers={"Content-Type": "application/json"},
            method="POST"), timeout=5)
    except Exception as e:
        print("webhook failed:", e, flush=True)


def cycle(c):
    """Poll everything first, then write in one short transaction.

    Holding a write transaction across the SNMP walks blocks the API for
    longer than its busy_timeout, so login fails with "database is locked".
    """
    devs = c.execute("SELECT * FROM devices").fetchall()
    if not devs:
        return

    polled = []
    for d in devs:
        if d["kind"] == "server":
            # the controller monitors itself; no SNMP daemon required
            up = str(int(float(open("/proc/uptime").read().split()[0]) * 100))
            ifn = str(sum(1 for _ in open("/proc/net/dev")) - 2)
            polled.append((d, "up", local_metrics(), up, ifn))
            continue
        args = auth(d)
        up = get(d["mgmt_ip"], args, O_UPTIME)
        ifn = get(d["mgmt_ip"], args, O_IFNUM)
        state = "up" if up else "down"
        m = metrics(d["mgmt_ip"], args) if state == "up" else {}
        polled.append((d, state, m, up, ifn))

    down = [d["hostname"] for d, state, _m, _u, _i in polled if state == "down"]

    for d, state, m, up, ifn in polled:
        c.execute("""INSERT INTO health_samples
                     (device_id, uptime_ticks, if_total, state,
                      cpu_pct, mem_pct, disk_pct, net_in_bps, net_out_bps)
                     VALUES (?,?,?,?,?,?,?,?,?)""",
                  (d["id"], int(up) if up and up.isdigit() else None,
                   int(ifn) if ifn and ifn.isdigit() else None, state,
                   m.get("cpu_pct"), m.get("mem_pct"), m.get("disk_pct"),
                   m.get("net_in_bps"), m.get("net_out_bps")))
        if state != d["last_state"]:
            emit(c, d["id"], "average" if state == "down" else "info",
                 "state %s -> %s" % (d["last_state"] or "unknown", state))
        c.execute("UPDATE devices SET last_state=?, last_seen=datetime('now') WHERE id=?",
                  (state, d["id"]))

    if len(down) == len(devs):
        emit(c, None, "disaster", "ALL %d devices unreachable" % len(devs))
        notify('{"level":"disaster","msg":"all devices down","count":%d}' % len(devs))
        print("CRITICAL: all devices down", flush=True)
    elif down:
        notify('{"level":"warning","msg":"%s down"}' % ",".join(down))

    c.commit()
    print("cycle: %d up / %d down" % (len(devs) - len(down), len(down)), flush=True)


def main():
    while not os.path.exists(DB):
        print("waiting for", DB, flush=True)
        time.sleep(5)
    while True:
        try:
            c = conn()
            cycle(c)
            c.close()
        except Exception as e:
            print("cycle error:", e, flush=True)
        time.sleep(INTERVAL)


if __name__ == "__main__":
    sys.exit(main())
