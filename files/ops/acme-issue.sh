#!/bin/sh
# Issue and install a trusted Let's Encrypt certificate for the DuckDNS name.
#
# DNS-01 is used on purpose: the controller sits behind NAT with no inbound
# 80/443, so an HTTP-01 challenge can never complete. DuckDNS accepts a TXT
# record through the same update API, which is what acme.sh's dns_duckdns hook
# drives. No port forwarding is needed to *get* the certificate (serving the
# site to the internet still needs one).
#
# Run on the controller over SSH. Refuses to do anything until the token and
# subdomain in duckdns.env are real.
set -u

LAB=/root/lab
OPS=$LAB/ops
S=$OPS/status.py
CERTDIR=$LAB/db/certs

. "$OPS/duckdns.env" 2>/dev/null || { DUCKDNS_DOMAIN=""; DUCKDNS_TOKEN=""; ACME_EMAIL=""; }

NOW=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
DOMAIN="${DUCKDNS_DOMAIN}.duckdns.org"
mkdir -p "$CERTDIR" "$LAB/db/ops"

if [ -z "${DUCKDNS_DOMAIN:-}" ] || [ -z "${DUCKDNS_TOKEN:-}" ]; then
  python3 "$S" tls ok=false state=unconfigured domain="" expires="" checked_at="$NOW" \
    message="DuckDNS token and subdomain are required before a certificate can be issued"
  exit 0
fi

if ! command -v acme.sh >/dev/null 2>&1; then
  apk add --no-cache acme.sh >/dev/null 2>&1 || {
    python3 "$S" tls ok=false state=error domain="$DOMAIN" checked_at="$NOW" \
      message="Could not install acme.sh"
    exit 1
  }
fi
ACME=$(command -v acme.sh || echo "/usr/lib/acme.sh/acme.sh")

# Both names appear across acme.sh releases; exporting both is harmless.
export DuckDNS_Token="$DUCKDNS_TOKEN"
export DuckDNS_Domain="$DOMAIN"
export DUCKDNS_Token="$DUCKDNS_TOKEN"
export DUCKDNS_Domain="$DOMAIN"

EMAIL_ARG=""
[ -n "${ACME_EMAIL:-}" ] && EMAIL_ARG="--accountemail $ACME_EMAIL"

# shellcheck disable=SC2086
"$ACME" --issue --server letsencrypt --dns dns_duckdns -d "$DOMAIN" \
  --keylength ec-256 $EMAIL_ARG >"$LAB/db/ops/acme.log" 2>&1 || {
  DETAIL=$(tail -3 "$LAB/db/ops/acme.log" 2>/dev/null | tr '\r\n' ' ' | cut -c1-160)
  python3 "$S" tls ok=false state=error domain="$DOMAIN" checked_at="$NOW" \
    message="ACME challenge failed - ${DETAIL:-see /root/lab/db/ops/acme.log}"
  exit 1
}

# shellcheck disable=SC2086
"$ACME" --install-cert -d "$DOMAIN" \
  --fullchain-file "$CERTDIR/fullchain.pem" \
  --key-file "$CERTDIR/key.pem" \
  --reloadcmd "cd /root/lab && docker compose restart api" >>"$LAB/db/ops/acme.log" 2>&1 || {
  python3 "$S" tls ok=false state=error domain="$DOMAIN" checked_at="$NOW" \
    message="Certificate issued but install failed - see acme.log"
  exit 1
}

# The API runs as uid 10001 inside the container and reads /data/certs.
chown -R 10001:10001 "$CERTDIR"
chmod 600 "$CERTDIR/key.pem"
chmod 644 "$CERTDIR/fullchain.pem"

EXPIRES=""
if [ -f "$CERTDIR/fullchain.pem" ]; then
  EXPIRES=$(openssl x509 -enddate -noout -in "$CERTDIR/fullchain.pem" 2>/dev/null | cut -d= -f2-)
  echo "$DOMAIN" > "$LAB/db/ops/tls.domain"

  # A certificate for a DuckDNS name is not trusted when the browser is pointed
  # at 192.168.23.10. Publish the same name in the lab's own DNS responder so
  # clients inside the lab resolve it straight to the controller and get a
  # matching, trusted name on https://<domain>:8443.
  docker exec -e LAB_DOMAIN="$DOMAIN" lab-poller python3 -c "
import os, sqlite3
d = os.environ['LAB_DOMAIN']
db = sqlite3.connect('/data/net.db')
db.execute('DELETE FROM dns_records WHERE name = ?', (d,))
db.execute('INSERT INTO dns_records(name, ip, kind, ttl, note) VALUES (?,?,?,?,?)',
           (d, '192.168.23.10', 'A', 60, 'Local override for trusted HTTPS'))
db.commit()
" >/dev/null 2>&1

  (cd "$LAB" && docker compose restart api) >/dev/null 2>&1
fi

python3 "$S" tls ok=true state=issued domain="$DOMAIN" \
  expires="$EXPIRES" issued_at="$NOW" checked_at="$NOW" \
  message="Trusted certificate installed; HTTPS on port 8443"
