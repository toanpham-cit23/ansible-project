package main

// Interactive SSH terminal for the Database view.
//
// The older /api/terminal/{id}/exec endpoint runs ONE allowlisted command and
// returns its output. That is enough for "show ip route", but it cannot back a
// real terminal: no prompt, no line editing, no interactive tool. A demo needs
// the real thing, so this file opens a genuine PTY over SSH and pumps it
// through a WebSocket.
//
// Security notes, because this is the most dangerous endpoint in the app:
//   - withAdmin only. A viewer must never get a shell.
//   - the credential is decrypted server-side and never sent to the browser.
//   - the shell runs on the target device, not on the controller.
//   - the WebSocket carries no credential; the session cookie is the only auth,
//     and coder/websocket's default same-origin check applies (no wildcard).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// resizeMsg is the only client->server control frame; anything else is stdin.
type resizeMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func handleSSHSession(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad device id")
		return
	}

	var (
		host     string
		mgmtIP   string
		username sql.NullString
		sshPort  sql.NullInt64
		blob     []byte
	)
	if err := db.QueryRow(`SELECT hostname, mgmt_ip, username, ssh_port, password_enc
	                       FROM devices WHERE id = ?`, id).
		Scan(&host, &mgmtIP, &username, &sshPort, &blob); err != nil {
		fail(w, http.StatusNotFound, "no such device")
		return
	}

	user := "root"
	if username.Valid && username.String != "" {
		user = username.String
	}
	port := 22
	if sshPort.Valid && sshPort.Int64 > 0 {
		port = int(sshPort.Int64)
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

	// Probe TCP/22 first. ssh.Dial would sit for its full timeout on a closed
	// port, and the browser would show a spinner with no explanation. A
	// one-second probe turns that into "port 22 closed on 192.168.23.51".
	probe, perr := net.DialTimeout("tcp", net.JoinHostPort(mgmtIP, strconv.Itoa(port)), 2*time.Second)
	if perr != nil {
		fail(w, http.StatusBadGateway,
			fmt.Sprintf("%s does not answer on port %d (%s) - no SSH daemon there, or the network blocks it",
				mgmtIP, port, perr))
		return
	}
	_ = probe.Close()

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         8 * time.Second,
	}
	client, err := ssh.Dial("tcp", net.JoinHostPort(mgmtIP, strconv.Itoa(port)), cfg)
	if err != nil {
		fail(w, http.StatusBadGateway, "ssh login failed: "+err.Error())
		return
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		fail(w, http.StatusBadGateway, "cannot open session: "+err.Error())
		return
	}
	defer session.Close()

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 38400,
		ssh.TTY_OP_OSPEED: 38400,
	}
	// 30x100 is only the initial size; the browser sends a resize frame as soon
	// as xterm knows the real dimensions.
	if err := session.RequestPty("xterm-256color", 30, 100, modes); err != nil {
		fail(w, http.StatusBadGateway, "pty request failed: "+err.Error())
		return
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		fail(w, http.StatusInternalServerError, "stdin: "+err.Error())
		return
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		fail(w, http.StatusInternalServerError, "stdout: "+err.Error())
		return
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		fail(w, http.StatusInternalServerError, "stderr: "+err.Error())
		return
	}

	if err := session.Shell(); err != nil {
		fail(w, http.StatusBadGateway, "shell failed: "+err.Error())
		return
	}

	// No OriginPatterns: coder/websocket then enforces Origin == Host, which is
	// what stops another site from opening a shell inside the user's session.
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close(websocket.StatusNormalClosure, "")
	ws.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	logEvent(id, "info", "terminal: interactive SSH session opened by "+u.Username+" on "+host)

	var writeMu sync.Mutex
	write := func(b []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.Write(ctx, websocket.MessageBinary, b)
	}

	var wg sync.WaitGroup

	// device -> browser, one goroutine per stream so a stalled stderr cannot
	// starve stdout.
	pump := func(src interface{ Read([]byte) (int, error) }) {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, rerr := src.Read(buf)
			if n > 0 {
				if werr := write(buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(stdout)
	go pump(stderr)

	// browser -> device
	for {
		typ, data, rerr := ws.Read(ctx)
		if rerr != nil {
			break
		}
		if typ != websocket.MessageBinary && typ != websocket.MessageText {
			continue
		}
		// A leading '{' is our resize control frame. Terminal input starting
		// with '{' is possible but almost never valid JSON with type=resize, so
		// a stray brace still reaches the shell.
		if len(data) > 0 && data[0] == '{' {
			var m resizeMsg
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols > 0 && m.Rows > 0 {
				if m.Cols > 500 {
					m.Cols = 500
				}
				if m.Rows > 300 {
					m.Rows = 300
				}
				_ = session.WindowChange(m.Rows, m.Cols)
				continue
			}
		}
		if _, werr := stdin.Write(data); werr != nil {
			break
		}
	}

	cancel()
	_ = session.Close()
	wg.Wait()
	logEvent(id, "info", "terminal: interactive SSH session closed on "+host)
}

// handleDevicePassword returns the plaintext credential for one device.
//
// This exists only so the Database view can reveal a password on demand for a
// demo. Admin-only, logged on every call, and the value is sent only after the
// operator clicks. A viewer can never reach it.
func handleDevicePassword(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad device id")
		return
	}
	var (
		host  string
		uname sql.NullString
		blob  []byte
	)
	if err := db.QueryRow(`SELECT hostname, username, password_enc FROM devices WHERE id = ?`, id).
		Scan(&host, &uname, &blob); err != nil {
		fail(w, http.StatusNotFound, "no such device")
		return
	}
	key := netdbKey()
	if key == nil || len(blob) == 0 {
		fail(w, http.StatusServiceUnavailable, "no stored credential")
		return
	}
	pw, err := aesDecrypt(key, blob)
	if err != nil {
		fail(w, http.StatusInternalServerError, "cannot decrypt credential")
		return
	}
	logEvent(id, "info", "credential revealed to "+u.Username+" for "+host)
	writeJSON(w, http.StatusOK, map[string]any{
		"hostname": host,
		"username": uname.String,
		"password": pw,
	})
}

// keep the log import honest even if nothing else uses it
var _ = log.Flags
