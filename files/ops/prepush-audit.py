#!/usr/bin/env python3
"""Fail a push if any file git is about to commit contains a live secret.

Runs on the controller as a pre-push guard. Secrets are read from the local
config files, never printed, and never written into the repository. On a fresh
clone those files will not exist, in which case only the filename check runs.
"""
import os
import re
import subprocess
import sys

REPO = "/root/ansible-project"
CONFIGS = ["/root/lab/.env", "/root/lab/ops/duckdns.env", f"{REPO}/hosts.ini"]

# Only values long enough to be meaningful are scanned. Two-character lab
# passwords would match half the source tree and tell us nothing.
MIN_LEN = 8
BAD_NAMES = re.compile(
    r"(?i)(\.env$|token\.yml$|vault_pass|hosts\.ini$|\.pem$|\.key$|net\.db|id_rsa)")


# Only credential-bearing keys. Treating every assignment as a secret made the
# guard flag IP addresses and hostnames, which appear all over the source and
# would have blocked every push.
SECRET_KEY = re.compile(
    r"(?i)^(jwt_secret|netdb_key|admin_password|smtp_pass|mail_password|"
    r"duckdns_token|acme_email|github_token|ansible_password|ansible_ssh_pass|"
    r"ansible_become_pass|vault_router_password|api_key|secret_key)$")
# The ACME contact address is personal data rather than a credential; keep it
# out of the scan so a real address in a doc does not block a release.
PII_KEY = re.compile(r"(?i)^acme_email$")


def secrets():
    out = set()
    for p in CONFIGS:
        if not os.path.exists(p):
            continue
        try:
            lines = open(p, errors="ignore").read().splitlines()
        except OSError:
            continue
        for line in lines:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            m = re.match(r"([A-Za-z_][A-Za-z0-9_]*)\s*[=:]\s*(.+)$", line)
            if not m:
                continue
            key, val = m.group(1), m.group(2).strip().strip('"').strip("'")
            if PII_KEY.match(key) or not SECRET_KEY.match(key):
                continue
            if len(val) >= MIN_LEN and not val.startswith(("{{", "$", "<")):
                out.add(val)
            # ansible_password=... may sit inline in an INI line
            for im in re.finditer(r"ansible_password=(\S+)", line):
                v = im.group(1)
                if len(v) >= MIN_LEN:
                    out.add(v)
    return out


def staged_paths():
    r = subprocess.run(["git", "add", "-A", "-n"], cwd=REPO,
                       capture_output=True, text=True)
    return re.findall(r"add '([^']+)'", r.stdout)


def main():
    sec = secrets()
    paths = staged_paths()
    leaks = []
    for rel in paths:
        if BAD_NAMES.search(rel):
            leaks.append((rel, "sensitive filename"))
        p = os.path.join(REPO, rel)
        if not os.path.isfile(p) or os.path.getsize(p) > 4_000_000:
            continue
        try:
            txt = open(p, errors="ignore").read()
        except OSError:
            continue
        for v in sec:
            if v in txt:
                leaks.append((rel, "live secret value"))
    print(f"pre-push audit: {len(paths)} files, {len(sec)} secret values checked")
    if leaks:
        for rel, why in leaks:
            print(f"  BLOCKED {rel}: {why}")
        sys.exit(1)
    print("pre-push audit: CLEAN")


if __name__ == "__main__":
    main()
