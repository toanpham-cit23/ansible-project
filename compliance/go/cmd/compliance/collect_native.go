package main

// Native configuration collection over SSH.
//
// The Python runner depended on ansible-playbook being installed. The Go binary
// has no such need: it can open the SSH session itself, run
//
//	TERM=dumb vtysh -c 'show running-config'
//
// and write the same per-router .conf files. That removes Ansible, PyYAML and
// the Python virtualenv from the Controller's requirements, which is the main
// practical reason to port this component at all.
//
// Collection is still read-only. No command in this file changes router state.

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// inventoryHost is one parsed entry from an Ansible-style INI inventory.
type inventoryHost struct {
	Name     string
	Address  string
	User     string
	Password string
	KeyFile  string
	Port     int
	Vars     map[string]string
}

// parseInventory reads the subset of the Ansible INI format this project uses:
// a [routers] section with `name key=value` lines, plus a [routers:vars]
// section whose values apply to every host.
//
// This is deliberately not a general INI parser. Ansible's own parser has years
// of edge cases; reproducing them would be more code than the audit itself, and
// the inventory in this project is a flat list of three routers.
func parseInventory(path string) ([]inventoryHost, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("không mở được inventory: %w", err)
	}
	defer f.Close()

	var hosts []inventoryHost
	section := ""
	common := map[string]string{}
	type rawHost struct {
		name string
		vars map[string]string
	}
	var raws []rawHost

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			continue
		}
		if strings.HasSuffix(section, ":vars") {
			// group:vars section: name=value lines
			for k, v := range parseKV(line) {
				common[k] = v
			}
			continue
		}
		if section != "routers" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		raws = append(raws, rawHost{name: fields[0], vars: parseKV(strings.Join(fields[1:], " "))})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	for _, r := range raws {
		v := map[string]string{}
		for k, val := range common {
			v[k] = val
		}
		for k, val := range r.vars {
			v[k] = val
		}
		h := inventoryHost{
			Name:     r.name,
			Address:  v["ansible_host"],
			User:     v["ansible_user"],
			Password: v["ansible_password"],
			KeyFile:  v["ansible_ssh_private_key_file"],
			Vars:     v,
		}
		if h.Address == "" {
			h.Address = r.name
		}
		if h.User == "" {
			h.User = "root"
		}
		if p, err := strconv.Atoi(v["ansible_port"]); err == nil && p > 0 {
			h.Port = p
		}
		if h.Port == 0 {
			h.Port = 22
		}
		hosts = append(hosts, h)
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("không tìm thấy router nào trong section [routers] của %s", path)
	}
	return hosts, nil
}

// parseKV splits "a=1 b=2" into a map. It tolerates values containing '='
// by splitting on the first one only, which matters for base64-ish passwords.
func parseKV(s string) map[string]string {
	out := map[string]string{}
	for _, field := range strings.Fields(s) {
		if i := strings.Index(field, "="); i > 0 {
			out[field[:i]] = field[i+1:]
		}
	}
	return out
}

// authMethods builds the SSH auth list, preferring a key over a password.
//
// Note the change from the Python flow: because this is a direct SSH dial it
// never needs sshpass, and a key file works without extra packages. The
// inventory may still carry ansible_password, and it is used only when no key
// is configured.
func authMethods(h inventoryHost) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if h.KeyFile != "" {
		keyPath := h.KeyFile
		if strings.HasPrefix(keyPath, "~/") {
			home, _ := os.UserHomeDir()
			keyPath = filepath.Join(home, keyPath[2:])
		}
		raw, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("%s: không đọc được SSH key %s: %w", h.Name, keyPath, err)
		}
		signer, err := ssh.ParsePrivateKey(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: SSH key không hợp lệ: %w", h.Name, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if h.Password != "" {
		methods = append(methods, ssh.Password(h.Password))
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("%s: không có phương thức xác thực (cần ansible_ssh_private_key_file hoặc ansible_password)", h.Name)
	}
	return methods, nil
}

// collectNative gathers every router's running config into actualDir.
//
// It returns the same (exit code, message) pair as collectConfigs so run() can
// treat both collectors identically, plus an error for a problem that should
// abort the run outright (unreadable inventory) rather than be scored as a
// per-router collection failure.
//
// The exit code is the number of failed routers capped at 1, matching the
// Ansible contract where any failure is "non-zero". A router that fails is
// simply absent from actualDir, which the audit step then scores ERROR.
// Writing a partial or stale file here would be worse than writing nothing: it
// would grade the router against configuration it may no longer be running.
func collectNative(actualDir string, opts options) (int, string, error) {
	hosts, err := parseInventory(opts.inventory)
	if err != nil {
		return 0, "", err
	}

	if err := os.MkdirAll(actualDir, 0o700); err != nil {
		return 0, "", fmt.Errorf("không tạo được thư mục report %s: %w", actualDir, err)
	}

	// Same reason as the Ansible path: drop last run's snapshots first so an
	// unreachable router is graded ERROR instead of against a stale file.
	old, _ := filepath.Glob(filepath.Join(actualDir, "*.conf"))
	for _, f := range old {
		_ = os.Remove(f)
	}

	fmt.Printf("[1/3] Thu thập cấu hình từ %d router qua SSH (native)...\n", len(hosts))
	failures := 0
	for _, h := range hosts {
		cfg, err := fetchRunningConfig(h, opts.timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %-6s ERROR: %v\n", h.Name, err)
			failures++
			continue
		}
		dest := filepath.Join(actualDir, h.Name+".conf")
		// 0600: a running-config can contain credentials (SNMP communities,
		// keys). The Python playbook also wrote these 0600.
		if err := os.WriteFile(dest, []byte(cfg+"\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "  %-6s ERROR: không ghi được %s: %v\n", h.Name, dest, err)
			failures++
			continue
		}
		if opts.verbose {
			fmt.Printf("  %-6s ok (%d bytes)\n", h.Name, len(cfg))
		} else {
			fmt.Printf("  %-6s ok\n", h.Name)
		}
	}

	if failures > 0 {
		return 1, fmt.Sprintf("SSH native exit code: 1 (%d/%d router lỗi)", failures, len(hosts)), nil
	}
	return 0, "SSH native exit code: 0", nil
}

// fetchRunningConfig opens one SSH session and runs the FRR show command.
func fetchRunningConfig(h inventoryHost, timeout time.Duration) (string, error) {
	auth, err := authMethods(h)
	if err != nil {
		return "", err
	}
	cfg := &ssh.ClientConfig{
		User: h.User,
		Auth: auth,
		// The lab regenerates host keys on every rebuild, so pinning them would
		// break the documented rebuild-then-audit workflow. This is the same
		// trade-off the existing Ansible config makes with
		// host_key_checking = False.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         timeout,
	}

	addr := net.JoinHostPort(h.Address, strconv.Itoa(h.Port))
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return "", fmt.Errorf("ssh %s: %w", addr, err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	// TERM=dumb keeps vtysh from emitting pagination control characters.
	out, err := sess.CombinedOutput(`TERM=dumb vtysh -c 'show running-config'`)
	if err != nil {
		return "", fmt.Errorf("vtysh: %w", err)
	}
	text := strings.TrimRight(string(out), "\r\n\t ")
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("vtysh trả về rỗng (kiểm tra quyền chạy vtysh)")
	}
	return strings.ReplaceAll(text, "\r\n", "\n"), nil
}
