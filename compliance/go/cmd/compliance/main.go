// Command compliance performs a read-only FRRouting configuration compliance
// audit for the lab routers and records each run in the dashboard's SQLite
// database.
//
// This is a Go port of compliance/scripts/run_compliance.py. Behaviour is
// intended to be identical; where the two diverge for platform reasons the
// difference is documented at the point it occurs (see matchPattern and
// loadDesiredState).
//
// The audit only ever reads configuration. It never writes to a router, never
// calls `write memory`, and never remediates anything.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Exit codes are part of the contract documented in the README:
//
//	0 = every device compliant and collection succeeded
//	1 = non-compliant device(s) and/or collection failure
//	2 = bad input, bad desired-state, or database failure
const (
	exitOK       = 0
	exitFindings = 1
	exitInput    = 2
)

// Device status values. These are stored in SQLite and rendered by the
// dashboard, so they are a wire format and must not be renamed casually.
const (
	statusCompliant    = "COMPLIANT"
	statusNonCompliant = "NON_COMPLIANT"
	statusError        = "ERROR"
)

// Finding and Device mirror the Python dicts exactly, including the JSON key
// names, because compliance/reports/latest.json is consumed by the dashboard.
type Finding struct {
	Rule   string `json:"rule"`
	Detail string `json:"detail"`
}

type Device struct {
	Hostname string    `json:"hostname"`
	Status   string    `json:"status"`
	Findings []Finding `json:"findings"`
}

// Summary and Report reproduce the JSON document written to
// compliance/reports/latest.json.
type Summary struct {
	Total        int `json:"total"`
	Compliant    int `json:"compliant"`
	NonCompliant int `json:"noncompliant"`
	Errors       int `json:"errors"`
}

type Report struct {
	RunID              int64    `json:"run_id"`
	CheckedAt          string   `json:"checked_at"`
	Status             string   `json:"status"`
	CollectionExitCode int      `json:"collection_exit_code"`
	Summary            Summary  `json:"summary"`
	Devices            []Device `json:"devices"`
}

// Rule is one required pattern together with a compiled form. Compiling at
// load time means a bad regex fails the run before any router is touched,
// which matches the Python behaviour of validating in load_desired_state.
type Rule struct {
	Pattern string
	re      *regexp.Regexp
}

// RouterSpec is the desired state for a single router.
type RouterSpec struct {
	Rules []Rule
}

// DesiredState preserves the file's router order. Python iterates a dict, which
// preserves insertion order from Python 3.7; a Go map would randomise it and
// make the findings table and JSON output non-deterministic. That matters
// because the dashboard lists findings by hostname and the tests assert order.
type DesiredState struct {
	Order   []string
	Routers map[string]RouterSpec
}

