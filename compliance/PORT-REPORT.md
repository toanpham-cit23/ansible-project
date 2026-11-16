# Compliance Runner — Python → Go Port Report

**Date:** 2026-11-16
**Project:** `ansible-project-compliance-final/ansible-project-main` — `compliance/` add-on
**Task:** Convert `compliance/scripts/run_compliance.py` to Go; review, verify, implement.
**Result:** Port complete and **behaviourally identical**. A differential test proves the Go binary reproduces the Python runner's output byte-for-byte, including the JSON report and both SQLite tables.

**Incidental discovery:** the audit surfaced a **drift between the live routers and `playbooks/02_ospf.yml`** — the running OSPF topology no longer matches the repository. See §7; this is an operational finding, not a port defect, and it is the most actionable thing in this report.

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

**59 tests pass**, `gofmt` clean, `go vet` clean.

The first four mirror `test_compliance.py` one-for-one, in the same order, so the two files can be diffed side by side:

- `TestCompliantRouter`
- `TestMissingRuleIsNoncompliant`
- `TestMissingSnapshotIsError`
- `TestPersistsHistoryAndFindings`

The rest cover the divergences above, the CLI surface, the INI inventory parser, and the JSON wire format.

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
  (1, 'r2', 'NON_COMPLIANT', '^\\s*passive-interface lo$', '...')
  (1, 'r3', 'ERROR', 'Configuration collection', '...')

PARITY OK - Go output matches Python byte-for-byte
```

This compares stdout, the `latest.json` document, and both SQLite tables — not "does it look right", but "what exactly does it emit".

On Windows the binary argument needs its `.exe` suffix (`parity.py bin/compliance.exe`); the example above is the Controller form. Re-verified at the end of this session after the report was written — same output, `PARITY OK`.

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
| `^\s*network 192\.168\.23\.0/24 area 0$` | **absent** | line 16, commented as required for adjacency | ❌ **drift** |
| `^\s*passive-interface lo$` | **absent** (rendered as `ip ospf passive` under `interface lo`) | line 18 | ⚠️ **rule syntax vs FRR 10.6** |

So the two failures have **different causes**, which matters because they need different fixes:

- **The `/24` failure is configuration drift.** The routers no longer match the playbook that is supposed to define them. `10.0.1.0/29` and `10.0.3.0/29` appear **nowhere in any copy of the repository** — not in `playbooks/`, not in `host_vars/`, not in `ansible-project-v2`, not in `_backup-prepush-20261111`. They exist only on the live routers. Someone changed the live OSPF topology by hand, outside the pipeline.
- **The `passive-interface` failure is a rule-vs-version mismatch.** The playbook issues `passive-interface lo` (line 18), which FRR 10.6 accepts but renders as `ip ospf passive` under `interface lo`. So even a router freshly built by `master.yml` would fail this rule. This is a genuine bug in the rule, not in the router.

### 7.4 Recommended action

1. **Reconcile the topology first** — decide which design is intended:
   - If the `/24` should be advertised (what `02_ospf.yml` says), re-run `02_ospf.yml` and the routers return to compliance on that rule. Verify with `show ip ospf interface brief` that adjacency still forms.
   - If the `/29` point-to-point design is the new intent, then `02_ospf.yml`, the ACL in `05_acl.yml`, and the NTP/SNMP references all need updating together — and `desired_state.yml` should be changed to match. **The playbook currently contradicts the running network**, which means the next `master.yml` run would silently undo the manual change.

2. **Fix the `passive-interface` rule regardless of the above.** It cannot pass against FRR 10.6 in its current form. Either:
   - match the rendered form: `^\s*ip ospf passive$`, accepting that a plain regex cannot express "inside the `interface lo` block"; or
   - make `02_ospf.yml` emit the per-interface form so config and rule agree.

Neither change was made here. Modifying the lab's routing design, or the rules that grade it, is a decision for the owner — silently making the audit say `COMPLIANT` would hide a real inconsistency between repository and network.

### 7.5 Why this was not "fixed" in the port

The current behaviour is **pinned by a test** — `TestShippedRulesAgainstRealFRRConfig` embeds the real r1 capture and asserts that these two findings occur. If someone later "fixes" the Go regexes so the audit reports `COMPLIANT`, that test fails. That is deliberate: the port's job is to be faithful, and this divergence is real information about the lab, not noise to be smoothed away.

### 7.6 Provenance

No lab work was performed on 2026-11-13 or 2026-11-14 — there are no session transcripts for those dates, and the only activity in that window is unrelated coursework in `Downloads/`. The most recent lab sessions before this port were:

| Date | Session | Content |
|---|---|---|
| 2026-11-15 | `WorkBuddy AI-2026-11-15-14-23-44` | Mail server + Thunderbird guide (4 prompts) — no compliance work, but see §8 |
| 2026-11-12 | `WorkBuddy AI-2026-11-12-18-19-02` | Chat only (model identity, a long essay) — no lab work |

So the drift cannot be attributed to a change made in the last two or three days. It predates this session, and the audit is the first thing that has surfaced it.

The 2026-11-15 session did no work on this module, but it is not irrelevant — it is where the database-path trap in §8 was first diagnosed. "No compliance work" and "no useful context" are different claims, and only the first one is true here.


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
