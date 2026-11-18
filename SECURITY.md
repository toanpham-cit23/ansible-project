# Security notes

This repository is a teaching lab, not a production system. That is the
reason for the shortcuts below, not a justification for them.

## Exposed credentials

The following values were committed to a **public** repository and should be
treated as compromised. Untracking them does not make them secret again —
they remain readable in the git history and in any existing fork.

| Credential | Where it was exposed | Since |
| --- | --- | --- |
| Router SSH password for `r1`, `r2`, `r3` (user `root`) | `hosts.ini`, commit `eb7f929` | 2026-10-16 |
| Seeded device passwords `r1`, `r2`, `r3`, `alpinelinux` | `files/dashboard/setup.sh`, commit `5aa5b71` | 2026-10-28 |
| SNMP community string `public` | `playbooks/04_snmp.yml` | 2026-10-16 |

`hosts.ini` was removed from tracking in `6394969` and is now gitignored.
`hosts.ini.example` is the committed template; it carries `CHANGE_ME` and
holds no usable value.

**Status checked 2026-11-18.** The repository is still **public** (verified by
an unauthenticated `git ls-remote` succeeding), so everything above remains
world-readable regardless of untracking.

### What still has to happen

Rotation is the only remediation that matters, and it happens outside this
repository:

- [x] **`root` SSH password on r1, r2, r3 — mitigated.** Password
      authentication is disabled on **all three** routers: `.11`, `.12` and
      `.13` each offer only `publickey, keyboard-interactive`, so the leaked
      value cannot be used to log in anywhere.
      Disabling password auth is a *control*, not a rotation: the leaked string
      is still the account's password, and it becomes usable again the moment
      password auth is re-enabled on any host.
- [ ] Change the seeded dashboard device passwords, or re-run `setup.sh`
      with `DEV_PW_1`, `DEV_PW_2`, `DEV_PW_3` and `DEV_PW_10` set.
- [ ] Change the SNMP community string and pass it with
      `-e snmp_community=...`, or use SNMPv3 with auth and priv. The default
      `'public'` is still the fallback in `playbooks/04_snmp.yml:79`.
- [ ] Rotate `ADMIN_PASSWORD` in the dashboard `.env`.

Until those are done, the remaining credentials in the history are live.

### Audit performed 2026-11-18

| Scope | Method | Result |
| --- | --- | --- |
| Current tree (102 tracked files) | Pattern scan: GitHub/OpenAI/AWS/Slack/JWT/Telegram/Google key shapes, PEM headers, inline `ansible_password`, creds-in-URL | No live secret. The only matches are `CHANGE_ME` placeholders (`hosts.ini.example`, `main_test.go`), a unit-test token (`telegram_auth_test.go`), and `${DUCKDNS_TOKEN:-…}` indirection |
| All 474 historical text blobs | Same patterns plus `JWT_SECRET`/`NETDB_KEY`/`ADMIN_PASSWORD`/`TELEGRAM_BOT_TOKEN` assignments | Clean |
| Files that must never be committed | `git log --all --pretty=format: --name-only` for `.env`, `net.db`, `*.pem`, `*.key`, `vault_pass`, `token.yml`, `id_rsa` | **Never committed at any point** |
| `hosts.ini` exposure window | `git rev-list HEAD` ∩ blobs containing the path | 17 of 47 commits, **all ancestors of** `6394969`. No commit after the removal carries the file |
| Pushed binaries | `git ls-files` for `*.exe`, `compliance/bin/` | None tracked |

One correction to an earlier claim: the 2-character passwords in `hosts.ini`
(`r1`/`r2`/`r3`, identical to the hostnames) are confirmed **unchanged** — the
historical blob and the current lab inventory hash identically. They are not
merely old, and that is why disabling password auth mattered more than
untracking the file did.

Also worth stating plainly: **the `/24`-vs-`/29` OSPF drift recorded in
`compliance/PORT-REPORT.md` §7 could not be re-confirmed on 2026-11-18.** The
routers were unreachable for audit because key-based auth also fails
(`Permission denied (publickey,keyboard-interactive)`), so only password auth
is known to be closed. Restoring key access is what the compliance audit
depends on; without it every verdict is historical.

### What was not done, and why

History was **not** rewritten. `git filter-repo` or BFG would remove
`hosts.ini` from every commit, but the repository has a fork, and a fork
keeps its own copy of the history regardless. Rewriting would break every
existing clone and invalidate the commit hashes other people may have
referenced, in exchange for hiding a password that is already public and
already needs rotating. Rotating is cheaper and actually closes the hole.

If you fork this for real use, start from a fresh history rather than
filtering this one.

## Deliberate weaknesses

These are known and intentional for the lab. Do not carry them into
anything reachable from an untrusted network.

- **Plaintext passwords in the inventory.** `hosts.ini` stores
  `ansible_password` inline, and the file is excluded from git. For shared
  use, move it into Ansible Vault and reference `{{ vault_... }}`.
- **SNMP v2c.** The community string travels in clear text on every request.
  Acceptable inside an isolated lab segment; use v3 elsewhere.
- **`ansible_user=root`.** The playbooks drive FRR through `shell` and
  `vtysh`, which needs privilege. A dedicated user with sudo would be
  better.
- **Self-signed TLS.** The dashboard stack terminates TLS with a generated
  certificate, so clients cannot verify the host.

## Reporting

This is a personal lab. Open an issue rather than expecting a private
disclosure channel.

## CRLF line endings in `run_backup.sh`

One committed script, `run_backup.sh`, is stored in git with CRLF line
endings, so its `#!/bin/sh` is followed by a carriage return. On Linux/Alpine
that is a `bad interpreter: /bin/sh^M` failure, not a warning — the script will
not run until the file is converted. The other fourteen `.sh` files are stored
with LF and are fine; this is not a repository-wide problem.

The cause is a checkout where `core.autocrlf` was off or unset when this file
was committed, so the CRLF bytes were stored verbatim. With `core.autocrlf=true`
Git converts CRLF to LF on **commit** (verified: a CRLF scratch file added here
stores LF in the index), so re-committing this file from a checkout with that
setting is sufficient. The other scripts were added from such a checkout and
store LF.

`ansible.cfg`, `host_vars/*.yml` and most of `playbooks/*.yml` also carry CRLF
in their blobs. That is harmless — YAML and INI parsers accept CRLF, and only
the shebang line of an executable is sensitive to it. Leave them alone rather
than producing a 15-file whitespace diff; if you do normalise, do it as its own
commit so the real changes stay reviewable.

Fix:

    tr -d '\r' < run_backup.sh > run_backup.sh.tmp && mv run_backup.sh.tmp run_backup.sh
    git add run_backup.sh
    git commit -m "Store run_backup.sh with LF line endings"

Verify — count carriage returns in each stored blob and print the paths that
have any. Expect `run_backup.sh` before the fix and nothing after it:

    git ls-files '*.sh' | while read f; do
      n=$(git cat-file -p "HEAD:$f" | tr -cd '\r' | wc -c)
      [ "$n" -gt 0 ] && echo "CRLF: $f ($n CRs)"
    done

Use `tr`, not `grep -q $'\r'`. GNU grep on Git Bash drops a literal CR from a
command-line pattern and reports no match even against a file that contains
one — a verification command that silently passes on the exact file it should
catch is worse than no verification command.

Until that is done, convert before use on the controller:
`sed -i 's/\r$//' run_backup.sh`.
