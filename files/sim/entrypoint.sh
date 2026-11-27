#!/bin/sh
# PID 1 for a simulated host: sshd plus snmpd, with both watched.
#
# This exists because `sleep infinity` as PID 1 does not reap, and does not
# restart. When snmpd died inside test-switch it stayed `<defunct>` and the
# device kept answering ping while port 161 stayed silent - the dashboard
# showed "reachable, unmonitored" for ever and no amount of waiting brought
# SNMP back. A device that looks half-alive is the most misleading state a
# monitoring lab can produce.
#
# `exec` is deliberately NOT used at the end: this process has to stay alive
# and be the parent that reaps, otherwise we are back where we started.
set -u

LABEL="${LABEL:-$(hostname)}"
COMMUNITY="${SNMP_COMMUNITY:-public}"
NET="${SNMP_NET:-192.168.23.0/24}"
ROOT_PW="${ROOT_PW:-lab12345}"

# ---------------------------------------------------------------- sshd
# Password auth, because the dashboard stores a username and a password per
# device and opens a password session. There is no key distribution here and
# these are throwaway containers on an isolated segment.
mkdir -p /run/sshd
echo "root:${ROOT_PW}" | chpasswd
sed -i 's/^#*PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
sed -i 's/^#*PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config
grep -q '^PermitRootLogin' /etc/ssh/sshd_config || echo 'PermitRootLogin yes' >> /etc/ssh/sshd_config
grep -q '^PasswordAuthentication' /etc/ssh/sshd_config || echo 'PasswordAuthentication yes' >> /etc/ssh/sshd_config

# ---------------------------------------------------------------- snmpd
# sysName must be the inventory name. `hostname` inside the container is the
# container id, and writing that would make every switch fail its compliance
# rule ("sysName must equal the inventory name").
#
# Two traps here, both found by reading strace rather than by guessing:
#
#  1. net-snmp reads its compiled-in default config (/etc/snmp/snmpd.conf) AND
#     the file given with -c. Writing agentAddress into the default file and
#     also passing -c with an agentAddress makes snmpd bind the same socket
#     twice; the second bind returns EADDRINUSE and the daemon exits with
#     'Error opening specified endpoint'. That looked like a kernel permission
#     problem and was not one. So there is exactly ONE config file, the default
#     one, and snmpd is started with no -c at all.
#
#  2. `agentAddress udp:161` and `udp:0.0.0.0:161` both fail on this build,
#     while `udp:<the interface address>:161` works. Leaving agentAddress out
#     is not the answer either: it succeeds but binds 127.0.0.1 only, so the
#     agent is invisible from the segment - the silent-port-161 failure this
#     whole change exists to fix.
snmp_addr() {
  # busybox `ip` has no -br, and `ip -br a` printed nothing (which made an
  # earlier test look like "no address at all"). Parse the plain form.
  for dev in eth0 eth1; do
    a=$(ip -4 addr show "$dev" 2>/dev/null \
        | sed -n 's/.*inet \([0-9][0-9.]*\)\/.*/\1/p' | head -1)
    [ -n "$a" ] && { echo "$a"; return; }
  done
  echo ""
}

ADDR=$(snmp_addr)
if [ -n "$ADDR" ]; then
  AGENT="agentAddress udp:${ADDR}:161"
else
  echo "entrypoint: no IPv4 address found; snmpd will bind loopback only"
  AGENT="agentAddress udp:127.0.0.1:161"
fi

mkdir -p /etc/snmp
cat > /etc/snmp/snmpd.conf <<EOF
rocommunity ${COMMUNITY} ${NET}
sysLocation "Lab simulator"
sysContact admin@lab.local
sysName ${LABEL}
${AGENT}
EOF

# MIB files are not installed (they are ~30 MB and nothing here resolves OIDs
# by name), so silence the "Cannot find module" chatter instead of letting it
# fill the log on every start.
export MIBS=""

start_sshd() {
  /usr/sbin/sshd -D -e &
  SSHD=$!
}
start_snmpd() {
  # No -c: the config lives at the compiled-in default path, and passing -c as
  # well makes snmpd read BOTH files and bind agentAddress twice (EADDRINUSE).
  /usr/sbin/snmpd -f -Lo &
  SNMPD=$!
}

echo "entrypoint: ${LABEL} starting sshd and snmpd"
start_sshd
start_snmpd

# ---------------------------------------------------------------- keepalive
# A workstation is not interesting to a switch until it sends something. The
# routers learn a MAC from traffic, and an idle container ages out of their ARP
# table - which the dashboard then reports, correctly, as a host that stopped
# answering. So the simulated host keeps touching the network, exactly like a
# real PC with a desktop OS does.
#
# The `timeout` around nc is not decoration: busybox nc does not reliably honour
# -w on a connect that never completes, and a keepalive loop blocked inside nc
# stops pinging - the host then ages out while its container still looks
# healthy. That failure already happened once, to all eight at the same time.
keepalive() {
  while :; do
    for t in ${ROUTERS}; do
      ping -c1 -W1 "$t" >/dev/null 2>&1
    done
    [ -n "${PEER:-}" ] && timeout 2 nc -z "$PEER" 22 >/dev/null 2>&1
    sleep "${KEEPALIVE_S:-5}"
  done
}

if [ -n "${ROUTERS:-}" ]; then
  keepalive &
  KEEP=$!
else
  KEEP=""
fi

trap 'kill $SSHD $SNMPD 2>/dev/null; [ -n "$KEEP" ] && kill $KEEP 2>/dev/null; exit 0' TERM INT

# Supervise. Two jobs, and both matter:
#   reap   - `wait $PID` collects the dead child. Without it the process stays
#            <defunct> for the life of the container, which is exactly the
#            state that made test-switch look alive-but-unmonitored.
#   restart - bring the daemon back, so a crash costs seconds of telemetry
#            instead of a permanent gap.
# Polling every 2 s instead of using `wait -n`: busybox ash has it, but this
# script also has to run under a plain `sh` in a rescue container, and a
# missing builtin would turn the supervisor into a busy loop.
while :; do
  if ! kill -0 "$SSHD" 2>/dev/null; then
    echo "entrypoint: sshd exited, restarting"
    wait "$SSHD" 2>/dev/null || true
    start_sshd
  fi
  if ! kill -0 "$SNMPD" 2>/dev/null; then
    echo "entrypoint: snmpd exited, restarting"
    wait "$SNMPD" 2>/dev/null || true
    start_snmpd
  fi
  sleep 2
done
