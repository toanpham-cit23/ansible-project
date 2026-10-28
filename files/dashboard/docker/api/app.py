"""FastAPI + SQLite dashboard. No ORM, no session store beyond a jti table."""
import base64
import hashlib
import hmac
import json
import os
import secrets
import sqlite3
import time
from contextlib import contextmanager

from fastapi import Depends, FastAPI, HTTPException, Request, Response
from fastapi.responses import FileResponse, JSONResponse
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, Field

DB = os.environ.get("NETDB", "/data/net.db")
SECRET = os.environ.get("JWT_SECRET", "")
COOKIE_SECURE = os.environ.get("COOKIE_SECURE", "false").lower() == "true"
TTL = int(os.environ.get("TOKEN_TTL", "1800"))
HERE = os.path.dirname(os.path.abspath(__file__))
STATIC = os.path.join(HERE, "static")

if len(SECRET) < 32:
    raise RuntimeError("JWT_SECRET must be at least 32 chars")

app = FastAPI(title="lab", docs_url=None, redoc_url=None, openapi_url=None)


@contextmanager
def db(write=False):
    con = sqlite3.connect(DB, timeout=5)
    con.row_factory = sqlite3.Row
    con.execute("PRAGMA busy_timeout=15000")
    con.execute("PRAGMA foreign_keys=ON")
    if write:
        con.execute("PRAGMA synchronous=NORMAL")
    try:
        yield con
        if write:
            con.commit()
    except Exception:
        con.rollback()
        raise
    finally:
        con.close()


def rows(sql, args=()):
    with db() as c:
        return [dict(r) for r in c.execute(sql, args)]


def b64(b):
    return base64.urlsafe_b64encode(b).rstrip(b"=")


def unb64(s):
    return base64.urlsafe_b64decode(s + b"=" * (-len(s) % 4))


def hash_pw(pw):
    salt = secrets.token_bytes(16)
    dk = hashlib.scrypt(pw.encode(), salt=salt, n=2 ** 14, r=8, p=1, dklen=32)
    return "scrypt$16384$8$1$%s$%s" % (salt.hex(), dk.hex())


def verify_pw(pw, stored):
    try:
        _, n, r, p, salt, want = stored.split("$")
        dk = hashlib.scrypt(pw.encode(), salt=bytes.fromhex(salt),
                            n=int(n), r=int(r), p=int(p), dklen=32)
        return hmac.compare_digest(dk.hex(), want)
    except Exception:
        return False


def mint(user_id, jti):
    head = b64(json.dumps({"alg": "HS256", "typ": "JWT"}, separators=(",", ":")).encode())
    body = b64(json.dumps({"sub": user_id, "jti": jti, "exp": int(time.time()) + TTL},
                          separators=(",", ":")).encode())
    sig = b64(hmac.new(SECRET.encode(), head + b"." + body, hashlib.sha256).digest())
    return b".".join([head, body, sig]).decode()


def parse(token):
    try:
        head, body, sig = token.encode().split(b".")
        if json.loads(unb64(head))["alg"] != "HS256":
            return None
        good = b64(hmac.new(SECRET.encode(), head + b"." + body, hashlib.sha256).digest())
        if not hmac.compare_digest(good, sig):
            return None
        claim = json.loads(unb64(body))
        return claim if claim["exp"] > time.time() else None
    except Exception:
        return None


ATTEMPTS = {}


def rate_ok(ip):
    now = time.time()
    hits = [t for t in ATTEMPTS.get(ip, []) if now - t < 300]
    ATTEMPTS[ip] = hits
    if len(hits) >= 5:
        return False
    hits.append(now)
    return True


def current(request: Request):
    tok = request.cookies.get("sess")
    claim = parse(tok) if tok else None
    if not claim:
        raise HTTPException(401, "auth required")
    with db() as c:
        row = c.execute("""SELECT u.id, u.username, u.role FROM sessions s
                           JOIN users u ON u.id = s.user_id
                           WHERE s.jti = ? AND s.expires_at > datetime('now')""",
                        (claim["jti"],)).fetchone()
    if not row:
        raise HTTPException(401, "session revoked")
    return dict(row)


def admin(user=Depends(current)):
    if user["role"] != "admin":
        raise HTTPException(403, "admin only")
    return user


class Login(BaseModel):
    username: str = Field(min_length=1, max_length=64)
    password: str = Field(min_length=1, max_length=256)


