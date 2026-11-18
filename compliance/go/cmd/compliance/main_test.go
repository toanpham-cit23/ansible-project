package main

// Tests for the compliance port.
//
// The first four mirror compliance/tests/test_compliance.py one-for-one, in the
// same order and with the same assertions, so a reader can diff the two files
// side by side. The rest cover the places where a naive Go translation would
// silently diverge from Python — those are the tests that actually justify the
// port being more than a transcription.

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- fixtures

// writeActual creates actualDir/<hostname>.conf with the given content.
func writeActual(t *testing.T, dir, hostname, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, hostname+".conf"), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s.conf: %v", hostname, err)
	}
}

// desiredFrom builds a DesiredState from an ordered list of (hostname, rules).
// Patterns go through compilePattern, exactly as loadDesiredState does, so the
// tests exercise the real regex path rather than a hand-built stub.
func desiredFrom(t *testing.T, entries [][2]any) *DesiredState {
	t.Helper()
	state := &DesiredState{Routers: map[string]RouterSpec{}}
	for _, e := range entries {
		hostname := e[0].(string)
		patterns := e[1].([]string)
		spec := RouterSpec{}
		for _, p := range patterns {
			re, err := compilePattern(p)
			if err != nil {
				t.Fatalf("compile %q: %v", p, err)
			}
			spec.Rules = append(spec.Rules, Rule{Pattern: p, re: re})
		}
		state.Order = append(state.Order, hostname)
		state.Routers[hostname] = spec
	}
	return state
}

// hostnames returns the device order, for assertion messages.
func hostnames(devices []Device) []string {
	out := make([]string, len(devices))
	for i, d := range devices {
		out[i] = d.Hostname
	}
	return out
}

// ---------------------------------------------------------------- python parity

