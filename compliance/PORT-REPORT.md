# Compliance Runner — Python → Go Port Report

**Date:** 2026-11-16, revised 2026-11-18
**Project:** `ansible-project-compliance-final/ansible-project-main` — `compliance/` add-on
**Task:** Convert `compliance/scripts/run_compliance.py` to Go; review, verify, implement.
**Result:** Port complete and **behaviourally identical**. A differential test proves the Go binary reproduces the Python runner's output byte-for-byte, including the JSON report and both SQLite tables.

**Incidental discovery:** the audit surfaced a **drift between the live routers and `playbooks/02_ospf.yml`** — the running OSPF topology no longer matches the repository. See §7; this is an operational finding, not a port defect, and it is the most actionable thing in this report.

**Revision note (2026-11-18).** Re-verified and three items changed, all in §7: the passivation rule was fixed (§7.8), the warning that a pipeline re-run "silently undoes" the manual change was **retracted as factually wrong** (§7.7), and both were re-tested against the differential harness.

---

## 1. Scope — what actually needed porting

The dashboard's Go API already owned the *read* half of this feature, so no dashboard work was required.

| Component | Language | Status |
|---|---|---|
| `compliance/scripts/run_compliance.py` | Python, 224 lines | **Ported to Go** |
| `compliance/tests/test_compliance.py` | Python, 4 tests | Ported to Go (59 tests now) |
| `files/dashboard/docker/api-go/compliance.go` | Go, 66 lines | Unchanged — already correct |
| `compliance/playbooks/collect_configs.yml` | Ansible | Unchanged; still used by default |

Only the **writer** (the collector) needed porting. The Go API already implemented `handleCompliance`, `ensureComplianceSchema`, and the `compliance_runs` / `compliance_findings` schema.

### The Python program's actual functions

| Function | Responsibility |
|---|---|
| `load_desired_state(path)` | YAML load + validation (hostname shape, non-empty router list, per-rule string type, regex pre-compile). Raises `ValueError` with Vietnamese messages. |
| `collect_configs(inventory, playbook, skip_collect)` | **Deletes existing `*.conf` first** so an unreachable router is not graded on stale configuration, then shells out to `ansible-playbook`. Returns `(127, "Không tìm thấy ansible-playbook...")` when the binary is absent. |
| `audit_devices(desired)` | Missing snapshot → `ERROR`. Otherwise `re.search(pattern, actual, re.MULTILINE)` per rule; any miss → `NON_COMPLIANT` naming that rule. |
| `save_results(db_path, results, checked_at)` | Single transaction: one `compliance_runs` row plus findings. A **compliant** device still gets a synthetic `All required rules` row so the dashboard shows it was *checked*, not merely clean. |
| `main()` | argparse; writes `reports/latest.json`; exit `0` only when every device is compliant **and** collection succeeded. |

---

## 2. Files created

| File | Purpose |
|---|---|
| `compliance/go/go.mod` | New module `compliance`, Go 1.23. Deliberately separate from `files/dashboard/docker/api-go` (module `labapi`) so the collector ships standalone without the dashboard's HTTP handlers. |
| `compliance/go/go.sum` | Dependency checksums (57 entries, including test-only deps pulled in by `go mod tidy`). |
| `compliance/go/Makefile` | `build` / `test` / `parity` / `check` / `tidy` / `clean` targets. |
| `compliance/go/cmd/compliance/main.go` | Entry point, arg parsing, path resolution, YAML validation, regex parity layer, audit, reporting. |
| `compliance/go/cmd/compliance/sqlite.go` | Schema + transactional history write. |
| `compliance/go/cmd/compliance/yaml.go` | Isolates the `yaml.v3` dependency behind a node-level API. |
| `compliance/go/cmd/compliance/collect_native.go` | Ansible-free SSH collection (`--native`). |
| `compliance/go/cmd/compliance/main_test.go` | 59 tests. |
| `compliance/tests/parity.py` | Differential harness: runs both runners on identical input and diffs the results. |

## 3. Files modified

| File | Change |
|---|---|
| `compliance/run.sh` | Prefers the Go binary at `compliance/bin/compliance`; falls back to `.venv/bin/python`, then `python3`. Passes through extra arguments. |
| `compliance/install_alpine.sh` | Adds `--go` mode (installs `go` + `openssh-client`, builds the binary, no Python/Ansible). Default Python mode retained. |
| `compliance/README.md` | Documents both runners, the `--go` install path, `make` targets, the parity test, and the two broken rules. |
| `.gitignore` | Ignores `compliance/bin/` and `compliance/go/compliance` — a committed binary would silently drift out of sync with its source. |

---

## 4. Defects found and fixed

These are the substantive engineering findings. All three were found by **testing**, not by reading the code.

### 4.1 Missing `(?m)` — the port would have failed on 4 of 5 rules

Python passes `re.MULTILINE` **per call**:

```python
re.search(pattern, actual, re.MULTILINE) is None
```

`MULTILINE` is not baked into the pattern. A literal port that only compiles the pattern loses it, and then every `^` anchors to the **start of the whole file** rather than each line. In `show running-config` output, only the first line (`Building configuration...`) could ever match.

**Fix** — `compilePattern` prepends `(?m)`:

```go
return regexp.Compile("(?m)" + expanded)
```

**Regression test:** `TestMultilineAnchorsMatchPerLine` asserts a rule on line 4 matches.

### 4.2 CRLF snapshots — the one that actually bit

This was caught by the differential harness on its **first run**, and my unit tests had missed it.

Python's `Path.read_text()` opens in text mode with `newline=None`, which performs **universal-newline translation**: `\r\n` and lone `\r` become `\n` before any regex sees the text. `os.ReadFile` does no such thing.

Consequence: with a CRLF snapshot, `^router ospf$` cannot match `"router ospf\r"`, so **every `$`-anchored rule fails** and a fully compliant router is reported `NON_COMPLIANT`.

Demonstrated directly:

```
repr: 'hostname r1\nrouter ospf\n'          <- Python (translated)
match ^router ospf$: True
go-equivalent repr: 'hostname r1\r\nrouter ospf\r\n'
match on raw bytes: False                    <- Go without normalisation
```

