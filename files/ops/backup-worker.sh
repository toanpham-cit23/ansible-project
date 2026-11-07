#!/bin/sh
# Cron hook: execute queued operator requests from the dashboard.
# The API writes /data/ops/*.request; it never runs a command itself.
set -u
LAB=/root/lab
D=$LAB/db/ops

if [ -f "$D/snapshot.request" ]; then
  rm -f "$D/snapshot.request"
  "$LAB/ops/snapshot.sh" >/dev/null 2>&1
fi

if [ -f "$D/retention.request" ]; then
  MODE=$(cat "$D/retention.request" 2>/dev/null | head -1)
  rm -f "$D/retention.request"
  if [ "$MODE" = "apply" ]; then
    "$LAB/ops/retention.sh" --apply >/dev/null 2>&1
  else
    "$LAB/ops/retention.sh" >/dev/null 2>&1
  fi
fi