class DevicePatch(BaseModel):
    mgmt_ip: str | None = Field(default=None, max_length=64)
    ssh_port: int | None = Field(default=None, ge=1, le=65535)
    telnet_port: int | None = Field(default=None, ge=1, le=65535)
    asset_tag: str | None = Field(default=None, max_length=64)
    notes: str | None = Field(default=None, max_length=500)
    kind_override: str | None = None


class Credentials(BaseModel):
    username: str = Field(max_length=64)
    password: str = Field(max_length=256)


RANGES = {"5m": 5, "15m": 15, "30m": 30, "60m": 60, "6h": 360, "12h": 720, "24h": 1440, "all": 0}


@app.get("/healthz")
def healthz():
    try:
        rows("SELECT 1")
        return {"ok": True}
    except Exception:
        return JSONResponse({"ok": False}, status_code=503)


@app.post("/api/login")
def login(body: Login, request: Request, response: Response):
    ip = request.client.host if request.client else "?"
    if not rate_ok(ip):
        raise HTTPException(429, "too many attempts")
    with db() as c:
        u = c.execute("SELECT * FROM users WHERE username = ?", (body.username,)).fetchone()
    if not u or not verify_pw(body.password, u["pw_hash"]):
        raise HTTPException(401, "bad credentials")
    jti = secrets.token_urlsafe(16)
    with db(write=True) as c:
        c.execute("""INSERT INTO sessions(jti, user_id, expires_at, ip, ua)
                     VALUES (?,?,datetime('now', ?),?,?)""",
                  (jti, u["id"], "+%d seconds" % TTL, ip,
                   (request.headers.get("user-agent") or "")[:180]))
    response.set_cookie("sess", mint(u["id"], jti), max_age=TTL, httponly=True,
                        samesite="strict", secure=COOKIE_SECURE, path="/")
    return {"ok": True, "user": u["username"], "role": u["role"]}


@app.post("/api/logout")
def logout(request: Request, response: Response, user=Depends(current)):
    tok = request.cookies.get("sess")
    claim = parse(tok) if tok else None
    if claim:
        with db(write=True) as c:
            c.execute("DELETE FROM sessions WHERE jti = ?", (claim["jti"],))
    response.delete_cookie("sess", path="/")
    return {"ok": True}


@app.get("/api/me")
def me(user=Depends(current)):
    cfg = {r["key"]: r["value"] for r in rows("SELECT key, value FROM settings")}
    return {"user": user["username"], "role": user["role"],
            "theme": cfg.get("theme", "light"), "layout": cfg.get("layout", "detailed")}


@app.get("/api/summary")
def summary(user=Depends(current)):
    return rows("""SELECT effective_kind AS kind, COUNT(*) AS n,
                          SUM(last_state='up') AS up, SUM(last_state='down') AS down
                   FROM v_inventory GROUP BY effective_kind""")


@app.get("/api/devices")
def devices(user=Depends(current)):
    return rows("SELECT * FROM v_inventory ORDER BY hostname")


@app.patch("/api/devices/{dev_id}")
def patch_device(dev_id: int, body: DevicePatch, user=Depends(admin)):
    sets, args = [], []
    for k, v in body.model_dump(exclude_none=True).items():
        if k == "kind_override" and v not in ("router", "l3switch", "switch", ""):
            raise HTTPException(400, "bad kind_override")
        sets.append("%s = ?" % k)
        args.append(v or None)
    if not sets:
        return {"ok": True, "changed": 0}
    args.append(dev_id)
    with db(write=True) as c:
        cur = c.execute("UPDATE devices SET %s WHERE id = ?" % ", ".join(sets), args)
        if cur.rowcount == 0:
            raise HTTPException(404, "no such device")
    return {"ok": True, "changed": cur.rowcount}


@app.put("/api/devices/{dev_id}/credentials")
def put_credentials(dev_id: int, body: Credentials, user=Depends(admin)):
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM
    key = os.environ.get("NETDB_KEY", "")
    if len(key) != 64:
        raise HTTPException(503, "NETDB_KEY not configured")
    nonce = os.urandom(12)
    blob = nonce + AESGCM(bytes.fromhex(key)).encrypt(nonce, body.password.encode(), None)
    with db(write=True) as c:
        cur = c.execute("UPDATE devices SET username = ?, password_enc = ? WHERE id = ?",
                        (body.username, blob, dev_id))
        if cur.rowcount == 0:
            raise HTTPException(404, "no such device")
    return {"ok": True}


