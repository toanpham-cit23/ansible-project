#!/bin/sh
# Re-sync everything that depends on addressing after a network change.
#
# Run by the 5-minute cron and by the if-up hook below. Three different
# addresses are in play and only one of them is public:
#
#   eth0  192.168.23.10   static lab LAN - what lab clients connect to
#   eth1  192.168.220.x   DHCP, VMware NAT - what the host must forward to
#   public 118.69.10.174  the NAT gateway
#
# A new network changes the public IP and often the eth1 address. The local
# DNS override follows eth0, so the trusted name keeps working inside the lab.
#
# The DuckDNS A record is refreshed only while the legacy Cloudflare tunnel
# still exists. That record existed to give the tunnel a stable origin; with
# the tunnel retired (see remove-legacy-tunnel.sh) the name points at a port
# nothing listens on, and updating it would just keep a dead name alive.
set -u

LAB=/root/lab
OPS=$LAB/ops
S=$OPS/status.py
D=$LAB/db/ops
NOW=$(date -u '+%Y-%m-%dT%H:%M:%SZ')

iface_ip() {
  ip -4 addr show "$1" 2>/dev/null | awk '/inet /{print $2}' | cut -d/ -f1 | head -1
}
LAN_IP=$(iface_ip eth0)
WAN_IP=$(iface_ip eth1)

# Legacy path only: keep the DuckDNS name current while the tunnel needs it.
if [ -x /etc/init.d/lab-tunnel ]; then
  "$OPS/duckdns-update.sh" >/dev/null 2>&1
fi

# Public IP is worth reporting either way, and it must not depend on DuckDNS
# being configured. Query it directly, falling back to the last known value.
PUB=$(wget -qO- --timeout=8 https://api.ipify.org 2>/dev/null | tr -d '\r\n' | head -c 45)
[ -n "$PUB" ] || PUB=$(cat "$D/duckdns.ip" 2>/dev/null)

# Keep the lab's own DNS pointing at wherever eth0 actually is. If the VM is
# moved and the lab subnet changes, the trusted name must not keep pointing at
# a dead 192.168.23.10.
DOMAIN=""
[ -f "$D/tls.domain" ] && DOMAIN=$(cat "$D/tls.domain")
OVERRIDE="none"
if [ -n "$DOMAIN" ] && [ -n "$LAN_IP" ]; then
  if docker exec -e LAB_DOMAIN="$DOMAIN" -e LAB_IP="$LAN_IP" lab-poller python3 -c "
import os, sqlite3
db = sqlite3.connect('/data/net.db')
db.execute('DELETE FROM dns_records WHERE name = ?', (os.environ['LAB_DOMAIN'],))
db.execute('INSERT INTO dns_records(name, ip, kind, ttl, note) VALUES (?,?,?,?,?)',
           (os.environ['LAB_DOMAIN'], os.environ['LAB_IP'], 'A', 60,
            'Local override for trusted HTTPS'))
db.commit()
" >/dev/null 2>&1; then
    OVERRIDE="$DOMAIN -> $LAN_IP"
  else
    OVERRIDE="update failed"
  fi
fi

MSG="Public access is served by Pangolin on :443; the in-lab entry point is ${LAN_IP:-eth0}:8443"
python3 "$S" network ok=true state=synced \
  lan_ip="${LAN_IP:-unknown}" wan_ip="${WAN_IP:-unknown}" public_ip="${PUB:-unknown}" \
  local_override="$OVERRIDE" forward_hint="$MSG" checked_at="$NOW" \
  message="Local DNS override follows eth0 automatically"
