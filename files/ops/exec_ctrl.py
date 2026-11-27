#!/usr/bin/env python3
"""Run a command on the lab controller over SSH as `admin`, then escalate to root.

Why a script and not plain ssh: there is no sshpass on this workstation and the
controller's sshd is password-only for `admin`. paramiko keeps the password out
of argv and out of shell history.

Usage:
    python3 exec_ctrl.py "uname -a"                  # as admin
    python3 exec_ctrl.py --root "rc-service lab-tunnel status"
    python3 exec_ctrl.py --root --file remove.sh     # upload and run as root

Credentials come from the environment, never argv:
    LAB_SSH_HOST (default 192.168.23.10)
    LAB_SSH_USER (default admin)
    LAB_SSH_PASS (required; the admin password, not stored in this repo)

Escalation notes, learned on this box:
  * There is no sudo. Alpine ships doas (/usr/bin/doas) with no /etc/doas.conf,
    so doas prompts for the invoking user's password on the tty.
  * doas needs a pty: exec_command gives none, the prompt never appears and the
    call hangs. A shell channel is used instead.
  * Nested quoting through `doas sh -c '...'` mangled commands in testing, so
    the command is written to a temp file with SFTP and executed as a file.
    That also makes multi-line scripts work without escaping.
"""
import argparse
import os
import posixpath
import re
import sys
import time
import uuid

import paramiko

HOST = os.environ.get("LAB_SSH_HOST", "192.168.23.10")
PORT = int(os.environ.get("LAB_SSH_PORT", "22"))
USER = os.environ.get("LAB_SSH_USER", "admin")
# No default. This repository is public, so a working password must never be
# committed here - the same mistake as the `labadmin` leak in 0633488.
PASS = os.environ.get("LAB_SSH_PASS")
TIMEOUT = int(os.environ.get("LAB_SSH_TIMEOUT", "300"))

if not PASS:
    raise SystemExit(
        "set LAB_SSH_PASS in the environment (the admin password for the "
        "controller). It is deliberately not stored in this repo."
    )


def connect():
    c = paramiko.SSHClient()
    # The controller's host key has been regenerated during rebuilds; pinning it
    # would break this script without protecting against anyone who can already
    # reach port 22 on the lab LAN.
    c.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    c.connect(HOST, port=PORT, username=USER, password=PASS, timeout=25,
              allow_agent=False, look_for_keys=False)
    return c


def run_pty(c, cmdline, need_pass=False, timeout=TIMEOUT):
    """Run one shell line over a pty, feeding the doas password when prompted.

    Returns (rc, output). The command must be a single line and must echo a
    sentinel `__RC:<n>__` at the end so the exit status survives the pty.
    """
    ch = c.invoke_shell(width=250, height=80)
    try:
        time.sleep(0.8)
        while ch.recv_ready():
            ch.recv(65535)
        ch.send(cmdline + "\n")
        buf = b""
        sent = False
        deadline = time.time() + timeout
        while time.time() < deadline:
            if ch.recv_ready():
                buf += ch.recv(65535)
                text = buf.decode("utf-8", "replace")
                if need_pass and not sent and "password:" in text:
                    ch.send(PASS + "\n")
                    sent = True
                if re.search(r"__RC:\d+__", text):
                    # let the tail of the output arrive before closing
                    time.sleep(0.25)
                    while ch.recv_ready():
                        buf += ch.recv(65535)
                    break
            else:
                time.sleep(0.05)
        text = buf.decode("utf-8", "replace")
        rc = 0
        m = re.search(r"__RC:(\d+)__", text)
        if m:
            rc = int(m.group(1))
            text = text[: m.start()]
        # strip the echoed command, the doas prompt and the trailing shell prompt
        text = re.sub(r"doas \([^)]*\) password: ", "", text)
        text = re.sub(r"\x1b\[[0-9;]*[A-Za-z]", "", text)   # ANSI escapes
        text = re.sub(r"\r\n?", "\n", text)
        # The pty echoes the line we typed ("doas sh /tmp/labexec-….sh; …").
        # Drop it plus any blank lines it left behind.
        text = re.sub(r"^[^\n]*(labexec-[0-9a-f]+\.sh|__RC:\$\?__)[^\n]*\n", "", text)
        text = re.sub(r"\n[^\n]*[#$] ?$", "", text)          # trailing prompt
        return rc, text.strip("\n")
    finally:
        ch.close()


def run(cmd, root=False, timeout=TIMEOUT):
    c = connect()
    try:
        # Always execute from an uploaded file: no quoting to get wrong, and the
        # same path works for a one-liner and a 200-line script.
        remote = "/tmp/labexec-%s.sh" % uuid.uuid4().hex[:12]
        script = cmd if cmd.endswith("\n") else cmd + "\n"
        with c.open_sftp() as sftp:
            with sftp.open(remote, "w") as f:
                f.write(script)
            sftp.chmod(remote, 0o700)
        try:
            prefix = "doas " if root else ""
            line = "%ssh %s; echo __RC:$?__" % (prefix, remote)
            return run_pty(c, line, need_pass=root, timeout=timeout)
        finally:
            with c.open_sftp() as sftp:
                try:
                    sftp.remove(remote)
                except OSError:
                    pass
    finally:
        c.close()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("command", nargs="?")
    ap.add_argument("--root", action="store_true", help="escalate with doas")
    ap.add_argument("--file", help="read the command from this local file")
    a = ap.parse_args()

    if a.file:
        cmd = open(a.file, encoding="utf-8").read()
    elif a.command:
        cmd = a.command
    else:
        ap.error("give a command or --file")

    rc, out = run(cmd, root=a.root)
    if out:
        print(out)
    sys.exit(rc)


if __name__ == "__main__":
    main()