// detail strings are user-facing and come straight from the Python original.
// They are kept in Vietnamese deliberately: changing them here would change
// what operators see in the dashboard for no functional gain.
const (
	detailNoSnapshot = "Không có file cấu hình mới. Kiểm tra SSH, inventory, vtysh và quyền truy cập."
	detailMissing    = "Không tìm thấy cấu hình bắt buộc trong running-config."
	ruleCollection   = "Configuration collection"
	ruleAllOK        = "All required rules"
	detailAllOK      = "Tất cả cấu hình bắt buộc đều được tìm thấy."
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return exitInput
	}

	paths := resolvePaths()

	desired, err := loadDesiredState(filepath.Join(paths.addon, "config", "desired_state.yml"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return exitInput
	}

	if !opts.skipCollect && !fileExists(opts.inventory) {
		fmt.Fprintf(os.Stderr, "ERROR: Inventory không tồn tại: %s\n", opts.inventory)
		return exitInput
	}

	var collectRC int
	var collectMsg string
	if opts.native {
		collectRC, collectMsg, err = collectNative(paths.actualDir, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			return exitInput
		}
	} else {
		collectRC, collectMsg = collectConfigs(paths.actualDir, opts)
	}
	fmt.Printf("[2/3] %s\n", collectMsg)
	devices, err := auditDevices(desired, paths.actualDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return exitInput
	}

	checkedAt := time.Now().UTC().Format("2006-01-02T15:04:05-07:00")
	runID, err := saveResults(opts.db, devices, checkedAt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Không lưu được lịch sử SQLite: %v\n", err)
		return exitInput
	}

	sum := summarise(devices)
	overall := overallStatus(sum)
	report := Report{
		RunID:              runID,
		CheckedAt:          checkedAt,
		Status:             overall,
		CollectionExitCode: collectRC,
		Summary:            sum,
		Devices:            devices,
	}

	reportPath := filepath.Join(paths.addon, "reports", "latest.json")
	if err := writeReport(reportPath, report); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Không ghi được báo cáo JSON: %v\n", err)
		return exitInput
	}

	fmt.Println("[3/3] Kết quả Compliance:")
	out, err := marshalIndentNoEscape(report)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return exitInput
	}
	fmt.Println(string(out))
	fmt.Printf("SQLite: %s\n", opts.db)
	fmt.Printf("JSON:   %s\n", reportPath)
	if collectRC != 0 {
		fmt.Fprintln(os.Stderr, "WARNING: Ansible có lỗi; các router không thu thập được được tính là ERROR.")
	}

	if overall == statusCompliant && collectRC == 0 {
		return exitOK
	}
	return exitFindings
}

// ---------------------------------------------------------------- paths

type pathSet struct {
	root      string
	addon     string
	actualDir string
	defDB     string
}

// resolvePaths reproduces the Python module's ROOT/ADDON/DEFAULT_DB layout.
//
// Python derives ROOT from __file__ (compliance/scripts/run_compliance.py ->
// project root). A Go binary has no __file__, so this supports an explicit
// --root for service use and otherwise walks upward from the working directory
// looking for the `compliance` directory. That keeps the documented
// "run from the project root" workflow working unchanged.
func resolvePaths() pathSet {
	root := os.Getenv("COMPLIANCE_ROOT")
	if root == "" {
		wd, err := os.Getwd()
		if err == nil {
			if found, ok := findProjectRoot(wd); ok {
				root = found
			} else {
				root = wd
			}
		} else {
			root = "."
		}
	}
	root, _ = filepath.Abs(root)
	addon := filepath.Join(root, "compliance")
	return pathSet{
		root:      root,
		addon:     addon,
		actualDir: filepath.Join(addon, "reports", "actual"),
		defDB:     filepath.Join(root, "files", "dashboard", "db", "net.db"),
	}
}

// findProjectRoot walks up from dir looking for a directory that contains a
// `compliance` subdirectory, which is the marker for this project's root.
func findProjectRoot(dir string) (string, bool) {
	for {
		if st, err := os.Stat(filepath.Join(dir, "compliance")); err == nil && st.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// ---------------------------------------------------------------- args

type options struct {
	inventory   string
	db          string
	skipCollect bool
	native      bool
	timeout     time.Duration
	verbose     bool
}

// parseArgs mirrors the Python argparse surface, plus the three flags the Go
// binary adds for its Ansible-free mode. Same flags, same defaults, same
// "unknown flag is an error" behaviour.
func parseArgs(args []string) (options, error) {
	opts := options{inventory: "hosts.ini", timeout: 20 * time.Second}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			usage()
			os.Exit(exitOK)
		case a == "--skip-collect":
			opts.skipCollect = true
		case a == "--native":
			opts.native = true
		case a == "--verbose" || a == "-v":
			opts.verbose = true
		case a == "--inventory" || a == "--db":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag needs an argument: %s", a)
			}
			i++
			if a == "--inventory" {
				opts.inventory = args[i]
			} else {
				opts.db = args[i]
			}
		case a == "--timeout":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag needs an argument: %s", a)
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return opts, fmt.Errorf("--timeout: %v", err)
			}
			opts.timeout = d
		case strings.HasPrefix(a, "--inventory="):
			opts.inventory = strings.TrimPrefix(a, "--inventory=")
		case strings.HasPrefix(a, "--db="):
			opts.db = strings.TrimPrefix(a, "--db=")
		case strings.HasPrefix(a, "--timeout="):
			d, err := time.ParseDuration(strings.TrimPrefix(a, "--timeout="))
			if err != nil {
				return opts, fmt.Errorf("--timeout: %v", err)
			}
			opts.timeout = d
		default:
			return opts, fmt.Errorf("unrecognized argument: %s", a)
		}
	}
	if opts.db == "" {
		opts.db = resolvePaths().defDB
	}
	// expanduser + resolve, as the Python version does for both paths.
	opts.inventory = expandHome(opts.inventory)
	if p, err := filepath.Abs(opts.inventory); err == nil {
		opts.inventory = p
	}
	opts.db = expandHome(opts.db)
	if p, err := filepath.Abs(opts.db); err == nil {
		opts.db = p
	}
	return opts, nil
}