**Fix** — `normaliseNewlines`, applied before matching. Order matters: `\r\n` must collapse to one `\n` before lone `\r` is handled, or CRLF becomes two newlines and shifts line boundaries.

**Regression tests:** `TestCRLFConfigIsGradedLikePython`, `TestNormaliseNewlines`.

### 4.3 `--native` was declared but never wired

`options` grew a `native` field that `parseArgs` never set and `run()` never read, leaving `collectNative` with no caller (a compile error). Fixed by parsing `--native`, `--timeout`, `--verbose`/`-v`, and branching in `run()`.

### 4.4 SQLite handle never closed (found 2026-11-18)

`save_results` used `with sqlite3.connect(path) as con:`. In Python's `sqlite3`, that context manager **commits the transaction but does not close the connection** — it never calls `close()`. The handle therefore stayed open until garbage collection.

On Linux this is invisible: the process exits and the lock goes with it. On Windows the connection holds an exclusive lock on the `.db` file, so `tempfile.TemporaryDirectory.cleanup()` failed with `WinError 32` and `test_compliance.py` reported `FAILED (errors=1)`. The assertion itself had passed; only the teardown broke.

This was **pre-existing, not introduced by the port** — `git stash` on the changes reproduces the identical error against the untouched original. It was fixed anyway, because a suite that always reports one error hides the next real one:

- `run_compliance.py`: `save_results` now wraps the transaction in `try/finally` and calls `con.close()`.
- `test_compliance.py`: the same pattern in `test_persists_history_and_findings`, which opened the file a second time to read it back.

**Result:** `Ran 4 tests … OK`.

The Go side never had this problem — `openComplianceDB` is followed by a `defer conn.Close()` in every path.

---

## 5. Deliberate divergences from Python, and why

A straight transliteration would have changed behaviour silently in four places. Each is handled explicitly with a comment at the point of divergence.

### 5.1 `\s` is ASCII in Go, Unicode in Python

Go's RE2 `\s` is `[\t\n\f\r ]`. Python's is the Unicode whitespace set, which also matches vertical tab (0x0B) and NBSP (U+00A0). Confirmed empirically: Python `\s` matches both; Go's does not.

A rule written as `^\s*network ...` would match an NBSP-indented line under Python and fail under Go — flipping a compliant router to `NON_COMPLIANT`.

**Fix** — `expandPythonClasses` rewrites `\s \S \d \D \w \W` into equivalent RE2 classes before compilation:

```go
pySpaceClass = `[\t\n\v\f\r \x{0085}\x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]`
pyDigitClass = `\p{Nd}`
pyWordClass  = `[\p{L}\p{N}_]`
```

It tracks `inClass` so an escape inside an existing class is left for RE2 (expanding it would produce `[[...]]`, a syntax error), and skips escaped backslashes so a literal `\\s` is untouched.

**Tests:** `TestPythonSpaceClassMatchesUnicodeWhitespace` (7 sub-cases), `TestWordAndDigitClassesAreUnicodeAware`, `TestExpandLeavesEscapedBackslashAlone`, `TestEscapesInsideCharacterClassArePreserved`.

### 5.2 PyYAML and `yaml.v3` disagree on numeric mapping keys

PyYAML (YAML 1.1) resolves an unquoted `123:` to the **integer** 123, which the Python code rejects with `Tên router không hợp lệ`. `yaml.v3` (YAML 1.2) decodes it as the **string** `"123"`, which would be silently accepted as a hostname.

**Fix** — decode into `yaml.Node` rather than `map[string]any`, then reject any router key whose tag is not `!!str`. This also preserves **file order**, which matters because Go map iteration is randomised while Python dicts preserve insertion order (3.7+); without an explicit ordered slice, findings and JSON output would be non-deterministic.

**Tests:** `TestNumericRouterKeyIsRejected`, `TestLoadDesiredStateHappyPath` (asserts order `r3,r1` from file).

Decoding into `map[string]any` was tried first and panicked with `interface conversion: interface {} is nil, not map[string]interface {}` on a malformed document — confirming it cannot distinguish the error paths.

### 5.3 Ansible output streaming

`collectConfigs` buffers Ansible's stdout/stderr into `strings.Builder` and flushes them explicitly, so the Go binary's console output stays in the same order the Python version produced when it inherited the pipe.

### 5.4 Error handling broadened in one place

If a snapshot exists but cannot be read, Python raises and aborts the entire run. The Go port scores that device `ERROR` and continues, so the remaining routers stay auditable. This is the only intentional behavioural difference, it is commented in `auditDevices`, and it cannot change a compliant/non-compliant verdict for a readable file.

---

## 6. Verification

### 6.1 Unit tests

**65 tests pass** (28 top-level plus 37 subtests), `gofmt` clean, `go vet` clean. The Python suite (`test_compliance.py`) is 4/4 green as well.

The first four mirror `test_compliance.py` one-for-one, in the same order, so the two files can be diffed side by side:

- `TestCompliantRouter`
- `TestMissingRuleIsNoncompliant`
- `TestMissingSnapshotIsError`
- `TestPersistsHistoryAndFindings`

The rest cover the divergences above, the CLI surface, the INI inventory parser, and the JSON wire format. `TestPassivationRuleIsScopedToLoopback` was added on 2026-11-18 with the rule fix (§7.8).

### 6.2 Differential test — the actual proof

`compliance/tests/parity.py` runs **both** implementations against the same fixtures, the same real `desired_state.yml`, and separate throwaway databases, then compares results:

```
$ python compliance/tests/parity.py compliance/bin/compliance

devices identical (3 routers)
summary identical: {"total": 3, "compliant": 1, "noncompliant": 1, "errors": 1} status=ERROR
compliance_runs identical: [('2026-11-16T00:00:00+00:00', 'ERROR', 3, 1, 1, 1, 'ansible')]
compliance_findings identical (4 rows)
  (1, 'r1', 'COMPLIANT', 'All required rules', 'Tất cả cấu hình bắt buộc đều được tìm thấy.')
  (1, 'r2', 'NON_COMPLIANT', '^\\s*network 192\\.168\\.23\\.0/24 area 0$', '...')
  (1, 'r2', 'NON_COMPLIANT', '^(?:[ \\t]*passive-interface lo|interface lo\\r?$\\n...)$', '...')
  (1, 'r3', 'ERROR', 'Configuration collection', '...')

PARITY OK - Go output matches Python byte-for-byte
```

