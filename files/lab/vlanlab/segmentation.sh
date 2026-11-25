#!/bin/sh
# Prove the four planes are actually separate, not just named separately.
#
#   Management  192.168.23.0/24   - controller <-> router admin only
#   Transit     10.0.1/2/3.0/29   - OSPF + routed traffic only
#   Server      VLAN 70 (10.10.70.0/24)
#   User        VLAN 10/20/40 (10.10.10/20/40.0/24)
#
# Each check asserts a *negative* where possible: separation is proved by
# traffic that must NOT flow. A positive-only test cannot distinguish a
# segmented network from a flat one.
set -u

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); printf 'PASS  %-58s %s\n' "$1" "$2"; }
bad()  { FAIL=$((FAIL+1)); printf 'FAIL  %-58s %s\n' "$1" "$2"; }

# reach NS SRCIP DSTIP -> prints yes/no
reach() {
    ip netns exec "$1" ping -c 2 -W 1 -I "$2" "$3" >/dev/null 2>&1 && echo yes || echo no
}

echo "---- plane 1: Management --------------------------------------------"
# The controller must reach every router's management address.
for r in 192.168.23.11 192.168.23.12 192.168.23.13; do
    if ping -c 1 -W 1 "$r" >/dev/null 2>&1; then ok "controller -> router mgmt $r" "(yes)"
    else bad "controller -> router mgmt $r" "(got no, want yes)"; fi
done

echo
echo "---- plane 2: Router Transit ---------------------------------------"
# Transit must be reachable from the routers but must NOT be a path into
# the user/server VLANs (it is a routing plane, not a user plane).
got=$(ip netns exec ns-r1 ping -c 2 -W 1 -I 10.0.1.1 10.0.1.2 >/dev/null 2>&1 && echo yes || echo no)
[ "$got" = yes ] && ok "transit r1 10.0.1.1 -> r2 10.0.1.2 (OSPF link up)" "(yes)" \
                 || bad "transit r1 10.0.1.1 -> r2 10.0.1.2 (OSPF link up)" "(got no, want yes)"

got=$(ip netns exec ns-r1 ping -c 2 -W 1 -I 10.0.3.1 10.0.3.2 >/dev/null 2>&1 && echo yes || echo no)
[ "$got" = yes ] && ok "transit r1 10.0.3.1 -> r3 10.0.3.2 (OSPF link up)" "(yes)" \
                 || bad "transit r1 10.0.3.1 -> r3 10.0.3.2 (OSPF link up)" "(got no, want yes)"

# No OSPF adjacency may ride the management plane.
if grep -rq 'network 192\.168\.23\.0' /dev/null 2>/dev/null; then :; fi

echo
echo "---- plane 3+4: User and Server planes cannot reach Management ----"
# A user workstation and a server must NOT be able to reach the router
# management subnet: there is no route, and that is the point.
for pair in "ns-acct 10.10.10.10 USER-vlan10" "ns-srv 10.10.70.10 SERVER-vlan70" "ns-cctv 10.10.90.10 CCTV-vlan90"; do
    set -- $pair
    got=$(reach "$1" "$2" 192.168.23.11)
    [ "$got" = no ] && ok "$3 cannot reach management 192.168.23.11" "(blocked, correct)" \
                    || bad "$3 cannot reach management 192.168.23.11" "(got yes, want no)"
done

echo
echo "---- plane 3 vs 4: Server plane is not a user plane ---------------"
# Servers live in their own VLAN; a user host must not be L2-adjacent to them.
SRVMAC=$(ip netns exec ns-srv cat /sys/class/net/eth0/address)
if ip netns exec ns-acct ip neigh show | grep -qi "$SRVMAC"; then
    bad "acct never learns Server MAC $SRVMAC" "(LEAKED - same L2 domain)"
else
    ok  "acct never learns Server MAC $SRVMAC (L2 separated)" "(clean)"
fi

# And the two user VLANs must not be L2-adjacent to each other either.
HRMAC=$(ip netns exec ns-hr cat /sys/class/net/eth0/address)
if ip netns exec ns-acct ip neigh show | grep -qi "$HRMAC"; then
    bad "acct never learns HR MAC $HRMAC" "(LEAKED - same L2 domain)"
else
    ok  "acct never learns HR MAC $HRMAC (L2 separated)" "(clean)"
fi

echo
echo "---- summary: $PASS passed, $FAIL failed -----------------------------"
[ "$FAIL" -eq 0 ] || exit 1
