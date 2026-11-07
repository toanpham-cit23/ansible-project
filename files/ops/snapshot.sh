#!/bin/sh
# Take a consistent snapshot of the lab's state.
#
# The API container is read-only and has no SSH client or sqlite3 CLI, so the
# button in the dashboard can only leave a request file. This script runs on
# the controller (picked up by backup-worker.sh from cron) and does the work.
# An online SQLite backup is used rather than copying net.db, because WAL mode
# would otherwise produce a snapshot that cannot be restored.
set -u

LAB=/root/lab
OPS=$LAB/ops
BACKUPS=$LAB/backups
STAMP=$(date -u '+%Y%m%d-%H%M%S')
TMP=$LAB/db/.snapshot.db
DEST="$BACKUPS/$STAMP.tgz"
mkdir -p "$BACKUPS"

# Online backup through the poller container, which already owns /data.
docker exec lab-poller python3 -c "
import sqlite3
s = sqlite3.connect('/data/net.db')
d = sqlite3.connect('/data/.snapshot.db')
s.backup(d)
d.close(); s.close()
" >/dev/null 2>&1 || {
  python3 "$OPS/status.py" backup ok=false state=error \
    message="SQLite online backup failed" checked_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  exit 1
}

# BusyBox tar does not handle several -C switches with file lists the way GNU
# tar does, so stage everything under one root first.
STAGE=$(mktemp -d /root/lab/tmp/snap.XXXXXX) || {
  python3 "$OPS/status.py" backup ok=false state=error \
    message="Could not create staging directory" checked_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  exit 1
}
cp "$TMP" "$STAGE/net.db" 2>/dev/null
cp -r "$LAB/db/ops" "$STAGE/ops-status" 2>/dev/null
cp -r "$LAB/ops" "$STAGE/ops-scripts" 2>/dev/null
cp "$LAB/.env" "$STAGE/env" 2>/dev/null
cp "$LAB/docker-compose.yml" "$STAGE/docker-compose.yml" 2>/dev/null

tar -czf "$DEST" -C "$STAGE" . 2>/dev/null || {
  rm -rf "$STAGE" "$TMP"
  python3 "$OPS/status.py" backup ok=false state=error \
    message="tar failed" checked_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  exit 1
}
chmod 600 "$DEST"
rm -rf "$STAGE" "$TMP"

COUNT=$(ls -1 "$BACKUPS"/*.tgz 2>/dev/null | wc -l | tr -d ' ')
SIZE=$(stat -c %s "$DEST" 2>/dev/null || echo 0)
RESTORE="never"
[ -f "$LAB/db/ops/restore.json" ] && \
  RESTORE=$(python3 -c "import json;print(json.load(open('$LAB/db/ops/restore.json')).get('at','never'))" 2>/dev/null)

python3 "$OPS/status.py" backup ok=true state=complete \
  last_snapshot="$STAMP" last_snapshot_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
  snapshot_count="$COUNT" last_snapshot_bytes="$SIZE" last_restore="$RESTORE" \
  message="Snapshot written to $DEST" \
  checked_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