// expandHome replaces a leading ~ or ~/ with the user's home directory, the way
// pathlib.Path.expanduser() does in the Python original.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		if len(p) <= 2 {
			return home
		}
		return filepath.Join(home, p[2:])
	}
	return p
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: compliance [--inventory PATH] [--db PATH] [--skip-collect]
                  [--native] [--timeout DUR] [--verbose]

Read-only FRR configuration compliance audit.

  --inventory PATH   Inventory file (default: hosts.ini)
  --db PATH          SQLite DB shared with the Dashboard
  --skip-collect     Audit existing .conf files only (testing)
  --native           Collect over SSH directly instead of via ansible-playbook
  --timeout DUR      SSH dial timeout for --native (default: 20s)
  --verbose, -v      Print snapshot size per router

Exit codes: 0 compliant, 1 findings or collection failure, 2 bad input.
`)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ---------------------------------------------------------------- desired state

// loadDesiredState reads and validates config/desired_state.yml.
//
// Parity note: PyYAML resolves mapping keys according to the YAML 1.1 core
// schema, so an unquoted `123:` becomes the integer 123 and the Python code
// rejects it with "Tên router không hợp lệ". yaml.v3 implements YAML 1.2 and
// decodes it as the string "123", which would be silently accepted as a
// hostname. To keep the two implementations in agreement this decodes into
// yaml.Node first and rejects any non-string router key explicitly.
func loadDesiredState(path string) (*DesiredState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("Không tìm thấy desired-state: %s", path)
		}
		return nil, fmt.Errorf("không đọc được %s: %w", path, err)
	}

	var doc yamlNode
	if err := unmarshalYAML(raw, &doc); err != nil {
		return nil, fmt.Errorf("YAML không hợp lệ trong %s: %v", path, err)
	}

	routersNode, err := documentField(&doc, "routers")
	if err != nil {
		return nil, err
	}
	if routersNode == nil || routersNode.Kind != mappingNode {
		return nil, fmt.Errorf("desired_state.yml phải có cấu trúc 'routers:' dạng mapping")
	}
	if len(routersNode.Content) == 0 {
		return nil, fmt.Errorf("Danh sách routers trong desired_state.yml đang trống")
	}

	state := &DesiredState{Routers: make(map[string]RouterSpec, len(routersNode.Content)/2)}
	for i := 0; i+1 < len(routersNode.Content); i += 2 {
		keyNode := routersNode.Content[i]
		valNode := routersNode.Content[i+1]

		// Only a plain scalar string is a valid hostname. yaml.v3 tags an
		// unquoted number as !!int, which is exactly the case PyYAML turns
		// into an int — reject both for the same message.
		if keyNode.Kind != scalarNode || keyNode.Tag != "!!str" {
			return nil, fmt.Errorf("Tên router không hợp lệ: %q", keyNode.Value)
		}
		hostname := keyNode.Value
		if _, dup := state.Routers[hostname]; dup {
			return nil, fmt.Errorf("Tên router bị trùng: %q", hostname)
		}
		if !hostnameRe.MatchString(hostname) {
			return nil, fmt.Errorf("Tên router không hợp lệ: %q", hostname)
		}

		if valNode.Kind != mappingNode {
			return nil, fmt.Errorf("Router %s: required_patterns phải là danh sách", hostname)
		}
		patternsNode, err := mappingField(valNode, "required_patterns")
		if err != nil {
			return nil, fmt.Errorf("Router %s: %v", hostname, err)
		}
		if patternsNode == nil || patternsNode.Kind != sequenceNode {
			return nil, fmt.Errorf("Router %s: required_patterns phải là danh sách", hostname)
		}

		spec := RouterSpec{Rules: make([]Rule, 0, len(patternsNode.Content))}
		for _, item := range patternsNode.Content {
			if item.Kind != scalarNode {
				return nil, fmt.Errorf("Router %s: mỗi required_pattern phải là chuỗi", hostname)
			}
			pattern := item.Value
			re, err := compilePattern(pattern)
			if err != nil {
				return nil, fmt.Errorf("Regex lỗi trên %s: %q: %v", hostname, pattern, err)
			}
			spec.Rules = append(spec.Rules, Rule{Pattern: pattern, re: re})
		}
		state.Order = append(state.Order, hostname)
		state.Routers[hostname] = spec
	}

	if len(state.Order) == 0 {
		return nil, fmt.Errorf("Danh sách routers trong desired_state.yml đang trống")
	}
	return state, nil
}

var hostnameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ---------------------------------------------------------------- regex parity

// pythonSpace mirrors Python's Unicode-aware `\s`.
//
// This is the one place where a straight port silently changes meaning. Go's
// RE2 `\s` is the ASCII set [\t\n\f\r ]. Python's `\s` also matches vertical
// tab (0x0B), NBSP (U+00A0) and the wider Unicode space category. A rule
// written as `^\s*network ...` therefore matches a config line indented with
// NBSP under Python but not under Go — a compliance rule would start reporting
// NON_COMPLIANT for a configuration Python considered fine.
//
// Rather than rewrite the user's regexes, every `\s`, `\S`, `\d`, `\D`, `\w`
// and `\W` escape is expanded to a RE2 character class with the same meaning as
// Python's, before compilation. Only the escape sequences change; anchors,
// groups, quantifiers and everything else are passed through untouched.
func compilePattern(pattern string) (*regexp.Regexp, error) {
	expanded, err := expandPythonClasses(pattern)
	if err != nil {
		return nil, err
	}
	// Python calls re.search(pattern, actual, re.MULTILINE). The MULTILINE flag
	// is passed per call, not baked into the pattern, so a literal port that
	// only compiles the pattern loses it. Without (?m) every `^` in the shipped
	// rules anchors to the start of the whole file and only the first line of
	// running-config can ever match — 4 of the 5 default rules would report
	// NON_COMPLIANT on a perfectly good router.
	return regexp.Compile("(?m)" + expanded)
}

const (
	// Python \s: Unicode whitespace.
	pySpaceClass = `[\t\n\v\f\r \x{0085}\x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]`
	// RE2 accepts \p{Zs} for the space-separator category; combining it with
	// the explicit controls covers the rest of Python's set.
	pyNotSpaceClass = `[^\t\n\v\f\r \x{0085}\x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]`
	// Python \d: Unicode decimal digits.
	pyDigitClass    = `\p{Nd}`
	pyNotDigitClass = `\P{Nd}`
	// Python \w: alphanumerics plus underscore, Unicode-aware.
	pyWordClass    = `[\p{L}\p{N}_]`
	pyNotWordClass = `[^\p{L}\p{N}_]`
)

// expandPythonClasses replaces Python-specific escapes with equivalent RE2
// character classes. It is careful to skip escaped backslashes so that a
// literal `\\s` in the pattern (an escaped backslash followed by 's') is left
// alone.
func expandPythonClasses(pattern string) (string, error) {
	var b strings.Builder
	b.Grow(len(pattern) + 64)

	inClass := false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]

		// Inside a character class, expanding `\s` into another class would
		// produce a nested-class syntax error, so leave escapes alone and let
		// RE2 handle them. The shipped rules never do this; the branch exists
		// so an unusual but valid pattern still compiles.
		if inClass {
			if c == '\\' && i+1 < len(pattern) {
				b.WriteByte(c)
				i++
				b.WriteByte(pattern[i])
				continue
			}
			if c == ']' {
				inClass = false
			}
			b.WriteByte(c)
			continue
		}

		if c == '[' {
			inClass = true
			b.WriteByte(c)
			continue
		}

		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(pattern) {
			return "", fmt.Errorf("trailing backslash")
		}
		n := pattern[i+1]
		switch n {
		case 's':
			b.WriteString(pySpaceClass)
			i++
		case 'S':
			b.WriteString(pyNotSpaceClass)
			i++
		case 'd':
			b.WriteString(pyDigitClass)
			i++
		case 'D':
			b.WriteString(pyNotDigitClass)
			i++
		case 'w':
			b.WriteString(pyWordClass)
			i++
		case 'W':
			b.WriteString(pyNotWordClass)
			i++
		default:
			b.WriteByte(c)
			b.WriteByte(n)
			i++
		}
	}
	return b.String(), nil
}

// matchPattern reports whether pattern matches anywhere in text, with the same
// line semantics as Python's re.search(..., re.MULTILINE).
//
// Go's (?m) makes ^ and $ match at line boundaries, which is the part of
// MULTILINE that matters here; it does not affect \s expansion, which is handled
// above.
func matchPattern(re *regexp.Regexp, text string) bool {
	return re.MatchString(text)
}

// ---------------------------------------------------------------- audit

// auditDevices compares each router's collected configuration against its
// desired state. Router order follows the desired-state file.
func auditDevices(desired *DesiredState, actualDir string) ([]Device, error) {
	devices := make([]Device, 0, len(desired.Order))
	for _, hostname := range desired.Order {
		spec := desired.Routers[hostname]
		configPath := filepath.Join(actualDir, hostname+".conf")

		if !fileExists(configPath) {
			devices = append(devices, Device{
				Hostname: hostname,
				Status:   statusError,
				Findings: []Finding{{Rule: ruleCollection, Detail: detailNoSnapshot}},
			})
			continue
		}

		raw, err := os.ReadFile(configPath)
		if err != nil {
			// Python would raise and abort; treating it as ERROR keeps the
			// remaining routers auditable, which is the more useful failure.
			devices = append(devices, Device{
				Hostname: hostname,
				Status:   statusError,
				Findings: []Finding{{Rule: ruleCollection, Detail: detailNoSnapshot}},
			})
			continue
		}

		// Two normalisations happen here, both required for parity.
		//
		// 1. errors="replace": invalid UTF-8 becomes U+FFFD rather than
		//    failing the read, which is what Python's errors="replace" does.
		//
		// 2. Universal newlines. Python's Path.read_text() opens in text mode
		//    with newline=None, which translates \r\n and lone \r to \n before
		//    the regex ever sees the text. os.ReadFile does no such thing, so
		//    without this a CRLF snapshot makes every `$`-anchored rule fail:
		//    `^router ospf$` cannot match "router ospf\r". That turns a
		//    compliant router NON_COMPLIANT purely because of line endings —
		//    which is exactly what the differential test caught.
		actual := normaliseNewlines(strings.ToValidUTF8(string(raw), "\uFFFD"))

		findings := make([]Finding, 0)
		for _, rule := range spec.Rules {
			if !matchPattern(rule.re, actual) {
				findings = append(findings, Finding{Rule: rule.Pattern, Detail: detailMissing})
			}
		}
		status := statusCompliant
		if len(findings) > 0 {
			status = statusNonCompliant
		}
		devices = append(devices, Device{Hostname: hostname, Status: status, Findings: findings})
	}
	return devices, nil
}

// normaliseNewlines reproduces Python's universal-newline translation, which
// text-mode reads apply automatically.
//
// Order matters: \r\n must collapse to a single \n before lone \r is handled,
// otherwise CRLF would become two newlines and shift line boundaries.
func normaliseNewlines(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func summarise(devices []Device) Summary {
	s := Summary{Total: len(devices)}
	for _, d := range devices {
		switch d.Status {
		case statusCompliant:
			s.Compliant++
		case statusNonCompliant:
			s.NonCompliant++
		case statusError:
			s.Errors++
		}
	}
	return s
}

// overallStatus encodes the precedence rules: any collection error outranks
// non-compliance, which outranks success.
func overallStatus(s Summary) string {
	if s.Errors > 0 {
		return statusError
	}
	if s.NonCompliant > 0 {
		return statusNonCompliant
	}
	return statusCompliant
}

// ---------------------------------------------------------------- collection

// collectConfigs shells out to the Ansible playbook, exactly as the Python
// runner does, so the documented workflow and its output stay unchanged.
//
// The native SSH collector (collectNative) exists because this binary should be
// usable without Ansible on the box; --native selects it. Ansible remains the
// default so existing runbooks and the README stay accurate.
func collectConfigs(actualDir string, opts options) (int, string) {
	if err := os.MkdirAll(actualDir, 0o700); err != nil {
		return 1, fmt.Sprintf("Không tạo được thư mục report: %v", err)
	}
	if opts.skipCollect {
		return 0, "Bỏ qua bước Ansible theo --skip-collect"
	}

	// Remove stale snapshots so an unreachable router is scored ERROR instead of
	// being graded against last run's configuration.
	old, _ := filepath.Glob(filepath.Join(actualDir, "*.conf"))
	for _, f := range old {
		_ = os.Remove(f)
	}

	pb := filepath.Join(resolvePaths().addon, "playbooks", "collect_configs.yml")
	fmt.Println("[1/3] Thu thập cấu hình từ router bằng Ansible...")

	cmd := exec.Command("ansible-playbook", "-i", opts.inventory, pb)
	cmd.Dir = resolvePaths().root
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if stdout.Len() > 0 {
		fmt.Print(stdout.String())
		if !strings.HasSuffix(stdout.String(), "\n") {
			fmt.Println()
		}
	}
	if stderr.Len() > 0 {
		fmt.Fprint(os.Stderr, stderr.String())
		if !strings.HasSuffix(stderr.String(), "\n") {
			fmt.Fprintln(os.Stderr)
		}
	}

	if err != nil {
		if errorsIsNotFound(err) {
			return 127, "Không tìm thấy ansible-playbook. Hãy cài Ansible trên Controller."
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), fmt.Sprintf("Ansible exit code: %d", ee.ExitCode())
		}
		return 1, fmt.Sprintf("Ansible exit code: 1 (%v)", err)
	}
	return 0, "Ansible exit code: 0"
}

func errorsIsNotFound(err error) bool {
	var ee *exec.Error
	return errorsAs(err, &ee)
}

// ---------------------------------------------------------------- report

// writeReport writes compliance/reports/latest.json.
//
// Python writes with ensure_ascii=False, so the Vietnamese detail strings appear
// as UTF-8 rather than \uXXXX escapes. json.Marshal escapes HTML by default but
// leaves non-ASCII alone, so this matches; the SetEscapeHTML(false) is still
// applied to keep `<` and `>` readable in the failures detail if a config ever
// contains them.
func writeReport(path string, report Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	out, err := marshalIndentNoEscape(report)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o600)
}

func marshalIndentNoEscape(v any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(b.String(), "\n")), nil
}
