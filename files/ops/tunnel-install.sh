#!/bin/sh
# Install / start the Cloudflare Quick Tunnel. Run over SSH; safe to re-run.
set -u

LAB=/root/lab
OPS=$LAB/ops
INIT=/etc/init.d/lab-tunnel
DOMAIN=""
if [ -f "$OPS/duckdns.env" ]; then
  . "$OPS/duckdns.env" 2>/dev/null || true
  [ -n "${DUCKDNS_DOMAIN:-}" ] && DOMAIN="${DUCKDNS_DOMAIN}.duckdns.org"
fi
if [ -z "$DOMAIN" ]; then
  echo "DUCKDNS_DOMAIN is not configured - refusing to start a tunnel with no origin" >&2
  exit 1
fi

# 1. Binary. Alpine has no cloudflared package, so fetch the static Go build.
if ! command -v cloudflared >/dev/null 2>&1; then
  apk add --no-cache ca-certificates >/dev/null 2>&1
  V=$(wget -qO- --timeout=10 https://api.github.com/repos/cloudflare/cloudflared/releases/latest 2>/dev/null \
      | grep -m1 '"tag_name"' | sed 's/[^0-9.]*//g' | tr -d ' ,"' | head -c 20)
  [ -z "$V" ] && V=2026.10.7
  wget -q -O /usr/local/bin/cloudflared \
    "https://github.com/cloudflare/cloudflared/releases/download/$V/cloudflared-linux-amd64" || {
      echo "Could not download cloudflared" >&2; exit 1
    }
  chmod 755 /usr/local/bin/cloudflared
fi
echo "cloudflared: $(cloudflared --version 2>&1 | head -1)"

# 2. Resolve the cert's own name to loopback so cloudflared can verify the
#    real Let's Encrypt certificate against the :8443 origin. Without this the
#    name resolves to the public IP, where nothing is forwarded.
if ! grep -qE "^127\.0\.0\.1[[:space:]]+$DOMAIN([[:space:]]|$)" /etc/hosts; then
  printf '\n# lab-tunnel: keep the dashboard HTTPS origin on loopback\n127.0.0.1 %s\n' "$DOMAIN" >> /etc/hosts
  echo "added /etc/hosts entry for $DOMAIN"
fi

# 3. Service.
cp "$OPS/lab-tunnel.initd" "$INIT"
chmod 755 "$INIT"
rc-update add lab-tunnel default >/dev/null 2>&1
rc-service lab-tunnel restart >/dev/null 2>&1

# 4. Status watcher on the same cron as everything else.
TASK="* * * * * $OPS/tunnel-status.sh"
touch /etc/crontabs/root
grep -Fq "$TASK" /etc/crontabs/root || printf '%s\n' "$TASK" >> /etc/crontabs/root
rc-service crond status >/dev/null 2>&1 || rc-service crond start >/dev/null 2>&1

# 5. Give the edge a moment to allocate a hostname.
i=0
while [ "$i" -lt 20 ]; do
  URL=$(grep -oE 'https://[a-zA-Z0-9-]+\.trycloudflare\.com' /var/log/lab-tunnel.log 2>/dev/null | tail -1)
  [ -n "$URL" ] && break
  i=$((i+1))
  sleep 2
done
"$OPS/tunnel-status.sh"
