#!/bin/sh
# Docker test devices (L3 switch, router, switch) with a live SNMP agent.
#
# Why `docker exec` and not SSH: these containers sit on the `lab-users`
# macvlan. A macvlan child cannot be reached from the host that owns the
# parent NIC - ARP never comes back - so SSH and snmpget from the controller
# both time out. The dashboard gets their config through `docker exec`, which
# is still the real running config of that device. This is documented, not
# hidden: the macvlan topology needs a gateway before external SNMP works.
#
# Usage:
#   ./testnet.sh up      create the three containers and start SNMP
#   ./testnet.sh down    remove them
#   ./testnet.sh status  show container + SNMP state
#   ./testnet.sh config  print the running config of each device

set -eu

NET=lab-users
SUBNET=192.168.23.0/24
DEVICES="test-l3switch:192.168.23.51 test-router:192.168.23.52 test-switch:192.168.23.53"
WORK=/root/lab/docker/testnet
SNMPD=/usr/sbin/snmpd
LIBS="/usr/lib/libnetsnmp.so.45 /usr/lib/libnetsnmpagent.so.45 /usr/lib/libnetsnmpmibs.so.45"

ensure_net() {
  docker network inspect "$NET" >/dev/null 2>&1 || {
    echo "creating macvlan $NET"
    docker network create -d macvlan \
      --subnet "$SUBNET" --gateway 192.168.23.1 -o parent=eth0 "$NET"
  }
}

# The alpine image cannot fetch packages at runtime on this host (repo access
# is denied inside the container). Copy the already-installed host binaries in.
seed_snmp() {
  name=$1
  # Use the inventory name, not `hostname` inside the container: docker sets
  # the container hostname to a short ID, which would write "sysName 3a1f..."
  # and make every switch fail its compliance rule.
  label=$2
  docker cp "$SNMPD" "$name:/usr/sbin/snmpd" >/dev/null 2>&1 || true
  for lib in $LIBS; do
    # Replace symlinks with real files: docker cp follows them into a loop.
    real=$(readlink -f "$lib" 2>/dev/null || echo "$lib")
    short=$(basename "$lib")
    docker cp "$real" "$name:/usr/lib/$short" >/dev/null 2>&1 || true
  done
  docker exec -e LABEL="$label" "$name" sh -c '
    mkdir -p /etc/snmp /var/lib/snmp
    cat > /etc/snmp/snmpd.conf <<EOF
rocommunity public 192.168.23.0/24
sysLocation "Lab testnet"
sysContact admin@lab.local
sysName $LABEL
agentAddress udp:161
EOF
    pkill snmpd 2>/dev/null || true
    /usr/sbin/snmpd -c /etc/snmp/snmpd.conf -f >/tmp/snmpd.log 2>&1 &
  ' >/dev/null 2>&1 || true
}

case "${1:-status}" in
  up)
    mkdir -p "$WORK"
    ensure_net
    command -v snmpd >/dev/null 2>&1 || apk add --no-cache net-snmp >/dev/null
    for pair in $DEVICES; do
      name=${pair%%:*}; ip=${pair##*:}
      docker rm -f "$name" >/dev/null 2>&1 || true
      docker run -d --name "$name" --network "$NET" --ip "$ip" \
        alpine:3.20 sleep infinity >/dev/null
      seed_snmp "$name" "$name"
      echo "up: $name at $ip"
    done
    ;;
  down)
    for pair in $DEVICES; do
      docker rm -f "${pair%%:*}" >/dev/null 2>&1 || true
    done
    echo "down"
    ;;
  status)
    docker ps --format '{{.Names}} {{.Status}}' | grep -E '^test-' || echo 'no test devices'
    ;;
  config)
    for pair in $DEVICES; do
      name=${pair%%:*}
      echo "=== $name ==="
      echo "# hostname"; docker exec "$name" hostname 2>/dev/null || true
      echo "# snmpd.conf"; docker exec "$name" cat /etc/snmp/snmpd.conf 2>/dev/null || true
      echo "# interfaces"; docker exec "$name" ip -brief addr 2>/dev/null || true
    done
    ;;
  *)
    echo "usage: $0 up|down|status|config" >&2
    exit 2
    ;;
esac