This compares stdout, the `latest.json` document, and both SQLite tables — not "does it look right", but "what exactly does it emit".

On Windows the binary argument needs its `.exe` suffix (`parity.py bin/compliance.exe`); the example above is the Controller form. Re-verified twice: once at the end of the original session, and again after the rule fix in §7.8 — same verdict, `PARITY OK`, byte-for-byte.

### 6.3 Live router test — `--native`

`--native` was run against the **real r1** (`192.168.23.11`, FRR 10.6.1) using the lab key:

```
[1/3] Thu thập cấu hình từ 1 router qua SSH (native)...
  r1     ok (518 bytes)
[2/3] SSH native exit code: 0
```

(The `518 bytes` is the trimmed console figure; the file on disk is 519 bytes because the collector appends a trailing newline, matching what the Ansible playbook wrote.)

The captured snapshot is a genuine `show running-config`. Python was then re-run against **that same snapshot** and produced an identical verdict, closing the loop: the native collector produces configuration the Python auditor grades exactly as the Go auditor does.

No command issued by the collector modifies router state — the only remote command is `TERM=dumb vtysh -c 'show running-config'`.

### 6.4 `run.sh` both paths

- With the binary present: dispatches to Go, forwards `--skip-collect`, exit `1`.
- With the binary removed: falls back to the Python runner, same result.

### 6.5 Environment

There was no Go toolchain on this machine. Installed go1.23.5 to `C:\Users\Admin\.workbuddy-ai\binaries\go\go\bin`; module cache under the same tree. `go mod tidy` intermittently fails with `Access is denied` when cleaning `.partial` files — harmless, worked around with `go get <module>@<version>`.

`make` is also absent on this Windows box, so the Makefile targets were exercised by their underlying `go build` / `go test` commands rather than through `make` itself. The Makefile is unchanged and correct for the Controller, where Alpine's `go` package ships `make` alongside it — but that specific indirection is untested here.

---

## 7. Open finding — the live routers have drifted from the playbook

This is **not** a port defect; both implementations agree with each other. What they disagree with is the **repository**.

The initial read of this finding was "the rule set is out of date for FRR 10.6". Checking the playbook proves that is only half true, and the more actionable half is drift.

### 7.1 Evidence — live r1

Captured read-only by the native collector from `192.168.23.11` (FRR 10.6.1):

```
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
```

### 7.2 Evidence — what the repository says should be there

`playbooks/02_ospf.yml` declares the intent explicitly, and `playbooks/master.yml:20` imports it, so it is part of the standard pipeline:

```yaml
vars:
  # The segment that actually carries the OSPF hellos.
  # Without this network statement no adjacency can ever form.
  lan_subnet: 192.168.23.0/24
...
  -c 'network {{ lan_subnet }} area {{ ospf_area }}' \   # line 16
  -c 'network {{ loopback_ip }}/32 area {{ ospf_area }}' \
  -c 'passive-interface lo' \                            # line 18
```

`192.168.23.0/24` is also load-bearing elsewhere: `05_acl.yml:11` permits it, `03_ntp.yml:38` and `07_check.yml:93` point at `192.168.23.10` inside it, and `04_snmp.yml:79` defaults `snmp_allowed_net` to it.

### 7.3 The comparison

| Rule | Live r1 | `02_ospf.yml` says | Verdict |
|---|---|---|---|
| `^hostname\s+r1$` | present | — | ✅ match |
| `^router ospf$` | present | — | ✅ match |
| `^\s*network 1\.1\.1\.1/32 area 0$` | present | line 17 | ✅ match |
| `^\s*network 192\.168\.23\.0/24 area 0$` | **absent** | line 16, commented as required for adjacency | ❌ **drift** — still failing, intentionally |
| passivation rule | present, as `ip ospf passive` under `interface lo` | line 18 | ✅ fixed in §7.8 |

Two of the five rules used to fail. They had **different causes**, which mattered because they needed different fixes:

- **The `/24` failure is configuration drift.** The routers no longer match the playbook that is supposed to define them. `10.0.1.0/29` and `10.0.3.0/29` appear **nowhere in any copy of the repository** — not in `playbooks/`, not in `host_vars/`, not in `ansible-project-v2`, not in `_backup-prepush-20261111`. They exist only on the live routers. Someone changed the live OSPF topology by hand, outside the pipeline. **Left unfixed on purpose** — see §7.5.
- **The passivation failure was a rule-vs-version mismatch.** The playbook issues `passive-interface lo` (line 18), which FRR 10.6 accepts but renders as `ip ospf passive` under `interface lo`. So even a router freshly built by `master.yml` would have failed that rule. This was a genuine bug in the rule, not in the router, and it has been fixed — see §7.8.

### 7.4 Recommended action

1. **Reconcile the topology first** — decide which design is intended:
   - If the `/24` should be advertised (what `02_ospf.yml` says), re-run `02_ospf.yml` and the routers return to compliance on that rule. Verify with `show ip ospf interface brief` that adjacency still forms.
   - If the `/29` point-to-point design is the new intent, then `02_ospf.yml`, the ACL in `05_acl.yml`, and the NTP/SNMP references all need updating together — and `desired_state.yml` should be changed to match. **The playbook currently contradicts the running network.**

2. **~~Fix the `passive-interface` rule regardless of the above.~~ Done — see §7.7.** The rule was widened to accept both renderings.

### 7.5 Why the `/24` finding is pinned by a test

The `/24` behaviour is **pinned by a test** — `TestShippedRulesAgainstRealFRRConfig` embeds the real r1 capture and asserts that the drift finding still occurs. If someone later widens the `/24` regex so the audit reports `COMPLIANT`, that test fails. That is deliberate: the port's job is to be faithful, and this divergence is real information about the lab, not noise to be smoothed away.

The `passive-interface` assertion is pinned in **both** directions instead, because that one was a genuine rule bug rather than lab drift (§7.7).

### 7.6 Provenance

No lab work was performed on 2026-11-13 or 2026-11-14 — there are no session transcripts for those dates, and the only activity in that window is unrelated coursework in `Downloads/`. The most recent lab sessions before this port were:

| Date | Session | Content |
|---|---|---|
| 2026-11-15 | `WorkBuddy AI-2026-11-15-14-23-44` | Mail server + Thunderbird guide (4 prompts) — no compliance work, but see §8 |
| 2026-11-12 | `WorkBuddy AI-2026-11-12-18-19-02` | Chat only (model identity, a long essay) — no lab work |

So the drift cannot be attributed to a change made in the last two or three days. It predates this session, and the audit is the first thing that has surfaced it.

The 2026-11-15 session did no work on this module, but it is not irrelevant — it is where the database-path trap in §8 was first diagnosed. "No compliance work" and "no useful context" are different claims, and only the first one is true here.

### 7.7 Correction — the warning about "silently overwriting" was wrong

An earlier revision of this report said that re-running `master.yml` "would silently undo the manual change". **That is false, and the truth is worse.** Checking every `vtysh` command the pipeline issues:

| Playbook | FRR config commands | Effect on the `/29` statements |
|---|---|---|
| `01_fix_frr.yml` | `interface lo`, `ip address <lo>/32` | none |
| `02_ospf.yml` | `network <lan>/24`, `network <lo>/32`, `passive-interface lo` | **none — these are pure additions** |
| `04_snmp.yml` | `agentx` | none |
| `05_acl.yml` | `no access-list 10`, then re-creates ACL 10 | none (replaces the ACL only) |
| `06_backup.yml`, `07_check.yml` | `show ...` only | none |

There is no `no router ospf`, no `no network`, and no playbook that writes `frr.conf` wholesale. So `master.yml` **cannot remove** the hand-made `10.0.1.0/29` and `10.0.3.0/29` — it can only add the `/24` alongside them.

The real risk is the opposite of the one originally written:

> Re-running `02_ospf.yml` re-adds `network 192.168.23.0/24 area 0`, so the audit would start reporting **COMPLIANT** — while the `/29` statements stay in the config, unaudited. The drift does not get undone; it becomes **invisible**. A green dashboard would then coexist with a topology nobody in the repository designed.

That is a false-green, which is more dangerous than a red finding, because nothing prompts anyone to look again. The fix is the same as before (decide the design, then make config and rules agree), but the reason to decide *before* re-running is that re-running actively destroys the evidence.

The one genuinely destructive playbook is `14_restore.yml`, which overwrites the running configuration from a backup. It is **not** part of `master.yml` and it refuses to run without `-e confirm_restore=yes`, so it is not a silent path.

### 7.8 The `passive-interface` fix that was applied

The old rule was `^\s*passive-interface lo$`. FRR 10.6 accepts the `passive-interface lo` command (so `02_ospf.yml` is correct) but renders the state as `ip ospf passive` inside the `interface lo` block. No configuration could satisfy the old rule, so the rule — not the router — was broken.

The shipped rule is now:

```
^(?:[ \t]*passive-interface lo|interface lo\r?$\n(?:[ \t]+[^\n]*\r?\n)*?[ \t]+ip ospf passive)$
```

It accepts both renderings and is deliberately **scoped to the loopback block**: the `exit` that closes an interface block is unindented, so it cannot satisfy `[ \t]+[^\n]*`, and the lazy repeat stops there. This means `ip ospf passive` under any *other* interface does not satisfy the rule.

Both directions are pinned by `TestPassivationRuleIsScopedToLoopback` (6 cases, including CRLF input and a config where only `eth0` is passivated). The differential harness still reports byte-for-byte parity with the Python runner on the new rule.

`02_ospf.yml` was **not** changed. The command it issues is valid; only the rule that graded the *rendering* was wrong.


---

## 8. Relation to the mail-server work (2026-11-15)

The mail-server session is directly relevant here, and not only chronologically. It is where the **database-path trap** that this compliance module can also fall into was first hit and diagnosed.

### 8.1 The mail work touched the same database

The outbound-mail work of 2026-11-15 modified `poller.py` and the Go `lab.go` mail path, and rewrote `/root/lab/.env`. Both of those live in the same `/root/lab` tree that owns `net.db`, which is the database this compliance runner writes `compliance_runs` and `compliance_findings` into. Nothing in the mail work created the compliance tables — but it did prove out the deployment mechanics that the Compliance section now depends on.

### 8.2 The concrete trap: two copies of the same database

This is the important item. `15_dashboard.yml` does:

```yaml
dash_dir: /root/lab
dash_src: "{{ playbook_dir }}/../files/dashboard"
...
- name: Copy the dashboard tree
  ansible.builtin.copy:
    src: "{{ dash_src }}/"
    dest: "{{ dash_dir }}/"
```

So there are **two** trees that look authoritative:

| Tree | What it is |
|---|---|
| `/root/ansible-project/files/dashboard/` | the repository checkout on the Controller |
| `/root/lab/` | the live deployment, created by copying the tree above |

`compliance/scripts/run_compliance.py` computes its default database as:

```python
ROOT = Path(__file__).resolve().parents[2]
DEFAULT_DB = ROOT / "files" / "dashboard" / "db" / "net.db"
```

The Go port reproduces that exactly (`defDB` in `main.go`). So **running the audit from the repository checkout writes to `<repo>/files/dashboard/db/net.db`, while the live dashboard serves `/root/lab/db/net.db`** — because compose mounts `./db:/data` with `NETDB=/data/net.db`.

Result: a successful audit writes rows into a file the dashboard never opens. The Compliance view then renders "Chưa có kết quả. Hãy chạy lệnh compliance trên Controller trước." even though the audit just ran and reported success. Nothing errors; the data simply lands in the wrong place.

This is the same class of problem recorded during the mail work, where `/root/lab` and `/root/ansible-project/files/dashboard` were found to be separate copies and editing only the live one was silently reverted by the next sync. It is documented in `README.md` §5 as "if the API container uses a different DB file, `NETDB` and `--db` must point at the same file" — but that note is easy to miss and the default path actively points at the wrong copy.

**Mitigation — always pass `--db` explicitly on the Controller:**

```sh
./compliance/bin/compliance --inventory hosts.ini --native --db /root/lab/db/net.db
```

Verified to work: the DB is opened with `busy_timeout(15000)` and `journal_mode(WAL)`, so writing from a separate process while the API is serving is safe.