@app.get("/api/devices/{dev_id}/health")
def health(dev_id: int, range: str = "5m", user=Depends(current)):
    if range == "live":
        out = rows("""SELECT ts, cpu_pct, mem_pct, disk_pct, net_in_bps, net_out_bps
                      FROM health_samples WHERE device_id = ?
                      ORDER BY ts DESC LIMIT 60""", (dev_id,))
        return out[::-1]
    if range not in RANGES:
        raise HTTPException(400, "bad range")
    if range == "all":
        raw = rows("""SELECT ts, cpu_pct, mem_pct, disk_pct, net_in_bps, net_out_bps
                        FROM health_samples WHERE device_id = ? ORDER BY ts""", (dev_id,))
        step = max(1, len(raw) // 240)
        return raw[::step]
    return rows("""SELECT strftime('%Y-%m-%d %H:%M', ts) AS ts,
                          ROUND(AVG(cpu_pct),1) cpu_pct, ROUND(AVG(mem_pct),1) mem_pct,
                          ROUND(AVG(disk_pct),1) disk_pct,
                          ROUND(AVG(net_in_bps),0) net_in_bps,
                          ROUND(AVG(net_out_bps),0) net_out_bps
                   FROM health_samples
                   WHERE device_id = ? AND ts >= datetime('now', ?)
                   GROUP BY ts ORDER BY ts""",
                (dev_id, "-%d minutes" % RANGES[range]))


@app.get("/api/events")
def events(limit: int = 50, severity: str | None = None, user=Depends(current)):
    limit = max(1, min(limit, 500))
    sql = """SELECT e.ts, e.severity, e.message, d.hostname FROM events e
             LEFT JOIN devices d ON d.id = e.device_id"""
    args = ()
    if severity:
        sql += " WHERE e.severity = ?"
        args = (severity,)
    return rows(sql + " ORDER BY e.ts DESC LIMIT ?", args + (limit,))


@app.get("/api/topology")
def topology(user=Depends(current)):
    return {"nodes": rows("SELECT id, hostname, mgmt_ip, effective_kind, last_state "
                          "FROM v_inventory"),
            "links": rows("SELECT a_device_id, b_device_id, if_a, if_b, proto FROM links")}


@app.get("/api/settings")
def get_settings(user=Depends(current)):
    return {r["key"]: r["value"] for r in rows("SELECT key, value FROM settings")}


@app.put("/api/settings")
def put_settings(body: dict, user=Depends(admin)):
    allow = {"theme": {"light", "dark"}, "layout": {"detailed", "simple"}}
    with db(write=True) as c:
        for k, v in body.items():
            if k not in allow or str(v) not in allow[k]:
                raise HTTPException(400, "bad setting: %s" % k)
            c.execute("INSERT INTO settings(key, value) VALUES(?,?) "
                      "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (k, str(v)))
    return {"ok": True}



class TermIn(BaseModel):
    cmd: str = Field(max_length=200)


ALLOWED = (
    "show ip ospf neighbor", "show ip route summary", "show ip route",
    "show version", "show interfaces", "show run", "show memory",
    "uptime", "free -m", "df -h", "ip -br a", "cat /etc/frr/frr.conf",
)


@app.post("/api/terminal/{dev_id}/exec")
def term_exec(dev_id: int, body: TermIn, user=Depends(admin)):
    import paramiko

    cmd = body.cmd.strip()
    if not any(cmd == a or cmd.startswith(a + " ") for a in ALLOWED):
        raise HTTPException(403, "command not in allowlist")

    with db() as c:
        d = c.execute("SELECT * FROM devices WHERE id = ?", (dev_id,)).fetchone()
    if not d:
        raise HTTPException(404, "no such device")

    from cryptography.hazmat.primitives.ciphers.aead import AESGCM
    key = os.environ.get("NETDB_KEY", "")
    pw = None
    if len(key) == 64 and d["password_enc"]:
        b = bytes(d["password_enc"])
        pw = AESGCM(bytes.fromhex(key)).decrypt(b[:12], b[12:], None).decode()
    if not pw:
        raise HTTPException(503, "no stored credential for this device")

    remote = "vtysh -c '%s'" % cmd.replace("'", "") if cmd.startswith("show") else cmd
    try:
        cl = paramiko.SSHClient()
        cl.set_missing_host_key_policy(paramiko.AutoAddPolicy())
        cl.connect(d["mgmt_ip"], port=d["ssh_port"] or 22,
                   username=d["username"] or "root", password=pw,
                   timeout=8, allow_agent=False, look_for_keys=False)
        _in, out, err = cl.exec_command(remote, timeout=15)
        txt = out.read().decode("utf-8", "replace") + err.read().decode("utf-8", "replace")
        cl.close()
    except Exception as e:
        raise HTTPException(502, "ssh failed: %s" % e)
    with db(write=True) as c:
        c.execute("INSERT INTO events(device_id, severity, message) VALUES (?,?,?)",
                  (dev_id, "info", "terminal: %s" % cmd[:120]))
    return {"host": d["hostname"], "cmd": cmd, "output": txt[-20000:]}


class PwChange(BaseModel):
    old: str = Field(min_length=1, max_length=256)
    new: str = Field(min_length=8, max_length=256)


class NewUser(BaseModel):
    username: str = Field(min_length=3, max_length=32, pattern=r"^[A-Za-z0-9_.-]+$")
    password: str = Field(min_length=8, max_length=256)
    role: str = "viewer"


@app.post("/api/account/password")
def change_password(body: PwChange, user=Depends(current)):
    with db() as c:
        row = c.execute("SELECT pw_hash FROM users WHERE id = ?", (user["id"],)).fetchone()
    if not row or not verify_pw(body.old, row["pw_hash"]):
        raise HTTPException(401, "current password is incorrect")
    with db(write=True) as c:
        c.execute("UPDATE users SET pw_hash = ? WHERE id = ?", (hash_pw(body.new), user["id"]))
    return {"ok": True}


@app.get("/api/users")
def list_users(user=Depends(admin)):
    return rows("SELECT id, username, role, created_at FROM users ORDER BY id")


@app.post("/api/users")
def add_user(body: NewUser, user=Depends(admin)):
    if body.role not in ("viewer", "admin"):
        raise HTTPException(400, "bad role")
    try:
        with db(write=True) as c:
            c.execute("INSERT INTO users(username, pw_hash, role) VALUES(?,?,?)",
                      (body.username, hash_pw(body.password), body.role))
    except sqlite3.IntegrityError:
        raise HTTPException(409, "username already exists")
    return {"ok": True}


@app.patch("/api/users/{uid}")
def set_role(uid: int, body: dict, user=Depends(admin)):
    role = body.get("role")
    if role not in ("viewer", "admin"):
        raise HTTPException(400, "bad role")
    with db(write=True) as c:
        cur = c.execute("UPDATE users SET role = ? WHERE id = ?", (role, uid))
        if cur.rowcount == 0:
            raise HTTPException(404, "no such user")
    return {"ok": True}


@app.delete("/api/users/{uid}")
def del_user(uid: int, user=Depends(admin)):
    if uid == user["id"]:
        raise HTTPException(400, "cannot delete the account you are using")
    with db(write=True) as c:
        cur = c.execute("DELETE FROM users WHERE id = ?", (uid,))
        if cur.rowcount == 0:
            raise HTTPException(404, "no such user")
    return {"ok": True}


def bootstrap():
    with db(write=True) as c:
        if c.execute("SELECT COUNT(*) FROM users").fetchone()[0]:
            return
        user = os.environ.get("ADMIN_USER", "admin")
        pw = os.environ.get("ADMIN_PASSWORD") or secrets.token_urlsafe(12)
        c.execute("INSERT INTO users(username, pw_hash, role) VALUES(?,?,'admin')",
                  (user, hash_pw(pw)))
        c.executemany("INSERT OR IGNORE INTO settings(key, value) VALUES(?,?)",
                      [("theme", "light"), ("layout", "detailed")])
        print("BOOTSTRAP admin=%s password=%s" % (user, pw), flush=True)


@app.on_event("startup")
def _start():
    for _ in range(30):
        if os.path.exists(DB):
            break
        time.sleep(1)
    bootstrap()


app.mount("/static", StaticFiles(directory=STATIC), name="static")


NOCACHE = {"Cache-Control": "no-store, max-age=0"}


@app.get("/")
def index():
    return FileResponse(os.path.join(STATIC, "index.html"), headers=NOCACHE)


@app.get("/app")
def shell():
    return FileResponse(os.path.join(STATIC, "app.html"), headers=NOCACHE)
