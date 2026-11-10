#!/bin/sh
# Re-sync everything that depends on addressing after a network change.
#
# Run by the 5-minute cron and by the if-up hook below. Three different
# addresses are in play and only one of them is public:
#
#   eth0  192.168.23.10   static lab LAN - what lab clients connect to
#   eth1  192.168.220.x   DHCP, VMware NAT - what the host must forward to
#   public 118.69.10.174  the NAT gateway - what DuckDNS points at
#
# A new network changes the public IP and often the eth1 address. Neither
# needs a manual edit here: the A record is refreshed and the local DNS
# override follows eth0, so the trusted name keeps working inside the lab.
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

"$OPS/duckdns-update.sh" >/dev/null 2>&1
PUB=$(python3 -c "import json;print(json.load(open('$D/duckdns.json')).get('public_ip',''))" 2>/dev/null)

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

MSG="Forward public TCP 8443 to ${WAN_IP:-eth1}:8443, then to ${LAN_IP:-eth0}:8443"
python3 "$S" network ok=true state=synced \
  lan_ip="${LAN_IP:-unknown}" wan_ip="${WAN_IP:-unknown}" public_ip="${PUB:-unknown}" \
  local_override="$OVERRIDE" forward_hint="$MSG" checked_at="$NOW" \
  message="A record and local override follow address changes automatically"