// test_compliant_router: every rule present -> COMPLIANT with no findings.
func TestCompliantRouter(t *testing.T) {
	dir := t.TempDir()
	writeActual(t, dir, "r1", "hostname r1\nrouter ospf\n")

	desired := desiredFrom(t, [][2]any{
		{"r1", []string{`^hostname\s+r1$`, `^router ospf$`}},
	})

	devices, err := auditDevices(desired, dir)
	if err != nil {
		t.Fatalf("auditDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("got %d devices, want 1", len(devices))
	}
	if devices[0].Status != statusCompliant {
		t.Errorf("status = %q, want %q", devices[0].Status, statusCompliant)
	}
	if len(devices[0].Findings) != 0 {
		t.Errorf("findings = %v, want none", devices[0].Findings)
	}
}

// test_missing_rule_is_noncompliant: one absent rule -> NON_COMPLIANT naming
// exactly that rule.
func TestMissingRuleIsNoncompliant(t *testing.T) {
	dir := t.TempDir()
	writeActual(t, dir, "r1", "hostname r1\n")

	missing := `^router ospf$`
	desired := desiredFrom(t, [][2]any{
		{"r1", []string{`^hostname\s+r1$`, missing}},
	})

	devices, err := auditDevices(desired, dir)
	if err != nil {
		t.Fatalf("auditDevices: %v", err)
	}
	d := devices[0]
	if d.Status != statusNonCompliant {
		t.Fatalf("status = %q, want %q", d.Status, statusNonCompliant)
	}
	if len(d.Findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", d.Findings)
	}
	if d.Findings[0].Rule != missing {
		t.Errorf("finding rule = %q, want %q", d.Findings[0].Rule, missing)
	}
	if d.Findings[0].Detail != detailMissing {
		t.Errorf("finding detail = %q, want %q", d.Findings[0].Detail, detailMissing)
	}
}

// test_missing_snapshot_is_error: no .conf file at all -> ERROR, not
// NON_COMPLIANT. The distinction matters: ERROR means "we could not look",
// which is an operational problem, not a configuration problem.
func TestMissingSnapshotIsError(t *testing.T) {
	dir := t.TempDir()

	desired := desiredFrom(t, [][2]any{
		{"r1", []string{`^hostname\s+r1$`}},
	})

	devices, err := auditDevices(desired, dir)
	if err != nil {
		t.Fatalf("auditDevices: %v", err)
	}
	d := devices[0]
	if d.Status != statusError {
		t.Fatalf("status = %q, want %q", d.Status, statusError)
	}
	if len(d.Findings) != 1 || d.Findings[0].Rule != ruleCollection {
		t.Fatalf("findings = %v, want one %q", d.Findings, ruleCollection)
	}
}

// test_persists_history_and_findings: one run row, one findings row per device,
// and the synthetic "All required rules" row for the compliant device.
func TestPersistsHistoryAndFindings(t *testing.T) {
	dir := t.TempDir()
	writeActual(t, dir, "r1", "hostname r1\n")

	dbPath := filepath.Join(t.TempDir(), "test.db")
	devices := []Device{
		{Hostname: "r1", Status: statusCompliant, Findings: nil},
		{Hostname: "r2", Status: statusNonCompliant, Findings: []Finding{{Rule: "x", Detail: "y"}}},
	}

	runID, err := saveResults(dbPath, devices, "2026-11-16T00:00:00+00:00")
	if err != nil {
		t.Fatalf("saveResults: %v", err)
	}
	if runID != 1 {
		t.Errorf("run_id = %d, want 1", runID)
	}

	conn, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Windows keeps the file handle open until Close, which the Python test
	// hit as a WinError 32 during rmtree. Closing explicitly here avoids that
	// class of flake entirely rather than papering over it.
	defer conn.Close()

	var total, compliant, noncompliant, errs int
	if err := conn.QueryRow(`SELECT devices_total,devices_compliant,devices_noncompliant,devices_error
	                         FROM compliance_runs WHERE id=?`, runID).
		Scan(&total, &compliant, &noncompliant, &errs); err != nil {
		t.Fatalf("query run: %v", err)
	}
	if total != 2 || compliant != 1 || noncompliant != 1 || errs != 0 {
		t.Errorf("counters = %d/%d/%d/%d, want 2/1/1/0", total, compliant, noncompliant, errs)
	}

	// r1 is compliant and therefore has no findings; the writer must still
	// emit the synthetic row so the dashboard shows it was checked.
	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM compliance_findings WHERE run_id=?`, runID).
		Scan(&count); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if count != 2 {
		t.Errorf("findings rows = %d, want 2", count)
	}

	var rule, detail string
	if err := conn.QueryRow(`SELECT rule,detail FROM compliance_findings
	                         WHERE run_id=? AND hostname='r1'`, runID).Scan(&rule, &detail); err != nil {
		t.Fatalf("query r1 finding: %v", err)
	}
	if rule != ruleAllOK || detail != detailAllOK {
		t.Errorf("compliant row = (%q,%q), want (%q,%q)", rule, detail, ruleAllOK, detailAllOK)
	}
}

// ---------------------------------------------------------------- divergence guards

// TestMultilineAnchorsMatchPerLine is the most important test in this file.
//
// Python calls re.search(pattern, actual, re.MULTILINE). MULTILINE is a flag on
// the *call*, not part of the pattern, so porting the pattern alone loses it and
// every `^` anchors to the start of the whole file. With the shipped rules that
// means only the first line of running-config can ever match, and a correctly
// configured router reports NON_COMPLIANT.
//
// This test fails loudly if `(?m)` is ever dropped from compilePattern.
func TestMultilineAnchorsMatchPerLine(t *testing.T) {
	// The rule appears on line 4. Without (?m) it can never match.
	config := "! built by vtysh\nhostname r1\n!\nrouter ospf\n network 1.1.1.1/32 area 0\n!\n"

	cases := []struct {
		name    string
		pattern string
		want    bool
	}{
		{"middle line ^...$", `^router ospf$`, true},
		{"indented rule", `^\s*network 1\.1\.1\.1/32 area 0$`, true},
		{"last line", `^!$`, true},
		{"absent rule", `^network 192\.168\.23\.0/24 area 0$`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			re, err := compilePattern(tc.pattern)
			if err != nil {
				t.Fatalf("compile %q: %v", tc.pattern, err)
			}
			if got := matchPattern(re, config); got != tc.want {
				t.Errorf("match(%q) = %v, want %v", tc.pattern, got, tc.want)
			}
		})
	}
}

// TestPythonSpaceClassMatchesUnicodeWhitespace guards the `\s` divergence.
//
// Go's RE2 `\s` is [\t\n\f\r ]. Python's `\s` is the Unicode whitespace set and
// also matches vertical tab (0x0B) and NBSP (U+00A0). A config line indented
// with either would satisfy `^\s*network ...` under Python and fail under a
// naive Go port, flipping a compliant router to NON_COMPLIANT.
func TestPythonSpaceClassMatchesUnicodeWhitespace(t *testing.T) {
	cases := []struct {
		name string
		pad  string
	}{
		{"plain space", " "},
		{"tab", "\t"},
		{"vertical tab U+000B", "\v"},
		{"form feed", "\f"},
		{"NBSP U+00A0", "\u00A0"},
		{"ideographic space U+3000", "\u3000"},
		{"en space U+2002", "\u2002"},
	}

	re, err := compilePattern(`^\s*passive-interface lo$`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := "hostname r1\n" + tc.pad + "passive-interface lo\n"
			if !matchPattern(re, config) {
				t.Errorf("rule did not match line indented with %q", tc.pad)
			}
		})
	}
}

// TestWordAndDigitClassesAreUnicodeAware covers the other three expansions. A
// rule using \w or \d against non-ASCII text must behave as Python does.
func TestWordAndDigitClassesAreUnicodeAware(t *testing.T) {
	cases := []struct {
		pattern string
		text    string
		want    bool
	}{
		{`^\w+$`, "café", true}, // Python \w includes accented letters
		{`^\d+$`, "١٢٣", true},  // Arabic-Indic digits are \d in Python
		{`^\W+$`, "!!!", true},  // non-word
		{`^\D+$`, "abc", true},  // non-digit
		{`^\w+$`, "a b", false}, // space is not a word char
	}
	for _, tc := range cases {
		re, err := compilePattern(tc.pattern)
		if err != nil {
			t.Fatalf("compile %q: %v", tc.pattern, err)
		}
		if got := re.MatchString(tc.text); got != tc.want {
			t.Errorf("match(%q, %q) = %v, want %v", tc.pattern, tc.text, got, tc.want)
		}
	}
}

// TestExpandLeavesEscapedBackslashAlone: a literal `\\s` in a pattern means
// "backslash followed by s" and must not be rewritten into a character class.
func TestExpandLeavesEscapedBackslashAlone(t *testing.T) {
	got, err := expandPythonClasses(`\\s`)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if got != `\\s` {
		t.Errorf("expanded %q to %q, want it unchanged", `\\s`, got)
	}

	re, err := compilePattern(`\\s`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !re.MatchString(`a\sb`) {
		t.Error(`pattern \\s should match a literal backslash followed by s`)
	}
	if re.MatchString("a b") {
		t.Error(`pattern \\s must not match a space`)
	}
}

// TestEscapesInsideCharacterClassArePreserved: expanding `\s` inside an
// existing class would produce `[[...]]`, which RE2 rejects. The expander must
// leave in-class escapes for RE2 to handle.
func TestEscapesInsideCharacterClassArePreserved(t *testing.T) {
	got, err := expandPythonClasses(`[\s]`)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if got != `[\s]` {
		t.Errorf("expanded to %q, want unchanged", got)
	}
	if _, err := compilePattern(`[\s]+`); err != nil {
		t.Errorf("compile [\\s]+: %v", err)
	}
}

// ---------------------------------------------------------------- yaml validation

// writeDesired writes a desired_state.yml fixture and returns its path.
func writeDesired(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "desired_state.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write desired state: %v", err)
	}
	return p
}

// TestLoadDesiredStateHappyPath also asserts file order is preserved, which a
// map-based decode would lose.
func TestLoadDesiredStateHappyPath(t *testing.T) {
	p := writeDesired(t, `routers:
  r3:
    required_patterns:
      - '^hostname\s+r3$'
  r1:
    required_patterns:
      - '^hostname\s+r1$'
      - '^router ospf$'
`)
	state, err := loadDesiredState(p)
	if err != nil {
		t.Fatalf("loadDesiredState: %v", err)
	}
	want := []string{"r3", "r1"}
	if strings.Join(state.Order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v (file order must be preserved)", state.Order, want)
	}
	if len(state.Routers["r1"].Rules) != 2 {
		t.Errorf("r1 rules = %d, want 2", len(state.Routers["r1"].Rules))
	}
}

// TestNumericRouterKeyIsRejected covers the PyYAML/yaml.v3 divergence.
//
// PyYAML resolves an unquoted `123:` to the integer 123, which the Python code
// rejects. yaml.v3 implements YAML 1.2 and would decode it as the string "123",
// silently accepting a numeric hostname. The port decodes into yaml.Node
// precisely so it can reject this the same way Python does.
func TestNumericRouterKeyIsRejected(t *testing.T) {
	p := writeDesired(t, `routers:
  123:
    required_patterns:
      - '^hostname 123$'
`)
	_, err := loadDesiredState(p)
	if err == nil {
		t.Fatal("numeric router key was accepted; Python rejects it")
	}
	if !strings.Contains(err.Error(), "Tên router không hợp lệ") {
		t.Errorf("error = %q, want the Python message 'Tên router không hợp lệ'", err)
	}
}

// TestLoadDesiredStateErrors checks every validation branch returns the same
// Vietnamese message the Python original produces.
func TestLoadDesiredStateErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "routers is a list not a mapping",
			body: "routers:\n  - r1\n",
			want: "'routers:' dạng mapping",
		},
		{
			name: "no routers key",
			body: "something: else\n",
			want: "'routers:' dạng mapping",
		},
		{
			name: "empty routers",
			body: "routers: {}\n",
			want: "đang trống",
		},
		{
			name: "invalid hostname",
			body: "routers:\n  \"bad host\":\n    required_patterns: []\n",
			want: "Tên router không hợp lệ",
		},
		{
			name: "required_patterns not a list",
			body: "routers:\n  r1:\n    required_patterns: nope\n",
			want: "required_patterns phải là danh sách",
		},
		{
			name: "non-string pattern",
			body: "routers:\n  r1:\n    required_patterns:\n      - {a: b}\n",
			want: "mỗi required_pattern phải là chuỗi",
		},
		{
			name: "bad regex",
			body: "routers:\n  r1:\n    required_patterns:\n      - '('\n",
			want: "Regex lỗi trên r1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadDesiredState(writeDesired(t, tc.body))
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestMissingDesiredStateIsReported: a missing file must produce the Python
// message, not a bare ENOENT.
func TestMissingDesiredStateIsReported(t *testing.T) {
	_, err := loadDesiredState(filepath.Join(t.TempDir(), "nope.yml"))
	if err == nil || !strings.Contains(err.Error(), "Không tìm thấy desired-state") {
		t.Fatalf("error = %v, want 'Không tìm thấy desired-state'", err)
	}
}

// TestShippedDesiredStateLoads parses the real config file, so a typo in
// compliance/config/desired_state.yml fails the build rather than a live run.
func TestShippedDesiredStateLoads(t *testing.T) {
	p := filepath.Join("..", "..", "..", "config", "desired_state.yml")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("shipped desired_state.yml not found at %s", p)
	}
	state, err := loadDesiredState(p)
	if err != nil {
		t.Fatalf("shipped desired state failed to load: %v", err)
	}
	if len(state.Order) != 3 {
		t.Errorf("routers = %v, want 3 entries", state.Order)
	}
}

// TestCRLFConfigIsGradedLikePython is a regression test for a real divergence
// the differential harness caught.
//
// Python's Path.read_text() uses universal newlines, so a CRLF snapshot becomes
// LF before any regex runs. os.ReadFile leaves the CR in place, and then
// `^router ospf$` cannot match "router ospf\r" — every $-anchored rule fails and
// a compliant router is reported NON_COMPLIANT. This was not a theoretical
// concern: it is what happened the first time the parity script ran on Windows.
func TestCRLFConfigIsGradedLikePython(t *testing.T) {
	dir := t.TempDir()
	writeActual(t, dir, "r1",
		"hostname r1\r\nrouter ospf\r\n network 1.1.1.1/32 area 0\r\n")

	desired := desiredFrom(t, [][2]any{
		{"r1", []string{
			`^hostname\s+r1$`,
			`^router ospf$`,
			`^\s*network 1\.1\.1\.1/32 area 0$`,
		}},
	})

	devices, err := auditDevices(desired, dir)
	if err != nil {
		t.Fatalf("auditDevices: %v", err)
	}
	if devices[0].Status != statusCompliant {
		t.Fatalf("CRLF config graded %q, want %q; findings: %v",
			devices[0].Status, statusCompliant, devices[0].Findings)
	}
}

// TestNormaliseNewlines pins the translation, including the ordering trap:
// CRLF must collapse to one LF, not two.
func TestNormaliseNewlines(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\nb", "a\nb"},
		{"a\r\nb", "a\nb"},
		{"a\rb", "a\nb"},
		{"a\r\nb\r\nc", "a\nb\nc"},
		{"\r\n\r\n", "\n\n"},
		{"no newline", "no newline"},
	}
	for _, tc := range cases {
		if got := normaliseNewlines(tc.in); got != tc.want {
			t.Errorf("normaliseNewlines(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestInvalidUTF8IsReplaced: Python reads with errors="replace", so invalid
// bytes become U+FFFD instead of aborting the audit.
func TestInvalidUTF8IsReplaced(t *testing.T) {
	dir := t.TempDir()
	// 0xff is not valid UTF-8. The hostname rule must still match the good
	// part of the file.
	if err := os.WriteFile(filepath.Join(dir, "r1.conf"),
		[]byte("hostname r1\nremark \xff\xfe bad bytes\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	desired := desiredFrom(t, [][2]any{
		{"r1", []string{`^hostname\s+r1$`}},
	})
	devices, err := auditDevices(desired, dir)
	if err != nil {
		t.Fatalf("auditDevices: %v", err)
	}
	if devices[0].Status != statusCompliant {
		t.Errorf("status = %q, want COMPLIANT; invalid UTF-8 must not abort the read", devices[0].Status)
	}
}

// TestShippedRulesAgainstRealFRRConfig pins how the shipped rules grade a real
// router capture.
//
// This is a genuine capture from r1 (FRR 10.6.1) via the native collector. Two
// of the five rules used to fail against it, for two unrelated reasons:
//
//   - network 192.168.23.0/24 area 0 : that prefix is no longer advertised in
//     OSPF; the transit links 10.0.1.0/29 and 10.0.3.0/29 replaced it. This is
//     real configuration drift and the rule is LEFT UNCHANGED — it must keep
//     failing until the lab owner decides whether the /24 or the /29 design is
//     the intended one.
//   - passive-interface lo : this was a rule bug, not a router bug. FRR 10.6
//     renders loopback passivation as "ip ospf passive" under "interface lo",
//     never as "passive-interface lo" under "router ospf", so a freshly
//     provisioned router failed a rule no configuration could satisfy. The
//     shipped rule now accepts both renderings, scoped to the "interface lo"
//     block so that passivating some other interface cannot satisfy it.
//
// The drift finding stays pinned: this test fails if someone widens the /24
// rule to make the audit report COMPLIANT, which would hide a real discrepancy
// between the repository and the network. The passivation assertion is also
// pinned in both directions — the lazy branch must not match a different
// interface's block — so a future "simplification" back to a bare
// `ip ospf passive` cannot quietly reintroduce the false pass.
const realR1Config = `Building configuration...

Current configuration:
!
frr version 10.6.1
frr defaults traditional
hostname r1
log commands
log syslog informational
log file /var/log/frr/audit.log informational
service integrated-vtysh-config
!
interface lo
 ip address 1.1.1.1/32
 ip ospf passive
exit
!
router ospf
 network 1.1.1.1/32 area 0
 network 10.0.1.0/29 area 0
 network 10.0.3.0/29 area 0
exit
!
access-list 10 seq 5 permit 192.168.23.0 0.0.0.255
access-list 10 seq 10 deny any
!
line vty
 access-class 10
 no login
exit
!
end
`

// shippedDesiredPath is the real rule file. Tests below read it rather than
// duplicating the regexes, because a copy in the test file can drift away from
// the file that actually grades the lab.
var shippedDesiredPath = filepath.Join("..", "..", "..", "config", "desired_state.yml")

// loadShippedRules returns the required_patterns for one router from the shipped
// desired_state.yml, skipping if the file is absent (e.g. a vendored checkout).
func loadShippedRules(t *testing.T, router string) []string {
	t.Helper()
	if _, err := os.Stat(shippedDesiredPath); err != nil {
		t.Skipf("shipped desired_state.yml not found at %s", shippedDesiredPath)
	}
	state, err := loadDesiredState(shippedDesiredPath)
	if err != nil {
		t.Fatalf("load shipped desired state: %v", err)
	}
	spec, ok := state.Routers[router]
	if !ok {
		t.Fatalf("router %q missing from shipped desired state (have %v)", router, state.Order)
	}
	patterns := make([]string, len(spec.Rules))
	for i, r := range spec.Rules {
		patterns[i] = r.Pattern
	}
	return patterns
}

func TestShippedRulesAgainstRealFRRConfig(t *testing.T) {
	patterns := loadShippedRules(t, "r1")
	if len(patterns) != 5 {
		t.Fatalf("r1 ships %d rules, want 5: %v", len(patterns), patterns)
	}
	lanRule, passivationRule := patterns[2], patterns[4]

	cases := []struct {
		name    string
		pattern string
		want    bool
		why     string
	}{
		{"hostname", patterns[0], true, ""},
		{"ospf process", patterns[1], true, ""},
		{"loopback passivation, FRR 10.6 rendering", passivationRule, true,
			"FRR 10.6 emits 'ip ospf passive' under 'interface lo'"},
		{"LAN network is still drifted", lanRule, false,
			"192.168.23.0/24 was replaced by the transit links 10.0.1.0/29 and 10.0.3.0/29"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			re, err := compilePattern(tc.pattern)
			if err != nil {
				t.Fatalf("compile %q: %v", tc.pattern, err)
			}
			if got := matchPattern(re, normaliseNewlines(realR1Config)); got != tc.want {
				msg := ""
				if tc.why != "" {
					msg = " (" + tc.why + ")"
				}
				t.Errorf("match(%q) = %v, want %v%s", tc.pattern, got, tc.want, msg)
			}
		})
	}

	// The end-to-end verdict for r1 must be NON_COMPLIANT with exactly one
	// finding left: the drifted /24. Passivation must no longer appear.
	dir := t.TempDir()
	writeActual(t, dir, "r1", realR1Config)

	desired := desiredFrom(t, [][2]any{{"r1", patterns}})

	devices, err := auditDevices(desired, dir)
	if err != nil {
		t.Fatalf("auditDevices: %v", err)
	}
	if devices[0].Status != statusNonCompliant {
		t.Errorf("status = %q; expected NON_COMPLIANT until the /24 drift is resolved",
			devices[0].Status)
	}
	if len(devices[0].Findings) != 1 {
		t.Fatalf("findings = %d, want 1 (only the /24 drift): %v",
			len(devices[0].Findings), devices[0].Findings)
	}
	if devices[0].Findings[0].Rule != lanRule {
		t.Errorf("finding rule = %q, want the /24 drift rule %q",
			devices[0].Findings[0].Rule, lanRule)
	}
}

// TestPassivationRuleIsScopedToLoopback pins the negative direction of the new
// passivation rule. The lazy branch must only be satisfied by "ip ospf passive"
// inside the "interface lo" block; the unindented "exit" that closes the block
// is what stops it from running on into the next interface.
func TestPassivationRuleIsScopedToLoopback(t *testing.T) {
	patterns := loadShippedRules(t, "r1")
	re, err := compilePattern(patterns[len(patterns)-1])
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "FRR 10.6 block form",
			body: "interface lo\n ip address 1.1.1.1/32\n ip ospf passive\nexit\n",
			want: true,
		},
		{
			name: "legacy router-level form",
			body: "router ospf\n network 1.1.1.1/32 area 0\n passive-interface lo\n",
			want: true,
		},
		{
			name: "passivation on a different interface only",
			body: "interface lo\n ip address 1.1.1.1/32\nexit\n!\ninterface eth0\n ip ospf passive\nexit\n",
			want: false,
		},
		{
			name: "loopback present but not passivated",
			body: "interface lo\n ip address 1.1.1.1/32\n ip ospf cost 10\nexit\n",
			want: false,
		},
		{
			name: "no loopback block at all",
			body: "router ospf\n network 1.1.1.1/32 area 0\n",
			want: false,
		},
		{
			name: "CRLF config, as read by the Python runner",
			body: "interface lo\r\n ip address 1.1.1.1/32\r\n ip ospf passive\r\nexit\r\n",
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchPattern(re, normaliseNewlines(tc.body)); got != tc.want {
				t.Errorf("match = %v, want %v for %q", got, tc.want, tc.body)
			}
		})
	}
}

// ---------------------------------------------------------------- report shape

// TestReportJSONMatchesPythonKeys pins the JSON field names. The dashboard
// parses this document, so a renamed field is a silent breakage.
func TestReportJSONMatchesPythonKeys(t *testing.T) {
	report := Report{
		RunID:              1,
		CheckedAt:          "2026-11-16T00:00:00+00:00",
		Status:             statusError,
		CollectionExitCode: 0,
		Summary:            Summary{Total: 3, Compliant: 1, NonCompliant: 1, Errors: 1},
		Devices: []Device{
			// A compliant device with an empty (non-nil) slice: must marshal
			// as [] because Python writes an empty list, not null.
			{Hostname: "r1", Status: statusCompliant, Findings: []Finding{}},
			// A device with a real finding, so rule/detail appear.
			{Hostname: "r2", Status: statusNonCompliant,
				Findings: []Finding{{Rule: `^router ospf$`, Detail: detailMissing}}},
		},
	}
	out, err := marshalIndentNoEscape(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)

	for _, want := range []string{
		`"run_id"`, `"checked_at"`, `"status"`, `"collection_exit_code"`,
		`"summary"`, `"total"`, `"compliant"`, `"noncompliant"`, `"errors"`,
		`"devices"`, `"hostname"`, `"findings"`, `"rule"`, `"detail"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("report JSON missing key %s\n%s", want, s)
		}
	}
	// Python writes noncompliant without an underscore; keep it that way.
	if strings.Contains(s, `"non_compliant"`) {
		t.Error(`report JSON used "non_compliant"; Python uses "noncompliant"`)
	}
	// ensure_ascii=False: Vietnamese must be literal UTF-8, not \u escapes.
	if strings.Contains(s, `\u00`) || strings.Contains(s, `\u1ea`) {
		t.Errorf("report JSON escaped non-ASCII; Python writes literal UTF-8\n%s", s)
	}
	// An empty findings list must marshal as [] and not null.
	if !strings.Contains(s, `"findings": []`) {
		t.Errorf(`empty findings did not marshal as []\n%s`, s)
	}
}

