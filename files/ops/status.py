#!/usr/bin/env python3
"""Write one ops status file atomically.

    status.py NAME key=value [key=value ...]

Values that look like ints/bools are converted, so the API can render them
without guessing. Atomic replace matters: the dashboard polls this every five
seconds and must never read a half-written file.
"""
import json
import os
import sys
import tempfile
import time

OUT = "/root/lab/db/ops"


def convert(v):
    low = v.lower()
    if low in ("true", "false"):
        return low == "true"
    if v.lstrip("-").isdigit():
        try:
            return int(v)
        except ValueError:
            pass
    return v


def main():
    if len(sys.argv) < 2:
        raise SystemExit("usage: status.py NAME key=value ...")
    name = sys.argv[1]
    safe = "".join(c for c in name if c.isalnum() or c in "-_")
    data = {}
    for a in sys.argv[2:]:
        k, sep, v = a.partition("=")
        if sep:
            data[k] = convert(v)
    data["updated_at"] = time.strftime("%Y-%m-%d %H:%M:%S", time.gmtime()) + " UTC"
    os.makedirs(OUT, exist_ok=True)
    dest = os.path.join(OUT, safe + ".json")
    fd, tmp = tempfile.mkstemp(dir=OUT, prefix=".tmp-")
    try:
        with os.fdopen(fd, "w") as fh:
            json.dump(data, fh, sort_keys=True, indent=1)
        os.chmod(tmp, 0o644)
        os.replace(tmp, dest)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)
    print(dest)


if __name__ == "__main__":
    main()