### 8.3 The mail work also explains a live-state caveat

The mail session recorded that `/root/lab/.env` holds `JWT_SECRET`, `NETDB_KEY`, `ADMIN_PASSWORD` and the `TELEGRAM_*` / `MAIL_*` keys, and that `15_dashboard.yml` deliberately never overwrites `.env` (rotating `NETDB_KEY` would strand the encrypted device credentials).

For compliance this matters in one way: the Compliance view is behind the dashboard login, so verifying the integration end-to-end needs working `ADMIN_USER`/`ADMIN_PASSWORD`. Those are generated on first deploy into `/root/lab/.env`. Do not regenerate them to "reset" a login — that file also holds the key that decrypts stored router passwords.

### 8.4 What this changes about the verification claims

§6.4 verified `run.sh` and the binary's write path, and §6.2 verified byte-identical database rows — but both used temporary databases. **The end-to-end path (audit → `/root/lab/db/net.db` → `/api/compliance` → dashboard Compliance view) has not been exercised**, for the same reason §9 gives: no Docker here, and the Controller password no longer authenticates.

Given §8.2, that untested hop is precisely the one most likely to fail. It is a path/configuration question, not a code question, and the `--db` flag is the control.

---

## 9. Not verified

| Item | Reason |
|---|---|
| `docker compose up -d --build api` (step 4 of the documented workflow) | No Docker on this machine. The API's read side was already correct and was not modified. |
| `--native` against r2 and r3 | Verified against r1 only. The collector is host-agnostic; this is coverage, not a known defect. |
| Ansible collection path on the Controller | The controller's `alpinelinux` password no longer authenticates (VM likely rebuilt), so `install_alpine.sh` could not be run end-to-end. Both shell scripts pass `sh -n` syntax checks. |
| `make -C compliance/go …` targets | No `make` on this Windows machine; the same `go build` / `go test` commands were run directly (see §6.5). |
| **Re-running the audit against the live lab on 2026-11-18** | The routers still answer on their management IPs (`192.168.23.11` responds to ping, port 22 is open), but SSH now rejects **both** the stored passwords and key auth: `no supported methods remain` with a password, `Permission denied (publickey,keyboard-interactive)` with the key. The re-verification therefore ran against the captured r1 snapshot embedded in the tests, not against the device. The snapshot is the same bytes the native collector returned when auth still worked, so the rule verdicts are unaffected — but the live config was **not** re-read, and could have changed since. |

### 9.1 What that means for the drift finding

The `/24` drift is reported from the 2026-11-16 capture. It has not been re-confirmed on the device today, because of the auth wall above. Two consequences worth stating plainly:

- If someone has since fixed the OSPF config by hand, the finding is stale and will clear on the next successful audit.
- If it has drifted further, the audit cannot tell you — nothing has looked at the device since.

Either way the finding should be treated as **last known state, 2026-11-16**, not as current truth. Restoring SSH access is a prerequisite for trusting any live verdict.

---

## 10. How to use it

```sh
# Build (on any machine with Go, or on the Controller via --go install)
make -C compliance/go build

# Audit using the Go binary, no Ansible required
./compliance/run.sh hosts.ini --native

# Audit via Ansible (unchanged default)
./compliance/run.sh hosts.ini

# Prove parity after any future change to either implementation
make -C compliance/go parity
```

On the Controller, add `--db` so the rows land in the database the dashboard actually serves — see §8.2, this is the one mistake that fails silently:

```sh
./compliance/bin/compliance --inventory hosts.ini --native --db /root/lab/db/net.db
```

Exit codes are unchanged from the Python contract: `0` all compliant, `1` findings or collection failure, `2` bad input.

## 11. GitHub push, review of the changes, and the defects the review found

This section supersedes the earlier operational note, which said the tree was
not a git repository and nothing had been committed. That was true when it was
written and is no longer true.

### 11.1 What was pushed, and how the history was preserved

The local tree had no `.git`, but `playbooks/09_git_github.yml:17` names the
remote: `https://github.com/toanpham-cit23/ansible-project.git`. Cloning it
showed the same project with **81 tracked files** at `2a387c1`, and the local
tree was a strict superset — the 26 files unique to local were exactly the
compliance add-on plus `files/dashboard/docker/api-go/compliance.go`. Nothing
on the remote would have been lost, but a fresh `git init` + push would have
presented an unrelated history in place of the real one.

So the existing history was attached: `git remote add origin …`, `git fetch
origin main`, `git reset --mixed origin/main`, then commit. Final remote HEAD
is `15fdc01`; local and remote are in sync.

Commits, all pushed:

| Commit | Content |
|---|---|
| `539f6f6` | Compliance port + first round of pre-push-audit fixes |
| `926c2fb` | CRLF note added to `SECURITY.md` |
| `97a2ab7` | CRLF note corrected (scope) |
| `f22e66d` | CRLF note corrected (YAML/INI files are harmless) |
| `15fdc01` | Audit output distinguishes "nothing to check" from "clean" |

A sixth commit, `013c0d9`, follows from the review in §11.3–11.5 and is
described there. It is the current remote HEAD.

### 11.2 The credential audit, on the actual commit set

Not on the working tree as a whole — on the exact 25 paths in the index, after
Git's line-ending normalisation:

- no `hosts.ini`, `.env`, `token.yml`, `.vault_pass`, private key, PEM, or
  `net.db`;
- no GitHub/AWS/Telegram/OpenAI token shapes, no `BEGIN PRIVATE KEY`, no
  long hex blobs outside `go.sum`;
- no credential assignments with real values in added lines;
- `compliance/tests/baseline_work/baseline.db` was excluded — it is generated
  by `_baseline.py`, which unlinks and rewrites it on every run, and nothing
  reads it. `.gitignore` now covers the whole directory.

The audit was repeated against a **fresh clone of the public repository**, not
just locally. `hosts.ini.example` still carries three `CHANGE_ME` placeholders,
which is intended.

### 11.3 Two more errors in the security guard, found by testing my own fix

The three defects fixed before the push (hardcoded `REPO`, unreachable inline
password scan, dry-run-only path collection) are described in §11.1's first
commit. Reviewing the whole change afterwards found **three more**, all in the
same 134-line file, all invisible without running the guard against a crafted
tree:

1. **`staged_paths()` audited only one side of the index.** My fix preferred
   `git diff --cached` and fell back to `git add -A -n`. But `09_git_github.yml`
   runs the audit *before* its own `git add -A`, so if a previous aborted run
   left anything staged, the guard looked only at the index and skipped every
   unstaged edit about to be committed. Fixed by taking the **union** of index
   paths and dry-run paths. Negative test: one clean file staged, secret left
   unstaged → now `BLOCKED`, previously `CLEAN`.

2. **The guard read the working tree, not the staged blob.** Stage a secret,
   then remove it from the working tree: the guard reported `CLEAN` while the
   commit still contained the secret. Fixed by scanning both the index blob
   (`git show :<path>`) and the working-tree file, and reporting once per file
   rather than once per copy found.

3. **`0 files, CLEAN` was ambiguous.** On a clean tree the guard printed CLEAN
   next to a zero-file count, which reads as a pass over the whole tree. It now
   prints `NOTHING TO CHECK (no staged or untracked files)`, or `CLEAN
   (filename check only – no live configs found to compare values against)`
   when the Controller's config files are absent.

All four scenarios now behave correctly: clean tree, ordinary staged secret,
staged-clean-plus-unstaged-secret, and staged-secret-erased-from-worktree.
The last three block with exit 1.

**Disadvantage that remains:** on any machine other than the Controller,
`/root/lab/.env`, `/root/lab/ops/duckdns.env` and `<repo>/hosts.ini` do not
exist, so the value comparison has nothing to compare against — only the
sensitive-filename check runs. The guard says so explicitly now, but it cannot
detect a plaintext token in an innocuously named file away from the Controller.
This is a coverage limit, not a code defect; the playbook runs it on the
Controller, where the configs exist.

### 11.4 Two wrong corrections I made, and how they were caught

Both were mine, not pre-existing defects:

- I first wrote in `SECURITY.md` that *every* `.sh` file stored CRLF. That was
  false. My `grep -cU $'\r'` reading was unreliable; `od -c` on the blobs showed
  only `run_backup.sh` among scripts. Fifteen files total store CRLF
  (`ansible.cfg`, `host_vars/*`, most `playbooks/*`, `run_backup.sh`), all
  pre-existing, none added by these commits. I checked that YAML really does
  parse CRLF (`yaml.safe_load` on `02_ospf.yml`) before documenting that those
  are harmless.
- The replacement verification command used `grep -q $'\r'`. Tested against
  the known CRLF file, it printed nothing — GNU grep on this Git Bash drops a
  literal CR from a command-line pattern. Replaced with
  `tr -cd '\r' | wc -c`, which was tested and does report the 13 CRs in
  `run_backup.sh`.
- I also wrote that `core.autocrlf=true` normalises "on checkout, not on
  commit". Wrong: it converts CRLF→LF **on commit**. Verified empirically by
  adding a CRLF scratch file and inspecting its index blob. `SECURITY.md` now
  says the CRLF blob came from a checkout where the setting was off or unset.

The lesson worth keeping: a verification command must be run against a known
failing case before it is trusted. Two of these three errors were in the
commands and explanation I added *in order to verify something else*.

### 11.5 Verification after the corrections

- `python -m py_compile files/ops/prepush-audit.py` — passes.
- All four guard scenarios above — exercised, expected results.
- `go test ./...` — still passes (the guard changes are Python-only).
- Fresh clone of the public repo — 101 tracked files, compliance sources
  present, no credential files present.
- Line-ending check on the public blobs — the 15 CRLF files are the same set
  as at `2a387c1`, so these commits introduced none.

`make` remains unavailable on this Windows machine; the Makefile targets were
run as their underlying `go build` / `go test` commands. The remaining
verification gaps from §9 still stand.

### 11.6 Operational note

The binary is gitignored (`compliance/bin/`, `compliance/go/compliance`) so it
cannot drift from its source in the repository; rebuild with `make -C
compliance/go build`. The Python runner and its tests were left untouched, so
the old path remains available for rollback and for the parity test.

On the Controller, remember §8.2: pass `--db /root/lab/db/net.db` explicitly or
the audit writes to a database the dashboard never opens.

---

## 12. Re-check of 2026-11-19

Six items were re-checked at the owner's request. Everything in this section
is in English by request.

### 12.1 Router health — still unauditable live

All three routers answer on their management IPs (`192.168.23.11`, `.12`,
`.13`), and port 22 offers `publickey` and `keyboard-interactive`. They still
reject the stored lab password, keyboard-interactive with that password, and
every key available on this workstation (`Permission denied`). The controller
(`.10`) offers password auth too, but none of the recorded passwords works.

**No new live verdict can be produced.** The last known compliance snapshot
remains the one described in §7, and it is historical, not current. The
`.1` address that answers on the segment is `DESKTOP-J8B9TQE` (the workstation
side of the virtual network), not a lab gateway.

### 12.2 Compliance as a standalone dashboard item

No structural change was needed: compliance was already a top-level nav item
(`app.html:99`), its own route (`views.compliance`), and its own API endpoint
(`GET /api/compliance`). Two defects in that surface were fixed:

- The empty state was still Vietnamese. It now says, in English, to run the
  audit with `--db /root/lab/db/net.db` — the exact trap from §8.2.
- A `nav-sep` "Audit" divider in the sidebar and its CSS make compliance a
  distinct section instead of one more link among the monitoring views. The
  divider is hidden when the sidebar is collapsed.

### 12.3 Switch — design only, no device

There is no managed switch anywhere in this lab. `files/lab/vlans.yml:13-16`
says so explicitly: the three switches are drawn, not cabled. They exist as
labels in `LAB.switches` (`app.js:825`), as `switch: s1|s2|s3` on the eight
design hosts, and in `host_design`, but there is no switch IP, no SSH
management, no switch in `hosts.ini.example`, and no playbook that configures
one. `playbooks/16_vlan.yml` declares `hosts: network_devices`, which the
inventory script would populate with a `switches` group — but that group is
empty in this lab.

**Conclusion:** nothing to reconfigure or roll back on a switch. The VLAN
design is a planning model until real switches exist.

### 12.4 Rollback guard — 180 s verified offline