// TestBaselineParity reproduces compliance/tests/_baseline.py in Go and compares
// the result field by field. The Python script established the ground truth:
//
//	r1 COMPLIANT (no findings), r2 NON_COMPLIANT (missing the /24 rule),
//	r3 ERROR (no snapshot), run_id 1, counters 3/1/1/1.
func TestBaselineParity(t *testing.T) {
	dir := t.TempDir()
	writeActual(t, dir, "r1",
		"hostname r1\nrouter ospf\n network 1.1.1.1/32 area 0\n"+
			" network 192.168.23.0/24 area 0\n passive-interface lo\n")
	writeActual(t, dir, "r2",
		"hostname r2\nrouter ospf\n network 2.2.2.2/32 area 0\n")

	desired := desiredFrom(t, [][2]any{
		{"r1", []string{
			`^hostname\s+r1$`, `^router ospf$`,
			`^\s*network 192\.168\.23\.0/24 area 0$`,
			`^\s*network 1\.1\.1\.1/32 area 0$`,
			`^\s*passive-interface lo$`}},
		{"r2", []string{
			`^hostname\s+r2$`, `^router ospf$`,
			`^\s*network 192\.168\.23\.0/24 area 0$`}},
		{"r3", []string{`^hostname\s+r3$`}},
	})

	devices, err := auditDevices(desired, dir)
	if err != nil {
		t.Fatalf("auditDevices: %v", err)
	}

	if got := strings.Join(hostnames(devices), ","); got != "r1,r2,r3" {
		t.Fatalf("order = %s, want r1,r2,r3", got)
	}

	wantStatus := []string{statusCompliant, statusNonCompliant, statusError}
	for i, want := range wantStatus {
		if devices[i].Status != want {
			t.Errorf("%s status = %q, want %q", devices[i].Hostname, devices[i].Status, want)
		}
	}
	if len(devices[0].Findings) != 0 {
		t.Errorf("r1 findings = %v, want none", devices[0].Findings)
	}
	if len(devices[1].Findings) != 1 {
		t.Fatalf("r2 findings = %v, want 1", devices[1].Findings)
	}
	if devices[1].Findings[0].Rule != `^\s*network 192\.168\.23\.0/24 area 0$` {
		t.Errorf("r2 finding rule = %q (regression: matched a rule Python did not)",
			devices[1].Findings[0].Rule)
	}
	if len(devices[2].Findings) != 1 || devices[2].Findings[0].Rule != ruleCollection {
		t.Errorf("r3 findings = %v, want one %q", devices[2].Findings, ruleCollection)
	}

	sum := summarise(devices)
	if sum != (Summary{Total: 3, Compliant: 1, NonCompliant: 1, Errors: 1}) {
		t.Errorf("summary = %+v, want 3/1/1/1", sum)
	}
	if got := overallStatus(sum); got != statusError {
		t.Errorf("overall = %q, want %q", got, statusError)
	}

	// Same DB round-trip the baseline performed.
	dbPath := filepath.Join(t.TempDir(), "baseline.db")
	runID, err := saveResults(dbPath, devices, "2026-11-16T00:00:00+00:00")
	if err != nil {
		t.Fatalf("saveResults: %v", err)
	}
	if runID != 1 {
		t.Errorf("run_id = %d, want 1", runID)
	}

	conn, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	var (
		checkedAt, status, source  string
		total, comp, noncomp, errs int
	)
	if err := conn.QueryRow(`SELECT checked_at,status,devices_total,devices_compliant,
	                                devices_noncompliant,devices_error,source
	                         FROM compliance_runs WHERE id=1`).
		Scan(&checkedAt, &status, &total, &comp, &noncomp, &errs, &source); err != nil {
		t.Fatalf("query run: %v", err)
	}
	// Matches the Python row exactly:
	//   (1, '2026-11-16T00:00:00+00:00', 'ERROR', 3, 1, 1, 1, 'ansible')
	if checkedAt != "2026-11-16T00:00:00+00:00" || status != statusError ||
		total != 3 || comp != 1 || noncomp != 1 || errs != 1 || source != "ansible" {
		t.Errorf("run row = (%s,%s,%d,%d,%d,%d,%s), want the Python baseline row",
			checkedAt, status, total, comp, noncomp, errs, source)
	}

	rows, err := conn.Query(`SELECT hostname,status,rule FROM compliance_findings
	                         WHERE run_id=1 ORDER BY id`)
	if err != nil {
		t.Fatalf("query findings: %v", err)
	}
	defer rows.Close()

	type findingRow struct{ host, status, rule string }
	var got []findingRow
	for rows.Next() {
		var r findingRow
		if err := rows.Scan(&r.host, &r.status, &r.rule); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := []findingRow{
		{"r1", statusCompliant, ruleAllOK},
		{"r2", statusNonCompliant, `^\s*network 192\.168\.23\.0/24 area 0$`},
		{"r3", statusError, ruleCollection},
	}
	if len(got) != len(want) {
		t.Fatalf("findings = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finding[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------- args & exit codes

func TestParseArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		// want* are compared after filepath.Clean/Abs, because parseArgs
		// deliberately resolves paths the way Python's Path.resolve() does.
		wantInventory string
		wantDB        string
		wantSkip      bool
		wantNative    bool
		wantVerbose   bool
		wantErr       bool
	}{
		{
			name:          "defaults",
			args:          nil,
			wantInventory: "hosts.ini",
		},
		{
			name:          "space separated",
			args:          []string{"--inventory", "inv.ini", "--db", "x.db"},
			wantInventory: "inv.ini",
			wantDB:        "x.db",
		},
		{
			name:          "equals separated",
			args:          []string{"--inventory=inv.ini", "--db=x.db"},
			wantInventory: "inv.ini",
			wantDB:        "x.db",
		},
		{
			name:          "skip collect",
			args:          []string{"--skip-collect"},
			wantInventory: "hosts.ini",
			wantSkip:      true,
		},
		{
			name:          "native and verbose",
			args:          []string{"--native", "--verbose"},
			wantInventory: "hosts.ini",
			wantNative:    true,
			wantVerbose:   true,
		},
		{
			name:          "short verbose",
			args:          []string{"-v"},
			wantInventory: "hosts.ini",
			wantVerbose:   true,
		},
		{
			name:    "unknown flag",
			args:    []string{"--nope"},
			wantErr: true,
		},
		{
			name:    "missing value",
			args:    []string{"--inventory"},
			wantErr: true,
		},
		{
			name:    "bad duration",
			args:    []string{"--timeout", "soon"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgs: %v", err)
			}
			// Paths come back absolute (Python calls .resolve()); compare the
			// tail so the assertion holds regardless of the working directory.
			if tc.wantInventory != "" && filepath.Base(got.inventory) != filepath.Base(tc.wantInventory) {
				t.Errorf("inventory = %q, want a path ending in %q", got.inventory, tc.wantInventory)
			}
			if tc.wantDB != "" && filepath.Base(got.db) != filepath.Base(tc.wantDB) {
				t.Errorf("db = %q, want a path ending in %q", got.db, tc.wantDB)
			}
			if got.skipCollect != tc.wantSkip {
				t.Errorf("skipCollect = %v, want %v", got.skipCollect, tc.wantSkip)
			}
			if got.native != tc.wantNative {
				t.Errorf("native = %v, want %v", got.native, tc.wantNative)
			}
			if got.verbose != tc.wantVerbose {
				t.Errorf("verbose = %v, want %v", got.verbose, tc.wantVerbose)
			}
		})
	}
}

// TestParseArgsResolvesInventoryToAbsolutePath: the Python version calls
// .resolve(), so downstream messages print an absolute path. Relative paths
// must be made absolute here too or the two produce different log lines.
func TestParseArgsResolvesInventoryToAbsolutePath(t *testing.T) {
	got, err := parseArgs([]string{"--inventory", "hosts.ini"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !filepath.IsAbs(got.inventory) {
		t.Errorf("inventory = %q, want an absolute path", got.inventory)
	}
}

// TestExitCodes documents the contract: 0 compliant, 1 findings, 2 bad input.
func TestExitCodes(t *testing.T) {
	if exitOK != 0 || exitFindings != 1 || exitInput != 2 {
		t.Fatalf("exit codes = %d/%d/%d, want 0/1/2", exitOK, exitFindings, exitInput)
	}
}

// TestBadInventoryPathReturnsInputError: a missing inventory with collection
// enabled is a setup mistake (exit 2), not a finding (exit 1).
func TestBadInventoryPathReturnsInputError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.ini")
	// resolvePaths walks up from the working directory, so point the binary at
	// the real project root to find desired_state.yml.
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	t.Setenv("COMPLIANCE_ROOT", root)

	if code := run([]string{"--inventory", missing}); code != exitInput {
		t.Errorf("exit code = %d, want %d for a missing inventory", code, exitInput)
	}
}

// ---------------------------------------------------------------- inventory parsing

// TestParseInventory reproduces the shipped hosts.ini shape, including the
// [routers:vars] block that must apply to every host.
func TestParseInventory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts.ini")
	body := `# comment line
[routers]
r1 ansible_host=192.168.23.11 loopback_ip=1.1.1.1 ansible_password=CHANGE_ME
r2 ansible_host=192.168.23.12 ansible_user=admin

[routers:vars]
ansible_connection=ssh
ansible_ssh_private_key_file=/root/.ssh/id_labops
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	hosts, err := parseInventory(p)
	if err != nil {
		t.Fatalf("parseInventory: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %d, want 2", len(hosts))
	}

	if hosts[0].Name != "r1" || hosts[0].Address != "192.168.23.11" {
		t.Errorf("r1 = %+v", hosts[0])
	}
	// Group vars reach r1, which had no ansible_user of its own.
	if hosts[0].User != "root" {
		t.Errorf("r1 user = %q, want root (default; group vars set no user)", hosts[0].User)
	}
	// Host vars beat group vars: r2 overrides the key with nothing, so the
	// group key still applies. r2 set its own ansible_user.
	if hosts[1].User != "admin" {
		t.Errorf("r2 user = %q, want admin (host var wins)", hosts[1].User)
	}
	if hosts[0].KeyFile != "/root/.ssh/id_labops" {
		t.Errorf("r1 keyfile = %q, want the group-var value", hosts[0].KeyFile)
	}
	if hosts[0].Port != 22 {
		t.Errorf("r1 port = %d, want the default 22", hosts[0].Port)
	}
}

// TestParseInventoryEmptyRouters: an inventory with no [routers] entries is a
// setup error, not an empty successful run.
func TestParseInventoryEmptyRouters(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts.ini")
	if err := os.WriteFile(p, []byte("[routers]\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := parseInventory(p); err == nil {
		t.Fatal("expected an error for an inventory with no routers")
	}
}

// TestAuthMethodsRejectsHostWithoutCredentials: failing at auth-method build
// time gives a clear message instead of an opaque SSH handshake failure.
func TestAuthMethodsRejectsHostWithoutCredentials(t *testing.T) {
	_, err := authMethods(inventoryHost{Name: "r9", Address: "192.0.2.9"})
	if err == nil {
		t.Fatal("expected an error for a host with no key and no password")
	}
	if !strings.Contains(err.Error(), "không có phương thức xác thực") {
		t.Errorf("error = %q, want the Vietnamese auth message", err)
	}
}
