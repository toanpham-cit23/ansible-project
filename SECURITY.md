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

### What still has to happen

Rotation is the only remediation that matters, and it happens outside this
repository:

- [ ] Change the `root` password on r1, r2 and r3.
- [ ] Change the seeded dashboard device passwords, or re-run `setup.sh`
      with `DEV_PW_1`, `DEV_PW_2`, `DEV_PW_3` and `DEV_PW_10` set.
- [ ] Change the SNMP community string and pass it with
      `-e snmp_community=...`, or use SNMPv3 with auth and priv.
- [ ] Rotate `ADMIN_PASSWORD` in the dashboard `.env`.

Until those are done, the credentials in the history are live.

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
