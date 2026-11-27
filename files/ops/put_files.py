#!/usr/bin/env python3
"""Upload local files to the controller over SFTP, preserving relative paths.

A tiny sibling of exec_ctrl.py: that one runs commands, this one moves files.
Both share the same credential rule - the password comes from LAB_SSH_PASS and
is never stored in the repo.

The remote parent chain is created one level at a time because Alpine's
sftp-server has no recursive mkdir. Each level is checked with lstat() rather
than stat(): a symlinked parent (e.g. /root/lab -> elsewhere) must not be
mistaken for a missing directory, and lstat is the cheap way to tell.

Usage:
    python3 put_files.py REMOTE_ROOT LOCAL... [--owner=user:group]

On Git Bash for Windows, prefix with MSYS_NO_PATHCONV=1 or the shell rewrites
/tmp/x into C:/Program Files/Git/tmp/x before Python ever sees it.
"""
import os
import posixpath
import sys

import paramiko

HOST = os.environ.get("LAB_SSH_HOST", "192.168.23.10")
PORT = int(os.environ.get("LAB_SSH_PORT", "22"))
USER = os.environ.get("LAB_SSH_USER", "admin")
PASS = os.environ.get("LAB_SSH_PASS")

if not PASS:
    raise SystemExit("set LAB_SSH_PASS in the environment")


def ensure_dir(sftp, path):
    """Create path and any missing parents. Returns True if it made something."""
    made = False
    cur = ""
    for part in path.split("/"):
        if not part:
            continue
        cur += "/" + part
        try:
            sftp.lstat(cur)
            continue
        except IOError:
            pass
        try:
            sftp.mkdir(cur)
            made = True
        except IOError as e:
            # Another process may have won the race, or the level already
            # exists as a symlink; re-check before giving up.
            try:
                sftp.lstat(cur)
            except IOError:
                raise SystemExit("cannot create %s: %s" % (cur, e))
    return made


def main():
    argv = [a for a in sys.argv[1:] if not a.startswith("--")]
    owner = None
    for a in sys.argv[1:]:
        if a.startswith("--owner="):
            owner = a.split("=", 1)[1]
    if len(argv) < 2:
        raise SystemExit(__doc__)

    remote_root = argv[0]
    c = paramiko.SSHClient()
    c.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    c.connect(HOST, port=PORT, username=USER, password=PASS, timeout=25,
              allow_agent=False, look_for_keys=False)
    try:
        with c.open_sftp() as sftp:
            for local in argv[1:]:
                rel = local.replace("\\", "/")
                for prefix in ("./",):
                    while rel.startswith(prefix):
                        rel = rel[len(prefix):]
                remote = posixpath.join(remote_root, rel)
                ensure_dir(sftp, posixpath.dirname(remote))
                sftp.put(local, remote)
                print("put %s -> %s (%d bytes)" % (local, remote,
                                                   os.path.getsize(local)))
        if owner:
            # chown needs root; this connection is `admin`, so it only works if
            # the remote already permits it. Report the failure instead of
            # pretending the upload was complete.
            cmd = "chown -R %s %s" % (owner, remote_root)
            stdin, stdout, stderr = c.exec_command(cmd)
            rc = stdout.channel.recv_exit_status()
            print("chown rc=%d %s" % (rc, stderr.read().decode().strip()))
    finally:
        c.close()


if __name__ == "__main__":
    main()