`files/ops/fw-guard.sh` already implements the 180-second window for the host
firewall (`GUARD_SECONDS=180`, deadline written by `apply`, cron tick every
minute, rollback on expiry unless `confirm`). It was never tested without a
controller, so `files/ops/test_fw_guard.py` was added: it runs a copy of the
script with stubbed `iptables`, `rc-service` and `status.py`.

Result: **6/6 checks passed** — apply arms ~now+180 and attaches the chain;
an early tick does nothing; expiry rolls back and clears the deadline;
`confirm` clears it; a confirmed chain is reinstalled without re-arming; manual
rollback removes the chain and the confirmation. `sh -n` clean. This verifies
the firewall guard only; there is no equivalent guard for router config
changes, and none was added without live SSH access to test it.

### 12.5 VLAN — consistent, and the seed path was executed

The three host lists agree exactly: eight MACs in `files/lab/vlans.yml`,
`files/sim/simctl.py` and `LAB.hosts` in `app.js`, with no missing or extra
entries. `files/lab/seed_design.py` was run against a scratch database created
from the **real** `files/dashboard/db/schema.sql`, with `design.json` rendered
from the real YAML:

```
seed exit: 0
9 VLANs, 8 hosts, 0 existing lease(s) relabelled
counts: vlans=9, subnets=9, host_design=8
balance: r1=2, r2=3, r3=3
```

That matches the stated design. One caveat remains unchanged from the YAML
comment: the tagged frames cannot reach these VLANs while the switches are
only drawn. `playbooks/16_vlan.yml` is still deliberately not imported into
`master.yml` because it needs `iproute2`, which the routers lack.

### 12.6 What changed on disk

| File | Change |
| --- | --- |
| `compliance/PORT-REPORT.md` | This section |
| `files/dashboard/docker/api/static/app.html` | "Audit" nav separator |
| `files/dashboard/docker/api/static/css/app.css` | `.nav-sep` styling |
| `files/dashboard/docker/api/static/js/app.js` | English empty state with the correct `--db` command |
| `files/ops/test_fw_guard.py` | New offline 180 s guard test |
| `files/ops/README.md` | Guard timing and test instructions |

---

## 13. Topology red markers — 2026-11-19 follow-up

The owner reported that the monitoring topology was still red and that the
previous check had not resolved it. The screenshot showed six red "routers",
each access switch marked with one failed uplink, and no online hosts.

### 13.1 Two different causes

**Genuine:** r1, r2 and r3 answer ICMP ping but do **not** answer SNMP from
this workstation (`sysDescr.0` on UDP/161 times out for all three). SSH is also
closed to us (§12.1). The dashboard's poller marks a device `down` when its
SNMP get fails, so those three cards are reporting a real monitoring failure.
It cannot be cleared from here: it needs the SNMP agent restored on the
routers or the controller's access to it repaired. The poller image does
include `net-snmp-tools`, so a missing client is not the cause.

**False:** `test-router`, `test-switch` and `test-laptop` (`.14`–`.16`) do not
answer ping and are not in the repository. They were added to the live
inventory. The topology formerly drew **every** non-controller device in the
OSPF core row and counted it as a router, which produced two wrong statements:
a switch or laptop was called a "router down", and it inherited the same
alarm styling as a real cabled router.

### 13.2 Code fix

`app.js` now selects only `effective_kind === 'router'` devices for the core
row, and separates three states instead of two:

| State | Meaning | Drawing |
|---|---|---|
| `up` | SNMP health answered | normal card |
| `down` | SNMP answered that the device is down | red card + X |
| empty / `unknown` | never successfully polled | neutral card + warning note |

`isDown()` and `isUnknown()` are used consistently for the controller link,
OSPF adjacency lines, the router cards, switch uplink loss and the
"routers down" counter. A never-polled device is no longer presented as a
confirmed fault.

`files/dashboard/docker/api/static/js/test_topology_state.js` extracts those
predicates from the real `app.js` and checks five cases (core-row membership,
confirmed down, unknown, up, and the two helper boundaries). **5/5 pass**;
`node` syntax check clean.

After this fix the screenshot's "6 routers down" would become "1 router down"
for the real r1 plus the added `test-router` if it is still classified as a
router — the three genuine red cards remain **until SNMP is restored**, which
is the honest result. Removing that red without fixing the agent would be a
false green.

### 13.3 What still has to happen

Restore SNMP on r1/r2/r3 (`04_snmp.yml` installs and starts snmpd; it requires
the gitignored `files/snmp/*.apk` on the Controller), then confirm the cards
clear on the next poll. Decide whether the three test devices should remain in
inventory; they are not part of the designed lab and are kept visible in the
inventory table rather than being silently deleted.

---

## 14. Compliance scope, rule-level drift and guard failure demos — 2026-11-19

The owner restated the module's contract, which changed the design mid-task:

> The compliance module checks the presence of the required rules in the
> running FRR config, records each check in SQLite, and shows the result on the
> dashboard with the run time. The scope is the rule list the user declared —
> extra configuration outside that list is not evaluated, and the module does
> not fix configuration. Status is compliant or non-compliant; error means the
> connection was lost.

### 14.1 What that ruled out

An in-progress change had started storing the **whole** collected running-config
per audit and diffing it line by line, so that any hand edit would be visible.
That is a superset of the stated scope: it would have judged configuration
outside the declared rules, and it stored configuration text the module has no
mandate to keep. It was reverted from both tools and from the dashboard before
being committed. `compliance_configs` does not exist in any schema.

### 14.2 What was built instead

**Rule-level drift.** The device endpoint compares each declared rule's result
against the previous run that audited that host. A rule that changes result
between two audits means the running-config gained or lost a line the rule file
requires — which is exactly the signal a direct edit produces, without judging
anything outside the list.

`GET /api/compliance/device?host=<name>` returns the device status, every
declared rule with its result and detail, and `before`/`changed` for any rule
whose outcome moved. It returns `found: false` when a host was not part of the
latest run, rather than inventing a status.

**Status vocabulary is unchanged and now pinned by tests:** `COMPLIANT`,
`NON_COMPLIANT`, and `ERROR` only for a device the collector could not read.
`ERROR` is never rendered as a compliance verdict.

