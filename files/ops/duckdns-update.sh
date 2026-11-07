#!/bin/sh
# Keep the DuckDNS A record pointed at the current public IP.
# Deployed and run on the controller over SSH; invoked by crond every 5 min.
set -u

LAB=/root/lab
OPS=$LAB/ops
S=$OPS/status.py
. "$OPS/duckdns.env" 2>/dev/null || { DUCKDNS_DOMAIN=""; DUCKDNS_TOKEN=""; }

NOW=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
mkdir -p "$LAB/db/ops"

if [ -z "${DUCKDNS_DOMAIN:-}" ] || [ -z "${DUCKDNS_TOKEN:-}" ]; then
  python3 "$S" duckdns \
    ok=false state=unconfigured \
    message="Set DUCKDNS_DOMAIN and DUCKDNS_TOKEN in /root/lab/ops/duckdns.env, then run acme-issue.sh" \
    checked_at="$NOW"
  exit 0
fi

IP=$(wget -qO- --timeout=10 https://api.ipify.org 2>/dev/null | tr -d '\r\n' | head -c 45)
if [ -z "$IP" ]; then
  IP=$(wget -qO- --timeout=10 https://ifconfig.me/ip 2>/dev/null | tr -d '\r\n' | head -c 45)
fi
if [ -z "$IP" ]; then
  python3 "$S" duckdns ok=false state=error message="Could not determine public IP" checked_at="$NOW"
  exit 1
fi

RESP=$(wget -qO- --timeout=12 \
  "https://www.duckdns.org/update?domains=${DUCKDNS_DOMAIN}&token=${DUCKDNS_TOKEN}&ip=${IP}" \
  2>/dev/null | tr -d '\r\n' | head -c 20)

echo "$IP" > "$LAB/db/ops/duckdns.ip"
if [ "$RESP" = "OK" ]; then
  python3 "$S" duckdns ok=true state=synced domain="${DUCKDNS_DOMAIN}.duckdns.org" \
    public_ip="$IP" last_sync="$NOW" checked_at="$NOW" \
    message="A record is current"
else
  python3 "$S" duckdns ok=false state=error domain="${DUCKDNS_DOMAIN}.duckdns.org" \
    public_ip="$IP" last_sync="" checked_at="$NOW" \
    message="DuckDNS rejected the update (check token and subdomain)"
fi
