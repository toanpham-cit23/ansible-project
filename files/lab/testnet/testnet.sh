#!/bin/sh
# Docker test devices (L3 switch, router, switch) with a live SNMP agent.
#
# Why the `mvl0` shim: a macvlan child is a *sibling* of the host NIC, not a
# child of it. The parent interface cannot talk to the children it hosts - the
# kernel routes the packet out of eth0, where the destination MAC is the
# child's own, and the frame never comes back. So the controller could not
# ping, snmpget or ssh the devices it was created to monitor, and the dashboard
# drew eleven healthy containers as a red outage.
#
# The fix is the standard one: a second macvlan interface on the same parent,
# in bridge mode, with its own address and a route to the container range.
# Traffic to a child then leaves by `mvl0`, which *is* a sibling, and the
# kernel handles it the way it handles any other host on the segment.
#
# Usage:
#   ./testnet.sh up      create the containers, start SNMP, install the shim
#   ./testnet.sh down    remove them
#   ./testnet.sh status  show container + SNMP + reachability state
#   ./testnet.sh config  print the running config of each device
#   ./testnet.sh shim    (re)install only the host-side macvlan shim

set -eu

NET=lab-users
SUBNET=192.168.23.0/24
PARENT=eth0
# Address for the shim interface. Deliberately outside the macvlan IP range
# (192.168.23.144/28) and outside the DHCP pool, and /32 so it does not claim
# a whole subnet on the wire.
SHIM_DEV=mvl0
SHIM_IP=192.168.23.200/32
# Everything the host must reach through the shim: the macvlan range plus the
# three test devices, which sit outside it because they predate the IPAM range.
SHIM_ROUTES="192.168.23.144/28 192.168.23.51/32 192.168.23.52/32 192.168.23.53/32"
DEVICES="test-l3switch:192.168.23.51 test-router:192.168.23.52 test-switch:192.168.23.53"
WORK=/root/lab/docker/testnet

# The host-side shim. Idempotent: every step checks first, so running it twice
# is harmless and it can be re-applied after a reboot.
install_shim() {
  ip link show "$SHIM_DEV" >/dev/null 2>&1 || \
    ip link add "$SHIM_DEV" link "$PARENT" type macvlan mode bridge
  ip addr show "$SHIM_DEV" | grep -q "${SHIM_IP%/*}" || \
    ip addr add "$SHIM_IP" dev "$SHIM_DEV"
  ip link set "$SHIM_DEV" up
  for r in $SHIM_ROUTES; do
    # `ip route show | grep` misses routes that exist but are printed with a
    # different metric or proto, so the add then fails with "RTNETLINK answers:
    # File exists" - and under `set -e` that killed the whole `up` before a
    # single container was created. `|| true` makes the step idempotent, which
    # is what it claims to be.
    ip route show | grep -q "^$r dev $SHIM_DEV" || ip route add "$r" dev "$SHIM_DEV" 2>/dev/null || true
  done
  echo "shim: $SHIM_DEV ${SHIM_IP} via $PARENT"
}

ensure_net() {
  docker network inspect "$NET" >/dev/null 2>&1 || {
    echo "creating macvlan $NET"
    docker network create -d macvlan \
      --subnet "$SUBNET" --gateway 192.168.23.1 -o parent="$PARENT" "$NET"
  }
}

# The test devices now run the same image as the simulated workstations
# (files/sim/Dockerfile): alpine plus openssh-server, net-snmp and an entrypoint
# that supervises both.
#
# They used to run `alpine:3.20 sleep infinity` with a host-built snmpd copied
# in at run time, because package repos are unreachable from inside a macvlan
# container. That had two consequences nobody saw until it was measured:
#   - no sshd at all, so the Terminal console on a test switch could not open;
#   - `sleep infinity` as PID 1 does not reap, so when snmpd crashed it stayed
#     <defunct> for ever and the device answered ping with a silent port 161.
#     That is the "reachable, unmonitored" state three devices were stuck in.
# Building the agent into the image removes both, and removes the copy step.
SIM_IMAGE=${SIM_IMAGE:-lab/simhost:1.0}
SIM_BUILD_DIR=${SIM_BUILD_DIR:-/root/lab-sim}

ensure_image() {
  docker image inspect "$SIM_IMAGE" >/dev/null 2>&1 && return 0
  echo "building $SIM_IMAGE from $SIM_BUILD_DIR"
  docker build -t "$SIM_IMAGE" "$SIM_BUILD_DIR" >/dev/null
}

case "${1:-status}" in
  up)
    mkdir -p "$WORK"
    ensure_net
    install_shim
    ensure_image
    for pair in $DEVICES; do
      name=${pair%%:*}; ip=${pair##*:}
      docker rm -f "$name" >/dev/null 2>&1 || true
      # LABEL is what snmpd publishes as sysName; the compliance rule compares
      # it with the inventory name, so it must be the inventory name and not
      # the container id docker assigns.
      docker run -d --name "$name" --network "$NET" --ip "$ip" \
        --restart unless-stopped \
        -e LABEL="$name" -e ROOT_PW="${DEV_PASS:-lab12345}" \
        -e ROUTERS="${ROUTERS:-192.168.23.11 192.168.23.12 192.168.23.13}" \
        -e PEER="${PEER:-192.168.23.11}" \
        "$SIM_IMAGE" >/dev/null
      echo "up: $name at $ip"
    done
    ;;
  shim)
    install_shim
    ;;
  down)
    for pair in $DEVICES; do
      docker rm -f "${pair%%:*}" >/dev/null 2>&1 || true
    done
    echo "down"
    ;;
  status)
    docker ps --format '{{.Names}} {{.Status}}' | grep -E '^test-' || echo 'no test devices'
    echo "--- reachability from this host (needs the shim)"
    for pair in $DEVICES; do
      ip=${pair##*:}
      if ping -c1 -w2 "$ip" >/dev/null 2>&1; then
        echo "  $ip reachable"
      else
        echo "  $ip UNREACHABLE - run '$0 shim'"
      fi
    done
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
    echo "usage: $0 up|down|status|config|shim" >&2
    exit 2
    ;;
esac
