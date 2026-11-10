#!/bin/sh
# One-command DuckDNS + trusted HTTPS setup, run on the controller over SSH.
#
#   DUCKDNS_DOMAIN=mylab DUCKDNS_TOKEN=xxxx /root/lab/ops/duckdns-setup.sh
#   /root/lab/ops/duckdns-setup.sh mylab xxxx you@example.com
#
# Env vars are preferred: arguments land in the shell history and in `ps`.
# The token is verified against DuckDNS before anything is written, so a typo
# cannot leave a half-configured certificate behind.
set -u

LAB=/root/lab
OPS=$LAB/ops
S=$OPS/status.py
CFG=$OPS/duckdns.env

DOMAIN="${DUCKDNS_DOMAIN:-${1:-}}"
TOKEN="${DUCKDNS_TOKEN:-${2:-}}"
EMAIL="${ACME_EMAIL:-${3:-}}"
NOW=$(date -u '+%Y-%m-%dT%H:%M:%SZ')

# Accept either "mylab" or "mylab.duckdns.org" so copy/paste from the DuckDNS
# dashboard just works.
DOMAIN=$(printf '%s' "$DOMAIN" | tr 'A-Z' 'a-z' | sed 's/\.duckdns\.org$//' | tr -d ' \r\n')

if [ -z "$DOMAIN" ] || [ -z "$TOKEN" ]; then
  echo "usage: DUCKDNS_DOMAIN=<subdomain> DUCKDNS_TOKEN=<token> $0" >&2
  echo "       (ACME_EMAIL is optional; a Let's Encrypt expiry notice is worth having)" >&2
  exit 2
fi
if ! printf '%s' "$DOMAIN" | grep -Eq '^[a-z0-9][a-z0-9-]{0,30}$'; then
  echo "Invalid subdomain. Use letters, digits and hyphens only, without .duckdns.org" >&2
  exit 2
fi

IP=$(wget -qO- --timeout=10 https://api.ipify.org 2>/dev/null | tr -d '\r\n' | head -c 45)
[ -z "$IP" ] && IP=$(wget -qO- --timeout=10 https://ifconfig.me/ip 2>/dev/null | tr -d '\r\n' | head -c 45)
if [ -z "$IP" ]; then
  echo "Cannot determine the public IP - outbound HTTPS is required" >&2
  exit 1
fi

echo "Verifying the token against DuckDNS for ${DOMAIN}.duckdns.org -> ${IP} ..."
RESP=$(wget -qO- --timeout=12 \
  "https://www.duckdns.org/update?domains=${DOMAIN}&token=${TOKEN}&ip=${IP}" \
  2>/dev/null | tr -d '\r\n' | head -c 20)
if [ "$RESP" != "OK" ]; then
  echo "DuckDNS rejected the token/domain (response: '${RESP}'). Nothing was changed." >&2
  echo "Check the subdomain is yours and the token is copied in full from duckdns.org." >&2
  python3 "$S" duckdns ok=false state=error domain="${DOMAIN}.duckdns.org" \
    public_ip="$IP" checked_at="$NOW" message="Token verification failed"
  exit 1
fi
echo "Token accepted."

umask 077
cat > "$CFG" <<EOF
# DuckDNS + ACME configuration. Mode 0600 - the token is a credential.
# Written by duckdns-setup.sh on ${NOW}.
DUCKDNS_DOMAIN="${DOMAIN}"
DUCKDNS_TOKEN="${TOKEN}"
ACME_EMAIL="${EMAIL}"
EOF
chmod 600 "$CFG"

"$OPS/duckdns-update.sh"
echo "Issuing the certificate (DNS-01, no inbound port needed) ..."
"$OPS/acme-issue.sh"

echo
echo "--- result ---"
cat "$LAB/db/ops/tls.json" 2>/dev/null || echo "no TLS status file"
echo
echo "Open https://${DOMAIN}.duckdns.org:8443 (or use the lab DNS record added by acme-issue.sh)."
echo "HTTP on :8080 remains available as a fallback."