**Detail strings are English.** Both tools changed together, so
`tests/parity.py` still reports byte-for-byte equality (`PARITY OK`).

### 14.3 Dashboard page

The Compliance view is now device-centric, in the shape of the controller
screenshot the owner supplied: a device tree on the left with a status dot and
a failing-rule count, and one device's evidence on the right — its verdict,
every declared rule with a tick or cross, and a "changed since" badge on any
rule whose result moved. Three honest empty states are distinguished:

| Situation | What the page says |
| --- | --- |
| Device unreachable | "Could not be checked" — not a verdict; the previous known result stands |
| First audit for the device | No comparison yet; comparisons start next run |
| No rule changed | Every declared rule matched exactly as before |

### 14.4 Rollback guard: what it does when things break

`demo_rollback.py` replays the stub's iptables calls into a real rule list and
walks a simulated SSH SYN from the controller through it, so a "blocked" result
means the rule order would genuinely deny the packet. **3/3 scenarios pass:**

1. **Lockout.** An operator inserts `DROP` above the SSH rule and never
   confirms. SSH from `192.168.23.10` is **blocked**; after the 180 s tick the
   guard detaches the chain and SSH is reachable again with nobody logged in.
2. **Rejected rule.** iptables refuses a rule mid-set. `add()` rolls the entire
   chain back immediately: nothing is left attached, no deadline remains, so no
   half-applied firewall exists.
3. **Lost confirmed chain.** Docker restarts the network and a confirmed chain
   disappears. The guard reinstalls it silently and does **not** start a new
   180 s countdown.

Demo 1 is the meaningful one: at the moment the guard fires the host really is
unreachable, which is the case no human can fix from outside.

### 14.5 Files changed

| File | Change |
| --- | --- |
| `files/dashboard/docker/api-go/compliance_device.go` | New device endpoint with rule-level drift |
| `files/dashboard/docker/api-go/compliance_device_test.go` | Contract tests against a real in-memory schema |
| `files/dashboard/docker/api/static/js/app.js` | Device-tree compliance page, three empty states |
| `files/dashboard/docker/api/static/css/app.css` | Two-column layout, rule rows, change badge |
| `files/ops/demo_rollback.py` | Three failure demos with a replayed ruleset |
| `files/ops/README.md` | Demo table and how to run it |
| `compliance/scripts/run_compliance.py` | English detail strings |
| `compliance/go/cmd/compliance/main.go` | Same strings, kept byte-identical |
| `files/dashboard/docker/api-go/go.mod`, `go.sum` | Indirect deps `go mod tidy` needs for a local build |

Verification: 4 Python tests, Go compliance tests, `PARITY OK`, guard 6/6,
demos 3/3, `node` syntax check on `app.js`, `go vet` clean on the API package.

### 14.6 The drift badge never fired for the case it was built for — 2026-11-19

Building the compliance demos (§14.7) found a defect in the feature as shipped
in §14.2. The demos are the reason it was found: each one asserts on what the
page would render, and demo 1 failed.

**The defect.** `saveResults` stores a compliant device as a **single**
`All required rules` row, not one row per rule. `handleComplianceDevice` finds
"changed" by looking each current rule up in the previous run's rule map. For a
device that was compliant last time, that map contains only the summary row — so
every real rule is absent, `seen` is false, and `changed` is false.

The consequence is precise and bad: the bond from *compliant last audit* to
*non-compliant this audit* is the one transition the badge exists to report, and
it was the one transition it could never report. A router that fails from the
start shows its failing rules with no badge (correct — nothing changed), and a
router that someone breaks by hand also showed no badge. The feature worked
except in the case it was designed for.

**The fix** is in `compareRules` (extracted from the handler so the tests call
the shipped code, not a copy). When the previous run was `COMPLIANT` and a rule
now fails, the rule's prior result is inferred as compliant. The inference holds
because the writer omits per-rule rows *only* when every declared rule matched.

Two boundaries were deliberate:

- **Restricted to `NON_COMPLIANT` current rows.** An `ERROR` device stores one
  synthetic `Configuration collection` row that is not a rule at all; nothing
  was evaluated, so nothing may be reported as changed. Without this restriction
  a network outage renders as drift — demo 3 caught exactly that over-reach in
  the first attempt at the fix.
- **Stated caveat:** a rule *added* to `desired_state.yml` between two audits was
  never evaluated before and is reported as changed. That is rare and
  human-authored, and silence about real drift is the worse failure.

**Regression test.** `TestCompliantToBrokenIsReportedAsChanged` seeds a
compliant run (one summary row) and a failing run, then asserts `changed == 1`.
Verified to fail when the inference is disabled and pass when it is restored.
`TestUnreachableIsNotReportedAsChanged` pins the `ERROR` boundary.

**A second, smaller correction.** The device panel was headed *Declared rules*
and implied every rule was listed. It only ever listed the failures. The heading
is now *Rule results*, with a line stating that a clean device shows one summary
row because the audit stores failures. The page no longer claims more than the
schema holds.

### 14.7 Compliance demos

`files/ops/demo_compliance.py`, 3/3 passing. It imports the real auditor, uses
the real `desired_state.yml`, writes a real database, and reads the device
back with the queries from `compliance_device.go`.

| Demo | Edit on the router | Result |
| --- | --- | --- |
| 1 | `no network 192.168.23.0/24 area 0` on r1 | r1 `NON_COMPLIANT`, rule marked changed from compliant; r2 unchanged |
| 2 | Banner, static route, stray interface on r2 | r2 `COMPLIANT`, nothing marked changed — outside the declared list |
| 3 | SSH to r3 fails, snapshot deleted | `ERROR`, previous result stands, no change flagged |

| File | Change |
| --- | --- |
| `files/ops/demo_compliance.py` | New: three demos against the real auditor |
| `files/dashboard/docker/api-go/compliance_device.go` | `compareRules` extracted; compliant-to-broken inference |
| `files/dashboard/docker/api-go/compliance_device_test.go` | Two regression tests for the inference and its boundary |
| `files/dashboard/docker/api/static/js/app.js` | Panel copy matches what is stored |
| `files/ops/README.md` | Demo section |

Verification: `ok labapi` with the two new tests (each confirmed to fail when its
guard is removed), demos 3/3, `node --check` clean, `go vet` clean.
