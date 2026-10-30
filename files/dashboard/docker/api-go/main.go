// Lab dashboard API — Go port of the FastAPI service.
// Single static binary, stdlib HTTP, SQLite via modernc (no cgo).
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"
	"golang.org/x/crypto/ssh"

	_ "modernc.org/sqlite"
)

var (
	dbPath       = env("NETDB", "/data/net.db")
	secret       = env("JWT_SECRET", "")
	cookieSecure = env("COOKIE_SECURE", "false") == "true"
	tokenTTL     = envInt("TOKEN_TTL", 1800)
	staticDir    = env("STATIC_DIR", "/app/static")
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return d
}

// ---------------------------------------------------------------- db

var db *sql.DB

func openDB() error {
	dsn := "file:" + dbPath + "?_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	var err error
	db, err = sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Hour)
	return db.Ping()
}

// ---------------------------------------------------------------- json

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"detail": msg})
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// ---------------------------------------------------------------- crypto

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := scrypt.Key([]byte(pw), salt, 16384, 8, 1, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("scrypt$16384$8$1$%s$%s", hex.EncodeToString(salt), hex.EncodeToString(dk)), nil
}

func verifyPassword(pw, stored string) bool {
	p := strings.Split(stored, "$")
	if len(p) != 6 || p[0] != "scrypt" {
		return false
	}
	n, _ := strconv.Atoi(p[1])
	r, _ := strconv.Atoi(p[2])
	pp, _ := strconv.Atoi(p[3])
	salt, err1 := hex.DecodeString(p[4])
	want, err2 := hex.DecodeString(p[5])
	if err1 != nil || err2 != nil {
		return false
	}
	dk, err := scrypt.Key([]byte(pw), salt, n, r, pp, 32)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(dk, want) == 1
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jwtSign(userID int64, jti string) string {
	head := b64([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := b64([]byte(fmt.Sprintf(`{"sub":%d,"jti":%q,"exp":%d}`, userID, jti, time.Now().Unix()+int64(tokenTTL))))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(head + "." + body))
	return head + "." + body + "." + b64(mac.Sum(nil))
}

func jwtParse(token string) (int64, string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, "", false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if subtle.ConstantTimeCompare(mac.Sum(nil), mustB64(parts[2])) != 1 {
		return 0, "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, "", false
	}
	var c struct {
		Sub int64  `json:"sub"`
		Jti string `json:"jti"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Exp < time.Now().Unix() {
		return 0, "", false
	}
	return c.Sub, c.Jti, true
}

func mustB64(s string) []byte {
	b, _ := base64.RawURLEncoding.DecodeString(s)
	return b
}

func aesEncrypt(key []byte, plain string) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return append(nonce, gcm.Seal(nil, nonce, []byte(plain), nil)...), nil
}

func aesDecrypt(key, blob []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(blob) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	out, err := gcm.Open(nil, blob[:gcm.NonceSize()], blob[gcm.NonceSize():], nil)
	return string(out), err
}

func netdbKey() []byte {
	h := os.Getenv("NETDB_KEY")
	if len(h) != 64 {
		return nil
	}
	k, err := hex.DecodeString(h)
	if err != nil {
		return nil
	}
	return k
}

// ---------------------------------------------------------------- auth

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

func currentUser(r *http.Request) (*User, bool) {
	c, err := r.Cookie("sess")
	if err != nil {
		return nil, false
	}
	uid, jti, ok := jwtParse(c.Value)
	if !ok {
		return nil, false
	}
	var u User
	err = db.QueryRow(`SELECT u.id, u.username, u.role FROM sessions s
	                   JOIN users u ON u.id = s.user_id
	                   WHERE s.jti = ? AND s.expires_at > datetime('now')`, jti).
		Scan(&u.ID, &u.Username, &u.Role)
	if err != nil || u.ID != uid {
		return nil, false
	}
	return &u, true
}

func withUser(h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := currentUser(r)
		if !ok {
			fail(w, http.StatusUnauthorized, "auth required")
			return
		}
		h(w, r, u)
	}
}

func withAdmin(h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return withUser(func(w http.ResponseWriter, r *http.Request, u *User) {
		if u.Role != "admin" {
			fail(w, http.StatusForbidden, "admin only")
			return
		}
		h(w, r, u)
	})
}

var attempts = map[string][]time.Time{}
var attemptMu = make(chan struct{}, 1)

// loginWindow is how long a failed attempt keeps counting, and loginMaxFailures
// is how many are tolerated inside it. Only failures are recorded: counting
// successful logins against the budget locked out an operator who reloaded the
// page a few times, which is exactly the person this is meant to let in.
const (
	loginWindow      = 5 * time.Minute
	loginMaxFailures = 10
)

// rateBlocked reports whether this address has failed too often lately.
func rateBlocked(ip string) bool {
	attemptMu <- struct{}{}
	defer func() { <-attemptMu }()
	now := time.Now()
	keep := attempts[ip][:0]
	for _, t := range attempts[ip] {
		if now.Sub(t) < loginWindow {
			keep = append(keep, t)
		}
	}
	attempts[ip] = keep
	return len(keep) >= loginMaxFailures
}

// rateFail records one failed attempt.
func rateFail(ip string) {
	attemptMu <- struct{}{}
	defer func() { <-attemptMu }()
	attempts[ip] = append(attempts[ip], time.Now())
}

// rateClearForget drops the history after a good login, so a correct password
// always resets the counter.
func rateClear(ip string) {
	attemptMu <- struct{}{}
	defer func() { <-attemptMu }()
	delete(attempts, ip)
}

// ---------------------------------------------------------------- models

type Device struct {
	ID             int64    `json:"id"`
	Hostname       string   `json:"hostname"`
	MgmtIP         string   `json:"mgmt_ip"`
	EffectiveKind  string   `json:"effective_kind"`
	KindConfidence float64  `json:"kind_confidence"`
	Vendor         *string  `json:"vendor"`
	Model          *string  `json:"model"`
	AssetTag       *string  `json:"asset_tag"`
	Site           string   `json:"site"`
	LastSeen       *string  `json:"last_seen"`
	LastState      *string  `json:"last_state"`
	SSHPort        int      `json:"ssh_port"`
	TelnetPort     int      `json:"telnet_port"`
}

const deviceSQL = `SELECT d.id, d.hostname, d.mgmt_ip,
       COALESCE(d.kind_override, d.kind) AS effective_kind,
       d.kind_confidence, d.vendor, d.model, d.asset_tag,
       COALESCE(s.name,'') AS site, d.last_seen, d.last_state,
       d.ssh_port, d.telnet_port
FROM devices d LEFT JOIN sites s ON s.id = d.site_id`

func scanDevices(rows *sql.Rows) ([]Device, error) {
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Hostname, &d.MgmtIP, &d.EffectiveKind, &d.KindConfidence,
			&d.Vendor, &d.Model, &d.AssetTag, &d.Site, &d.LastSeen, &d.LastState,
			&d.SSHPort, &d.TelnetPort); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- handlers

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := db.Ping(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]bool{"ok": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := r.RemoteAddr
	if i := strings.LastIndex(ip, ":"); i > 0 {
		ip = ip[:i]
	}
	if rateBlocked(ip) {
		fail(w, http.StatusTooManyRequests, "too many attempts")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &body); err != nil || body.Username == "" || body.Password == "" {
		fail(w, http.StatusBadRequest, "invalid request")
		return
	}
	var id int64
	var hash string
	err := db.QueryRow("SELECT id, pw_hash FROM users WHERE username = ?", body.Username).Scan(&id, &hash)
	if err != nil || !verifyPassword(body.Password, hash) {
		rateFail(ip)
		logEvent(0, "warning", "login failed for user "+body.Username)
		fail(w, http.StatusUnauthorized, "bad credentials")
		return
	}
	rateClear(ip)
	jti := randomToken(16)
	if _, err := db.Exec(`INSERT INTO sessions(jti, user_id, expires_at, ip, ua)
	                      VALUES (?,?,datetime('now', ?),?,?)`,
		jti, id, fmt.Sprintf("+%d seconds", tokenTTL), ip, truncate(r.UserAgent(), 180)); err != nil {
		fail(w, http.StatusInternalServerError, "session error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "sess", Value: jwtSign(id, jti), Path: "/", MaxAge: tokenTTL,
		HttpOnly: true, Secure: cookieSecure, SameSite: http.SameSiteStrictMode,
	})
	var role, name string
	_ = db.QueryRow("SELECT username, role FROM users WHERE id = ?", id).Scan(&name, &role)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": name, "role": role})
}

func handleLogout(w http.ResponseWriter, r *http.Request, u *User) {
	if c, err := r.Cookie("sess"); err == nil {
		if _, jti, ok := jwtParse(c.Value); ok {
			_, _ = db.Exec("DELETE FROM sessions WHERE jti = ?", jti)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "sess", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleMe(w http.ResponseWriter, r *http.Request, u *User) {
	theme, layout := "light", "detailed"
	rows, err := db.Query("SELECT key, value FROM settings")
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var k, v string
			if rows.Scan(&k, &v) == nil {
				switch k {
				case "theme":
					theme = v
				case "layout":
					layout = v
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"user": u.Username, "role": u.Role, "theme": theme, "layout": layout,
	})
}

func handleSummary(w http.ResponseWriter, r *http.Request, u *User) {
	rows, err := db.Query(`SELECT effective_kind, COUNT(*),
	                              SUM(last_state='up'), SUM(last_state='down')
	                       FROM v_inventory GROUP BY effective_kind`)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var kind string
		var n, up, down sql.NullInt64
		if rows.Scan(&kind, &n, &up, &down) == nil {
			out = append(out, map[string]any{"kind": kind, "n": n.Int64, "up": up.Int64, "down": down.Int64})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func handleDevices(w http.ResponseWriter, r *http.Request, u *User) {
	rows, err := db.Query(deviceSQL + " ORDER BY d.hostname")
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	list, err := scanDevices(rows)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func handlePatchDevice(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body map[string]any
	if err := decode(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid request")
		return
	}
	allowed := map[string]bool{"mgmt_ip": true, "ssh_port": true, "telnet_port": true,
		"asset_tag": true, "notes": true, "kind_override": true}
	sets, args := []string{}, []any{}
	for k, v := range body {
		if !allowed[k] || v == nil {
			continue
		}
		if k == "kind_override" {
			s, _ := v.(string)
			if s != "" && s != "router" && s != "l3switch" && s != "switch" && s != "server" {
				fail(w, http.StatusBadRequest, "bad kind_override")
				return
			}
		}
		sets = append(sets, k+" = ?")
		args = append(args, v)
	}
	if len(sets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": 0})
		return
	}
	args = append(args, id)
	res, err := db.Exec("UPDATE devices SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		fail(w, http.StatusNotFound, "no such device")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": n})
}

func handleCredentials(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid request")
		return
	}
	key := netdbKey()
	if key == nil {
		fail(w, http.StatusServiceUnavailable, "NETDB_KEY not configured")
		return
	}
	blob, err := aesEncrypt(key, body.Password)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	res, err := db.Exec("UPDATE devices SET username = ?, password_enc = ? WHERE id = ?",
		body.Username, blob, id)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(w, http.StatusNotFound, "no such device")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleDeviceHealth(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "5m"
	}
	type row struct {
		TS      string   `json:"ts"`
		CPUPct  *float64 `json:"cpu_pct"`
		MemPct  *float64 `json:"mem_pct"`
		DiskPct *float64 `json:"disk_pct"`
		NetIn   *float64 `json:"net_in_bps"`
		NetOut  *float64 `json:"net_out_bps"`
	}
	collect := func(q string, args ...any) ([]row, error) {
		rs, err := db.Query(q, args...)
		if err != nil {
			return nil, err
		}
		defer rs.Close()
		out := []row{}
		for rs.Next() {
			var x row
			if err := rs.Scan(&x.TS, &x.CPUPct, &x.MemPct, &x.DiskPct, &x.NetIn, &x.NetOut); err == nil {
				out = append(out, x)
			}
		}
		return out, rs.Err()
	}

	if rng == "live" {
		rs, err := collect(`SELECT ts, cpu_pct, mem_pct, disk_pct, net_in_bps, net_out_bps
		                    FROM health_samples WHERE device_id = ?
		                    ORDER BY ts DESC LIMIT 60`, id)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
			rs[i], rs[j] = rs[j], rs[i]
		}
		writeJSON(w, http.StatusOK, rs)
		return
	}

	if rng == "all" {
		rs, err := collect(`SELECT ts, cpu_pct, mem_pct, disk_pct, net_in_bps, net_out_bps
		                    FROM health_samples WHERE device_id = ? ORDER BY ts`, id)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		step := len(rs) / 240
		if step < 1 {
			step = 1
		}
		out := []row{}
		for i := 0; i < len(rs); i += step {
			out = append(out, rs[i])
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	minutes := map[string]int{"5m": 5, "15m": 15, "30m": 30, "60m": 60, "6h": 360, "12h": 720, "24h": 1440}
	m, ok := minutes[rng]
	if !ok {
		fail(w, http.StatusBadRequest, "bad range")
		return
	}
	rs, err := collect(`SELECT strftime('%Y-%m-%d %H:%M', ts) AS b,
	                           ROUND(AVG(cpu_pct),1), ROUND(AVG(mem_pct),1), ROUND(AVG(disk_pct),1),
	                           ROUND(AVG(net_in_bps),0), ROUND(AVG(net_out_bps),0)
	                    FROM health_samples
	                    WHERE device_id = ? AND ts >= datetime('now', ?)
	                    GROUP BY b ORDER BY b`, id, fmt.Sprintf("-%d minutes", m))
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

func handleEvents(w http.ResponseWriter, r *http.Request, u *User) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	sev := r.URL.Query().Get("severity")
	q := `SELECT e.ts, e.severity, e.message, COALESCE(d.hostname,'')
	      FROM events e LEFT JOIN devices d ON d.id = e.device_id`
	args := []any{}
	if sev != "" {
		q += " WHERE e.severity = ?"
		args = append(args, sev)
	}
	q += " ORDER BY e.ts DESC LIMIT ?"
	args = append(args, limit)

	rs, err := db.Query(q, args...)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rs.Close()
	out := []map[string]any{}
	for rs.Next() {
		var ts, severity, msg, host string
		if rs.Scan(&ts, &severity, &msg, &host) == nil {
			out = append(out, map[string]any{"ts": ts, "severity": severity, "message": msg, "hostname": host})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func handleTopology(w http.ResponseWriter, r *http.Request, u *User) {
	nodes := []map[string]any{}
	nr, err := db.Query(`SELECT id, hostname, mgmt_ip, effective_kind, last_state FROM v_inventory`)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	for nr.Next() {
		var id int64
		var host, ip, kind string
		var state sql.NullString
		if nr.Scan(&id, &host, &ip, &kind, &state) == nil {
			nodes = append(nodes, map[string]any{"id": id, "hostname": host, "mgmt_ip": ip,
				"effective_kind": kind, "last_state": state.String})
		}
	}
	nr.Close()

	links := []map[string]any{}
	lr, err := db.Query(`SELECT a_device_id, b_device_id, if_a, if_b, proto FROM links`)
	if err == nil {
		for lr.Next() {
			var a, b int64
			var ia, ib, proto sql.NullString
			if lr.Scan(&a, &b, &ia, &ib, &proto) == nil {
				links = append(links, map[string]any{"a_device_id": a, "b_device_id": b,
					"if_a": ia.String, "if_b": ib.String, "proto": proto.String})
			}
		}
		lr.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "links": links})
}

func handleGetSettings(w http.ResponseWriter, r *http.Request, u *User) {
	out := map[string]string{}
	rs, err := db.Query("SELECT key, value FROM settings")
	if err == nil {
		defer rs.Close()
		for rs.Next() {
			var k, v string
			if rs.Scan(&k, &v) == nil {
				out[k] = v
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func handlePutSettings(w http.ResponseWriter, r *http.Request, u *User) {
	var body map[string]string
	if err := decode(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid request")
		return
	}
	allow := map[string]map[string]bool{
		"theme":  {"light": true, "dark": true},
		"layout": {"detailed": true, "simple": true},
	}
	for k, v := range body {
		ok, exists := allow[k]
		if !exists || !ok[v] {
			fail(w, http.StatusBadRequest, "bad setting: "+k)
			return
		}
	}
	for k, v := range body {
		if _, err := db.Exec(`INSERT INTO settings(key, value) VALUES(?,?)
		                      ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleChangePassword(w http.ResponseWriter, r *http.Request, u *User) {
	var body struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := decode(r, &body); err != nil || len(body.New) < 8 {
		fail(w, http.StatusBadRequest, "new password needs 8+ characters")
		return
	}
	var hash string
	if err := db.QueryRow("SELECT pw_hash FROM users WHERE id = ?", u.ID).Scan(&hash); err != nil {
		fail(w, http.StatusNotFound, "no such user")
		return
	}
	if !verifyPassword(body.Old, hash) {
		fail(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	newHash, err := hashPassword(body.New)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := db.Exec("UPDATE users SET pw_hash = ? WHERE id = ?", newHash, u.ID); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,32}$`)

func handleListUsers(w http.ResponseWriter, r *http.Request, u *User) {
	rs, err := db.Query("SELECT id, username, role, created_at FROM users ORDER BY id")
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rs.Close()
	out := []map[string]any{}
	for rs.Next() {
		var id int64
		var name, role string
		var created sql.NullString
		if rs.Scan(&id, &name, &role, &created) == nil {
			out = append(out, map[string]any{"id": id, "username": name, "role": role, "created_at": created.String})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func handleAddUser(w http.ResponseWriter, r *http.Request, u *User) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid request")
		return
	}
	if !usernameRe.MatchString(body.Username) {
		fail(w, http.StatusBadRequest, "username must be 3-32 chars: letters, digits, . _ -")
		return
	}
	if len(body.Password) < 8 {
		fail(w, http.StatusBadRequest, "password needs 8+ characters")
		return
	}
	if body.Role != "viewer" && body.Role != "admin" {
		fail(w, http.StatusBadRequest, "bad role")
		return
	}
	hash, err := hashPassword(body.Password)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := db.Exec("INSERT INTO users(username, pw_hash, role) VALUES(?,?,?)",
		body.Username, hash, body.Role); err != nil {
		fail(w, http.StatusConflict, "username already exists")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleSetRole(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Role string `json:"role"`
	}
	if err := decode(r, &body); err != nil || (body.Role != "viewer" && body.Role != "admin") {
		fail(w, http.StatusBadRequest, "bad role")
		return
	}
	res, err := db.Exec("UPDATE users SET role = ? WHERE id = ?", body.Role, id)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(w, http.StatusNotFound, "no such user")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleDeleteUser(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if id == u.ID {
		fail(w, http.StatusBadRequest, "cannot delete the account you are using")
		return
	}
	res, err := db.Exec("DELETE FROM users WHERE id = ?", id)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(w, http.StatusNotFound, "no such user")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

var allowedCmds = []string{
	"show ip ospf neighbor", "show ip route summary", "show ip route",
	"show version", "show interfaces", "show run", "show memory",
	"uptime", "free -m", "df -h", "ip -br a", "cat /etc/frr/frr.conf",
}

func cmdAllowed(c string) bool {
	for _, a := range allowedCmds {
		if c == a || strings.HasPrefix(c, a+" ") {
			return true
		}
	}
	return false
}

func handleTerminal(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Cmd string `json:"cmd"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid request")
		return
	}
	cmd := strings.TrimSpace(body.Cmd)
	if cmd == "" || len(cmd) > 200 || !cmdAllowed(cmd) {
		fail(w, http.StatusForbidden, "command not in allowlist")
		return
	}

	var host, mgmtIP string
	user := "root"
	port := 22
	var sshPort sql.NullInt64
	var uname sql.NullString
	var blob []byte
	if err := db.QueryRow(`SELECT hostname, mgmt_ip, username, ssh_port, password_enc
	                       FROM devices WHERE id = ?`, id).
		Scan(&host, &mgmtIP, &uname, &sshPort, &blob); err != nil {
		fail(w, http.StatusNotFound, "no such device")
		return
	}
	if uname.Valid {
		user = uname.String
	}
	if sshPort.Valid {
		port = int(sshPort.Int64)
	}
	if port == 0 {
		port = 22
	}

	key := netdbKey()
	if key == nil || len(blob) == 0 {
		fail(w, http.StatusServiceUnavailable, "no stored credential for this device")
		return
	}
	password, err := aesDecrypt(key, blob)
	if err != nil {
		fail(w, http.StatusInternalServerError, "cannot decrypt credential")
		return
	}

	remote := cmd
	if strings.HasPrefix(cmd, "show") {
		remote = "vtysh -c '" + strings.ReplaceAll(cmd, "'", "") + "'"
	}

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         8 * time.Second,
	}
	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", mgmtIP, port), cfg)
	if err != nil {
		fail(w, http.StatusBadGateway, "ssh failed: "+err.Error())
		return
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		fail(w, http.StatusBadGateway, "ssh session failed: "+err.Error())
		return
	}
	defer sess.Close()
	out, _ := sess.CombinedOutput(remote)

	if len(out) > 20000 {
		out = out[len(out)-20000:]
	}
	logEvent(id, "info", truncate("terminal: "+cmd, 120))
	writeJSON(w, http.StatusOK, map[string]string{"host": host, "cmd": cmd, "output": string(out)})
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}


// ---------------------------------------------------------------- logs

// sigOf identifies an event class: same device + severity + message means the
// same kind of thing happening again, so a decision taken once applies later.
func sigOf(deviceID int64, severity, message string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s", deviceID, severity, message)))
	return hex.EncodeToString(h[:])[:16]
}

func ensureLogSchema() {
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS log_decisions(
		sig      TEXT PRIMARY KEY,
		action   TEXT NOT NULL CHECK (action IN ('block','ignore','allow')),
		by_user  TEXT,
		at       TEXT NOT NULL DEFAULT (datetime('now')))`)
	// additive columns; errors are "duplicate column", which is the steady state
	_, _ = db.Exec("ALTER TABLE events ADD COLUMN sig TEXT")
	_, _ = db.Exec("ALTER TABLE events ADD COLUMN action TEXT")
	_, _ = db.Exec("ALTER TABLE events ADD COLUMN acked INTEGER NOT NULL DEFAULT 0")
	_, _ = db.Exec("CREATE INDEX IF NOT EXISTS ix_events_sig ON events(sig)")
	// backfill signatures for rows written before this feature existed
	rows, err := db.Query("SELECT id, COALESCE(device_id,0), severity, message FROM events WHERE sig IS NULL")
	if err != nil {
		return
	}
	type pending struct {
		id  int64
		sig string
	}
	var todo []pending
	for rows.Next() {
		var id, dev int64
		var sev, msg string
		if rows.Scan(&id, &dev, &sev, &msg) == nil {
			todo = append(todo, pending{id, sigOf(dev, sev, msg)})
		}
	}
	rows.Close()
	for _, t := range todo {
		_, _ = db.Exec("UPDATE events SET sig = ? WHERE id = ?", t.sig, t.id)
	}
}

func isBlocked(sig string) bool {
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM log_decisions WHERE sig = ? AND action = 'block'", sig).Scan(&n)
	return n > 0
}

// logEvent writes an event unless that class has been blocked.
func logEvent(deviceID int64, severity, message string) {
	sig := sigOf(deviceID, severity, message)
	if isBlocked(sig) {
		return
	}
	_, _ = db.Exec("INSERT INTO events(device_id, severity, message, sig) VALUES (?,?,?,?)",
		deviceID, severity, message, sig)
}

func levelOf(sev string) string {
	switch sev {
	case "info":
		return "info"
	case "warning":
		return "warning"
	default:
		return "critical"
	}
}

func handleLogs(w http.ResponseWriter, r *http.Request, u *User) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per <= 0 || per > 100 {
		per = 10
	}

	where := []string{"1=1"}
	args := []any{}
	switch q.Get("level") {
	case "info":
		where = append(where, "e.severity = 'info'")
	case "warning":
		where = append(where, "e.severity = 'warning'")
	case "critical":
		where = append(where, "e.severity IN ('average','high','disaster')")
	}
	if search := strings.TrimSpace(q.Get("q")); search != "" {
		where = append(where, "(e.message LIKE ? OR d.hostname LIKE ?)")
		args = append(args, "%"+search+"%", "%"+search+"%")
	}
	if q.Get("include_ignored") != "1" {
		where = append(where, "(e.action IS NULL OR e.action <> 'ignore')")
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events e
	                       LEFT JOIN devices d ON d.id = e.device_id WHERE `+cond, args...).Scan(&total); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	pages := (total + per - 1) / per
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}

	rows, err := db.Query(`SELECT e.id, e.ts, e.severity, e.message,
	                              COALESCE(d.hostname,''), COALESCE(e.sig,''), COALESCE(e.action,'')
	                       FROM events e LEFT JOIN devices d ON d.id = e.device_id
	                       WHERE `+cond+` ORDER BY e.ts DESC, e.id DESC LIMIT ? OFFSET ?`,
		append(args, per, (page-1)*per)...)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id int64
		var ts, sev, msg, host, sig, action string
		if rows.Scan(&id, &ts, &sev, &msg, &host, &sig, &action) == nil {
			items = append(items, map[string]any{
				"id": id, "ts": ts, "severity": sev, "level": levelOf(sev),
				"message": msg, "device": host, "sig": sig, "action": action,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "page": page, "pages": pages, "per_page": per,
	})
}

func handleLogAction(w http.ResponseWriter, r *http.Request, u *User) {
	var body struct {
		Sig    string `json:"sig"`
		Action string `json:"action"`
	}
	if err := decode(r, &body); err != nil || len(body.Sig) < 8 {
		fail(w, http.StatusBadRequest, "sig required")
		return
	}
	switch body.Action {
	case "block", "ignore", "allow":
		_, err := db.Exec(`INSERT INTO log_decisions(sig, action, by_user) VALUES (?,?,?)
		                   ON CONFLICT(sig) DO UPDATE SET action = excluded.action,
		                                                 by_user = excluded.by_user,
		                                                 at = datetime('now')`,
			body.Sig, body.Action, u.Username)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		_, _ = db.Exec("UPDATE events SET action = ? WHERE sig = ?", body.Action, body.Sig)
	case "none":
		_, _ = db.Exec("DELETE FROM log_decisions WHERE sig = ?", body.Sig)
		_, _ = db.Exec("UPDATE events SET action = NULL WHERE sig = ?", body.Sig)
	default:
		fail(w, http.StatusBadRequest, "action must be block, ignore, allow or none")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sig": body.Sig, "action": body.Action})
}

// ---------------------------------------------------------------- bootstrap

func bootstrap() {
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n); err != nil {
		log.Fatal("bootstrap: ", err)
	}
	if n > 0 {
		return
	}
	user := env("ADMIN_USER", "admin")
	pass := os.Getenv("ADMIN_PASSWORD")
	if pass == "" {
		pass = randomToken(12)
	}
	hash, err := hashPassword(pass)
	if err != nil {
		log.Fatal("bootstrap hash: ", err)
	}
	if _, err := db.Exec("INSERT INTO users(username, pw_hash, role) VALUES(?,?,'admin')", user, hash); err != nil {
		log.Fatal("bootstrap insert: ", err)
	}
	_, _ = db.Exec("INSERT OR IGNORE INTO settings(key, value) VALUES('theme','light'),('layout','detailed')")
	log.Printf("BOOTSTRAP admin=%s password=%s", user, pass)
}

// ---------------------------------------------------------------- main

func main() {
	if len(secret) < 32 {
		log.Fatal("JWT_SECRET must be at least 32 chars")
	}
	for i := 0; i < 30; i++ {
		if _, err := os.Stat(dbPath); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err := openDB(); err != nil {
		log.Fatal("db: ", err)
	}
	defer db.Close()
	ensureLogSchema()
	ensureLabSchema()
	bootstrap()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("POST /api/login", handleLogin)
	mux.HandleFunc("POST /api/logout", withUser(handleLogout))
	mux.HandleFunc("GET /api/me", withUser(handleMe))
	mux.HandleFunc("GET /api/summary", withUser(handleSummary))
	mux.HandleFunc("GET /api/devices", withUser(handleDevices))
	mux.HandleFunc("PATCH /api/devices/{id}", withAdmin(handlePatchDevice))
	mux.HandleFunc("PUT /api/devices/{id}/credentials", withAdmin(handleCredentials))
	mux.HandleFunc("GET /api/devices/{id}/health", withUser(handleDeviceHealth))
	mux.HandleFunc("GET /api/events", withUser(handleEvents))
	mux.HandleFunc("GET /api/topology", withUser(handleTopology))
	mux.HandleFunc("GET /api/settings", withUser(handleGetSettings))
	mux.HandleFunc("PUT /api/settings", withAdmin(handlePutSettings))
	mux.HandleFunc("POST /api/account/password", withUser(handleChangePassword))
	mux.HandleFunc("GET /api/users", withAdmin(handleListUsers))
	mux.HandleFunc("POST /api/users", withAdmin(handleAddUser))
	mux.HandleFunc("PATCH /api/users/{id}", withAdmin(handleSetRole))
	mux.HandleFunc("DELETE /api/users/{id}", withAdmin(handleDeleteUser))
	mux.HandleFunc("POST /api/terminal/{id}/exec", withAdmin(handleTerminal))
	mux.HandleFunc("GET /api/logs", withUser(handleLogs))
	mux.HandleFunc("POST /api/logs/action", withAdmin(handleLogAction))

	// ---- operations layer (lab.go)
	mux.HandleFunc("GET /api/leases", withUser(handleLeases))
	mux.HandleFunc("PATCH /api/leases/{id}", withAdmin(handlePatchLease))
	mux.HandleFunc("GET /api/lease-events", withUser(handleLeaseEvents))
	mux.HandleFunc("GET /api/reboots", withUser(handleReboots))
	mux.HandleFunc("GET /api/reachability", withUser(handleReachability))
	mux.HandleFunc("GET /api/poll", withUser(handlePollInfo))

	mux.HandleFunc("GET /api/shifts", withUser(handleShifts))
	mux.HandleFunc("PUT /api/shifts", withAdmin(handlePutShift))
	mux.HandleFunc("DELETE /api/shifts/{id}", withAdmin(handleDeleteShift))
	mux.HandleFunc("GET /api/handovers", withUser(handleHandovers))
	mux.HandleFunc("POST /api/handover", withAdmin(handleHandover))

	mux.HandleFunc("GET /api/alerts", withUser(handleAlerts))
	mux.HandleFunc("PATCH /api/alerts/rules/{id}", withAdmin(handlePutAlertRule))
	mux.HandleFunc("POST /api/alerts/test", withAdmin(handleTestAlert))
	mux.HandleFunc("POST /api/alerts/redispatch", withAdmin(handleRedispatch))

	mux.HandleFunc("GET /api/logs/analysis", withUser(handleLogAnalysis))
	mux.HandleFunc("POST /api/logs/analysis/{id}/ack", withAdmin(handleAckFinding))
	mux.HandleFunc("POST /api/events/{id}/ack", withAdmin(handleAckEvent))

	mux.HandleFunc("POST /api/devices", withAdmin(handleAddDevice))
	mux.HandleFunc("DELETE /api/devices/{id}", withAdmin(handleDeleteDevice))
	mux.HandleFunc("POST /api/topology/discover", withAdmin(handleDiscover))
	mux.HandleFunc("GET /api/topology/proposals", withUser(handleProposals))
	mux.HandleFunc("POST /api/topology/proposals/{id}", withAdmin(handleDecideProposal))

	mux.HandleFunc("GET /api/dns", withUser(handleDNS))
	mux.HandleFunc("PUT /api/dns", withAdmin(handlePutDNS))
	mux.HandleFunc("DELETE /api/dns/{id}", withAdmin(handleDeleteDNS))

	noStore := func(p string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store, max-age=0")
			http.ServeFile(w, r, filepath.Join(staticDir, p))
		}
	}
	mux.HandleFunc("GET /{$}", noStore("index.html"))
	mux.HandleFunc("GET /app", noStore("app.html"))
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir))))

	srv := &http.Server{
		Addr:              ":8000",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	// optional: the same records that back the UI can be served as a real
	// zone, so http://lab.local:8080 works without /etc/hosts
	startDNS()

	log.Println("listening on :8000")
	log.Fatal(srv.ListenAndServe())
}
