#!/bin/sh
# The acceptance test from the task brief, run in one pass.
#
#   "a host in the Accounting VLAN cannot freely access the HR VLAN, while
#    still being able to access the Servers VLAN in accordance with the
#    defined access policies."
#
# Listeners are started inside this same invocation. Busybox nc exits after a
# single connection and a detached process dies with the SSH session, so a
# listener started earlier is gone by probe time and every "blocked" result
# becomes a false pass.
#
# Probe semantics (this is the part that matters):
#   CONNECTED / ConnectionRefused -> forwarded, ACL permitted
#   TimeoutError                  -> dropped,    ACL denied
set -u

pass=0; fail=0
ok()  { printf 'PASS  %s\n' "$1"; pass=$((pass+1)); }
bad() { printf 'FAIL  %s\n' "$1"; fail=$((fail+1)); }
say() { echo; echo "---- $* ----"; }

CLEANUP=""
cleanup() { [ -n "$CLEANUP" ] && kill $CLEANUP 2>/dev/null; }
trap cleanup EXIT INT TERM

say "start real listeners on the Servers host (VLAN 70)"
ip netns exec ns-srv python3 /root/vlanlab/srvlisten.py 80 443 22 3306 >/tmp/srv.log 2>&1 &
CLEANUP=$!
sleep 2
echo "  listening: $(ip netns exec ns-srv netstat -ltn 2>/dev/null | awk 'NR>2{print $4}' | tr '\n' ' ')"

# probe <ns> <ip> <port> -> prints CONNECTED | REFUSED | TIMEOUT
probe() {
    ip netns exec "$1" python3 - "$2" "$3" <<'EOF'
import socket, sys
ip, port = sys.argv[1], int(sys.argv[2])
s = socket.socket(); s.settimeout(2)
try:
    s.connect((ip, port)); print("CONNECTED")
except ConnectionRefusedError:
    print("REFUSED")
except Exception:
    print("TIMEOUT")
finally:
    s.close()
EOF
}

say "Accounting (VLAN 10) -> HR (VLAN 20) must be BLOCKED"
if ip netns exec ns-acct ping -c2 -W2 10.10.20.10 >/dev/null 2>&1; then
    bad "acct can still ping HR - ACL not effective"
else
    ok "acct cannot ping HR host 10.10.20.10"
fi

say "negative control - the same path must work from a permitted VLAN"
if ip netns exec ns-sales ping -c2 -W2 10.10.20.10 >/dev/null 2>&1; then
    ok "control: sales(40) CAN ping HR, so the acct block is a policy not a broken route"
else
    bad "control failed: sales cannot reach HR either - the acct block proves nothing"
fi

say "Accounting (VLAN 10) -> Servers (VLAN 70) per policy"
for p in 80 443; do
    r=$(probe ns-acct 10.10.70.10 $p)
    case $r in
        CONNECTED) ok "acct -> srv:$p ALLOWED (published service)" ;;
        *)         bad "acct -> srv:$p should be allowed, got $r" ;;
    esac
done
for p in 22 3306; do
    r=$(probe ns-acct 10.10.70.10 $p)
    case $r in
        TIMEOUT) ok "acct -> srv:$p DROPPED (unpublished service, service-specific ACL)" ;;
        *)       bad "acct -> srv:$p should be dropped, got $r" ;;
    esac
done

say "CCTV (VLAN 90) isolation from user VLANs"
r=$(probe ns-cctv 10.10.10.10 80 2>/dev/null)
if ip netns exec ns-cctv ping -c2 -W2 10.10.10.10 >/dev/null 2>&1; then
    bad "cctv can reach acct - should be isolated"
else
    ok "cctv cannot reach acct (VLAN 90 isolated)"
fi

say "policy result: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
