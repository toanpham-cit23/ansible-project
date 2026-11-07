#!/bin/sh
# Publish the current Quick Tunnel URL and health into a status file the
# dashboard reads. Run by cron every minute.
set -u

LAB=/root/lab
OPS=$LAB/ops
S=$OPS/status.py
LOG=/var/log/lab-tunnel.log
NOW=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
DOMAIN=""
if [ -f "$LAB/ops/duckdns.env" ]; then
  . "$LAB/ops/duckdns.env" 2>/dev/null || true
  [ -n "${DUCKDNS_DOMAIN:-}" ] && DOMAIN="${DUCKDNS_DOMAIN}.duckdns.org"
fi
ORIGIN="https://${DOMAIN:-localhost}:8443"

URL=$(grep -oE 'https://[a-zA-Z0-9-]+\.trycloudflare\.com' "$LOG" 2>/dev/null | tail -1)
RUNNING=no
rc-service lab-tunnel status >/dev/null 2>&1 && RUNNING=yes

if [ "$RUNNING" = "yes" ] && [ -n "$URL" ]; then
  python3 "$S" cloudflared ok=true state=connected url="$URL" \
    service="$RUNNING" origin="$ORIGIN" \
    checked_at="$NOW" \
    message="Reachable from anywhere with no port forward; URL changes if the tunnel restarts"
elif [ "$RUNNING" = "yes" ]; then
  python3 "$S" cloudflared ok=false state=starting url="" service="$RUNNING" \
    checked_at="$NOW" message="Tunnel is running, waiting for the assigned URL"
else
  python3 "$S" cloudflared ok=false state=down url="" service=no \
    checked_at="$NOW" message="Tunnel service is not running"
fi

[ -n "$URL" ] && echo "$URL"
