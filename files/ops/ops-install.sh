#!/bin/sh
# Install the ops layer on the controller. Run once over SSH; safe to re-run.
set -u
LAB=/root/lab
OPS=$LAB/ops
D=$LAB/db/ops

mkdir -p "$D" "$LAB/backups" "$LAB/db/certs" "$LAB/tmp"
chmod +x "$OPS"/*.sh "$OPS"/*.py 2>/dev/null || true
chmod 600 "$OPS/duckdns.env"
# The API runs as uid 10001 and must be able to leave request markers here.
chown 10001:10001 "$D" 2>/dev/null || true
chmod 755 "$D"

# Cron lines, added once. crond is already running on this controller.
touch /etc/crontabs/root
add_line() {
  grep -Fq "$1" /etc/crontabs/root || printf '%s\n' "$1" >> /etc/crontabs/root
}
# network-sync also runs duckdns-update, so there is no separate DuckDNS line.
add_line "*/5 * * * * $OPS/network-sync.sh"
add_line "* * * * * $OPS/backup-worker.sh"
add_line "0 3 1 1,7 * $OPS/retention.sh --apply"
# acme.sh --install-cert does NOT reliably add a cron entry on Alpine; without
# this the certificate would silently expire 90 days after issuance.
add_line "17 2 * * * /usr/bin/acme.sh --cron --home /root/.acme.sh >/dev/null 2>&1"
rc-service crond status >/dev/null 2>&1 || rc-service crond start >/dev/null 2>&1

# React immediately when the upstream link comes back on a new network, rather
# than waiting up to five minutes for the next cron tick.
mkdir -p /etc/network/if-up.d
cat > /etc/network/if-up.d/duckdns <<'HOOK'
#!/bin/sh
[ "$IFACE" = "eth1" ] || exit 0
/root/lab/ops/network-sync.sh >/dev/null 2>&1
HOOK
chmod +x /etc/network/if-up.d/duckdns

# Seed every status file so the dashboard is never blank on first load.
"$OPS/network-sync.sh"     >/dev/null 2>&1
# Re-running this installer must not needlessly reissue a valid certificate
# and restart the API. acme-issue.sh only runs when no certificate exists.
if [ ! -f "$LAB/db/certs/fullchain.pem" ]; then
  "$OPS/acme-issue.sh"     >/dev/null 2>&1
fi
"$OPS/fw-guard.sh" status  >/dev/null 2>&1
"$OPS/retention.sh"        >/dev/null 2>&1
"$OPS/snapshot.sh"         >/dev/null 2>&1

echo "installed. status files:"
ls -1 "$D"/*.json 2>/dev/null | sed 's#.*/##'
