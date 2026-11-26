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

# Default to the repository this file lives in, so the guard works from any
# checkout instead of only from the controller's /root/ansible-project. A
# hardcoded REPO meant that running it anywhere else raised FileNotFoundError
# out of subprocess(cwd=...) rather than reporting CLEAN or BLOCKED.
REPO = os.environ.get("PREPUSH_REPO") or os.path.dirname(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
)
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
            # Inventory lines carry credentials inline:
            #   r1 ansible_host=... ansible_password=secret
            # That line never matches the key=value regex below (its first token
            # is the hostname, followed by a space, not = or :), so this scan
            # must happen before that regex can `continue` past it. It used to
            # sit after the `if not m: continue`, which made it unreachable and
            # left inline inventory passwords unchecked.
            for im in re.finditer(
                    r"(?:ansible_password|ansible_ssh_pass|ansible_become_pass)"
                    r"\s*=\s*[\"']?([^\s\"']+)", line):
                v = im.group(1)
                if len(v) >= MIN_LEN:
                    out.add(v)
            m = re.match(r"([A-Za-z_][A-Za-z0-9_]*)\s*[=:]\s*(.+)$", line)
            if not m:
                continue
            key, val = m.group(1), m.group(2).strip().strip('"').strip("'")
            if PII_KEY.match(key) or not SECRET_KEY.match(key):
                continue
            if len(val) >= MIN_LEN and not val.startswith(("{{", "$", "<")):
                out.add(val)
    return out


def staged_paths():
    """Every path that `git add -A && git commit` would include.

    Two sources, unioned:
      - the index (`git diff --cached --name-only`), for files already staged;
      - a dry run (`git add -A -n`), for modified/untracked files not staged.

    Neither alone is enough. The dry run omits files that are already staged,
    and the index omits files that are not. Choosing one over the other left a
    hole: the push playbook runs this audit *before* its own `git add -A`, so
    a leftover staged file from an aborted run made the guard look only at the
    index and skip every unstaged edit that was about to be committed.
    """
    staged = subprocess.run(["git", "diff", "--cached", "--name-only"],
                            cwd=REPO, capture_output=True, text=True)
    pending = subprocess.run(["git", "add", "-A", "-n"], cwd=REPO,
                             capture_output=True, text=True)
    paths = [p for p in staged.stdout.splitlines() if p.strip()]
    # Git quotes paths containing spaces or non-ASCII in the dry run. `add -n`
    # output looks like: add 'path/to/file'
    paths += re.findall(r"add '([^']+)'", pending.stdout)
    return sorted(set(paths))


def file_texts(rel):
    """All versions of a path that could end up in the commit.

    Check both the index blob and the working-tree file. Reading only the
    working tree misses a secret that was staged and then removed from disk;
    reading only the index misses an unstaged secret that `git add -A` in the
    push playbook is about to stage. A path can legitimately have both, so
    return both rather than choosing one.
    """
    texts = []
    p = os.path.join(REPO, rel)
    try:
        if os.path.isfile(p) and os.path.getsize(p) <= 4_000_000:
            texts.append(open(p, errors="ignore").read())
    except OSError:
        pass
    try:
        r = subprocess.run(["git", "show", f":{rel}"], cwd=REPO,
                           capture_output=True, check=False)
        if r.returncode == 0 and len(r.stdout) <= 4_000_000:
            texts.append(r.stdout.decode("utf-8", "ignore"))
    except OSError:
        pass
    return texts


def _is_script(path):
    """True when the file declares a shell shebang.

    The extension list is not enough: this repo has runnable scripts with no
    extension at all, and a CRLF shebang in one of those is the worst case -
    the kernel cannot even find the interpreter.
    """
    try:
        with open(path, "rb") as fh:
            head = fh.read(64)
    except OSError:
        return False
    return head.startswith(b"#!") and (b"sh" in head.split(b"\n")[0])


