#!/usr/bin/env python3
"""Start real listeners on the Servers host, and test ACL reachability.

Why a Python listener and not `nc -l`: busybox nc exits as soon as its single
connection closes, so by the time the second probe runs the listener is gone
and every test degrades to ConnectionRefused. A forking socket server stays up
for the whole run.

The distinction that matters:
  CONNECTED / ConnectionRefused -> the packet was forwarded, ACL permitted it
  Timeout                       -> the packet was dropped, ACL denied it

A refused connection is therefore a *pass* for an "allowed" check as long as
something is listening; with a live listener it becomes CONNECTED.
"""
import socket
import sys
import threading
import time


def serve(port, ns):
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("0.0.0.0", port))
    s.listen(16)
    while True:
        try:
            c, _ = s.accept()
            c.close()
        except OSError:
            break


if __name__ == "__main__":
    ports = [int(a) for a in sys.argv[1:]] or [80, 443, 22, 3306]
    for p in ports:
        threading.Thread(target=serve, args=(p, None), daemon=True).start()
    # keep the process alive; the parent shell backgrounds it
    while True:
        time.sleep(3600)
