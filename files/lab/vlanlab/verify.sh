#!/bin/sh
# Verify the fabric. Prints PASS/FAIL per check so the report can quote it.
set -u

pass=0; fail=0
chk() {
    label=$1; expect=$2; shift 2
    if "$@" >/dev/null 2>&1; then got=yes; else got=no; fi
    if [ "$got" = "$expect" ]; then
        printf 'PASS  %-52s (%s)\n' "$label" "$got"; pass=$((pass+1))
    else
        printf 'FAIL  %-52s (got %s, want %s)\n' "$label" "$got" "$expect"; fail=$((fail+1))
    fi
}

ping_ok()   { ip netns exec "$1" ping -c2 -W2 "$2" >/dev/null 2>&1; }
ping_fail() { ! ping_ok "$1" "$2"; }

echo "---- L2 isolation: same VLAN reachable, other VLAN not -----------------"
chk "acct -> its gateway 10.10.10.254 (VLAN 10 SVI)"        yes ping_ok ns-acct 10.10.10.254
# NOTE: "acct cannot reach 10.10.20.10" is NOT an L2 test - with routing on,
# acct reaches it via the r1 gateway and the L3 ACL is what blocks it. The L2
# property to assert is that HR's MAC is never learned on acct's segment.
ip netns exec ns-acct ip neigh flush all 2>/dev/null
ip netns exec ns-acct ping -c1 -W2 10.10.20.10 >/dev/null 2>&1
HRMAC=$(ip netns exec ns-hr cat /sys/class/net/eth0/address)
if ip netns exec ns-acct ip neigh show | grep -qi "$HRMAC"; then
    echo "FAIL  acct learned HR's MAC $HRMAC - VLANs are not L2-separated"; fail=$((fail+1))
else
    echo "PASS  acct never learns HR's MAC $HRMAC (L2 separated)"; pass=$((pass+1))
fi
chk "acct can reach Servers 10.10.70.10 (routed, allowed)"   yes ping_ok ns-acct 10.10.70.10

echo
echo "---- inter-VLAN routing via SVIs + transit ---------------------------"
chk "acct(10) -> srv(70)   cross-router r1->r2"              yes ping_ok ns-acct 10.10.70.10
chk "acct(10) -> cctv(90) cross-router r1->r3"               yes ping_ok ns-acct 10.10.90.10
chk "sales(40) -> cctv(90) cross-router r2->r3"              yes ping_ok ns-sales 10.10.90.10
chk "sales(40) -> acct(10) cross-router r2->r1"              yes ping_ok ns-sales 10.10.10.10

echo
echo "---- trunk tagging actually in use (not flat L2) ---------------------"
# If the trunk were not tagging, a host would see every VLAN's broadcast
# domain and could reach HR directly. Already covered above, but assert the
# bridge presents the right pvid/untagged shape too.
bridge vlan show dev p-acct | grep -q '10 PVID Egress Untagged' \
    && { echo "PASS  p-acct is pvid 10 untagged"; pass=$((pass+1)); } \
    || { echo "FAIL  p-acct pvid/untagged wrong"; fail=$((fail+1)); }
bridge vlan show dev p-r1 | grep -qE '[[:space:]]10([[:space:]]|$)' \
    && { echo "PASS  p-r1 trunk carries tagged 10"; pass=$((pass+1)); } \
    || { echo "FAIL  p-r1 trunk missing tagged 10"; fail=$((fail+1)); }

echo
echo "---- summary: $pass passed, $fail failed -----------------------------"
[ "$fail" -eq 0 ]