def css_layout_problems(css):
    """Layout invariants that have already caused a shipped regression.

    The phone shell broke twice the same way: a rule made the sidebar full
    width without also stacking `.shell`, so `<main>` kept only its padding
    (18 px measured live) and headings wrapped one character per line. A
    second version fixed the shell but left the 44 px strip in `column`
    direction, which squeezed the nav to height 0 and hid the menu.

    Returns a list of (path, why) tuples; empty means the invariants hold.
    """
    flat = " ".join(css.split())        # collapse indentation and newlines
    # A rule's body, with "{" and ";" spacing normalised, so the checks do not
    # depend on how the file happens to be indented.
    flat = flat.replace("{ ", "{").replace(" ;", ";")
    out = []

    sidebar_full = ".side {width: 100%; max-width: 100%;" in flat
    # Any `html.device-phone .shell { ... }` rule may supply the column
    # direction; earlier ones (v10) only set min-width, so scan them all.
    shell_column = any(
        "flex-direction: column" in body[:200]
        for body in flat.split("html.device-phone .shell {")[1:]
    )
    if sidebar_full and not shell_column:
        out.append(("files/dashboard/docker/api/static/css/app.css",
                    "phone sidebar is full width but .shell is not a column - "
                    "<main> collapses to its padding"))

    strip_fixed = "height: 44px; max-height: 44px;" in flat
    # Likewise: any rule with the fixed strip height may set the row direction.
    strip_row = any(
        "flex-direction: row" in body[:300]
        for body in flat.split("height: 44px; max-height: 44px;")[1:]
    )
    if strip_fixed and not strip_row:
        out.append(("files/dashboard/docker/api/static/css/app.css",
                    "phone .side strip has a fixed height without "
                    "flex-direction: row - the nav collapses to 0 px"))

    if ".op-kv span" in flat:
        after = flat.split(".op-kv span")[-1][:200]
        if "white-space: nowrap" not in after:
            out.append(("files/dashboard/docker/api/static/css/app.css",
                        ".op-kv span may wrap - labels such as Inventory break "
                        "into one word per line"))

    # A wide table inside a phone viewport must have a horizontal scroll box.
    # `overflow-x` on <table> itself does nothing - a table box does not honour
    # it - so the rule looks right and ships broken: 793 px of log rows inside
    # a 390 px phone, with the Level and Action columns clipped away by the
    # surrounding .panel { overflow:hidden } and impossible to reach.
    if "device-phone" in flat:
        has_box = (".table-scroll" in flat
                   or "display: block" in flat.split("device-phone .panel > table")[-1][:160])
        if not has_box:
            out.append(("files/dashboard/docker/api/static/css/app.css",
                        "phone tables have no scroll box - a wide table is "
                        "clipped by the panel and its right columns are "
                        "unreachable"))

    # A drawing with text in it must not be scaled down to the viewport: the
    # topology is a fixed 980x565 map with 11-12 px labels, and letting it
    # shrink to 370 px turned every address into unreadable grey pixels.
    if "topo-box" in flat and "device-phone" in flat:
        after = flat.split("device-phone .topo-box")[-1][:220]
        if "min-width" not in after:
            out.append(("files/dashboard/docker/api/static/css/app.css",
                        "phone topology svg is scaled to the viewport - its "
                        "labels become unreadable; it needs a fixed min-width "
                        "and a scrolling box"))
    return out


def main():
    sec = secrets()
    paths = staged_paths()
    leaks = []
    for rel in paths:
        if BAD_NAMES.search(rel):
            leaks.append((rel, "sensitive filename"))
        if any(v in txt for txt in file_texts(rel) for v in sec):
            # One report per file even when the value appears in both the
            # index blob and the working tree (or in several lines).
            leaks.append((rel, "live secret value"))

    # Line endings are not a style question here. These scripts land on Alpine
    # and are run by sh, OpenRC and systemd; a CRLF shebang or a CRLF `case`
    # statement fails at runtime, not at review, and the failure looks like a
    # missing file rather than a bad byte. That already cost one debugging
    # cycle: an init script uploaded over SFTP ran perfectly from bash on the
    # workstation and made OpenRC report "Failed executing /etc/init.d/...".
    crlf = []
    for rel in paths:
        p = os.path.join(REPO, rel)
        if not (rel.endswith((".sh", ".initd", ".service")) or
                rel.endswith("/" + os.path.basename(rel)) and _is_script(p)):
            continue
        try:
            with open(p, "rb") as fh:
                if b"\r\n" in fh.read():
                    crlf.append(rel)
        except OSError:
            pass

    layout = []
    css_rel = "files/dashboard/docker/api/static/css/app.css"
    if css_rel in paths:
        try:
            css = open(os.path.join(REPO, css_rel), encoding="utf-8").read()
            layout = css_layout_problems(css)
        except OSError:
            pass

    print(f"pre-push audit: {len(paths)} files, {len(sec)} secret values checked")
    if leaks:
        for rel, why in leaks:
            print(f"  BLOCKED {rel}: {why}")
        sys.exit(1)
    if crlf:
        for rel in crlf:
            print(f"  BLOCKED {rel}: CRLF line endings in a script that runs on Linux")
        print("  fix with: sed -i 's/\\r$//' <file>")
        sys.exit(1)
    if layout:
        for rel, why in layout:
            print(f"  BLOCKED {rel}: {why}")
        sys.exit(1)
    if not paths:
        # Nothing staged and nothing untracked: there is no commit to guard.
        # Say so, rather than printing CLEAN next to "0 files" where it reads
        # as a pass over the whole tree.
        print("pre-push audit: NOTHING TO CHECK (no staged or untracked files)")
        return
    if not sec:
        print("pre-push audit: CLEAN (filename check only - no live configs "
              "found to compare values against)")
        return
    print("pre-push audit: CLEAN")


if __name__ == "__main__":
    main()
