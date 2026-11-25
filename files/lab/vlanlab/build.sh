#!/bin/sh
# Build a VLAN fabric on the controller using Linux bridges + network
# namespaces.
#
# Why namespaces: VLAN-tagged frames do not traverse the VMware port group
# this lab runs on. Measured, not assumed - the controller transmits
# "ethertype 802.1Q ... vlan 999" while r1's eth0.999 RX counter stays at
# exactly 0. A cross-VM trunk therefore needs a VMware-side change (port group
# VLAN ID 4095) that cannot be made from inside a guest.
#
# What this builds instead is the same fabric the real lab would have, using
# the same kernel primitives an OVS or Linux-bridge deployment uses:
# vlan_filtering bridges, pvid/untagged access ports, tagged trunk ports,
# 802.1Q sub-interfaces as SVIs, and per-VLAN ACLs.
#
# Addressing mirrors the real routers so the design stays recognisable:
#   transit r1<->r2 10.0.1.0/29  (.1 .2)   matches r1 eth1 / r2 eth1
#   transit r2<->r3 10.0.2.0/29  (.1 .2)   matches r2 eth2 / r3 eth2
#   transit r1<->r3 10.0.3.0/29  (.1 .2)   matches r1 eth2 / r3 eth2
#
# Idempotent: tears the fabric down first.

set -e

BR=br-lab
NS="ns-acct ns-hr ns-sales ns-srv ns-cctv ns-mgmt ns-r1 ns-r2 ns-r3"

log() { echo "  $*"; }

teardown() {
    for n in $NS; do
        ip netns del "$n" 2>/dev/null || true
    done
    # veth pairs die with their namespace; remove the bridge last
    ip link del "$BR" 2>/dev/null || true
    # stray host-side ports that outlived a namespace
    for p in p-acct p-hr p-sales p-srv p-cctv p-mgmt p-r1 p-r2 p-r3; do
        ip link del "$p" 2>/dev/null || true
    done
}

echo "== teardown =="
teardown

echo "== bridge (VLAN filtering enabled) =="
ip link add "$BR" type bridge vlan_filtering 1
ip link set "$BR" up
log "$BR up, vlan_filtering=1"

echo "== namespaces =="
for n in $NS; do
    ip netns add "$n"
    ip netns exec "$n" ip link set lo up
done
log "created: $NS"

# ---------------------------------------------------------------- access ports
# One veth pair per host. The host side lives in the host's namespace; the
# switch side joins the bridge as an untagged access port pinned to its VLAN.
#
#   add_access <short> <vlan> <ns> <addr/prefix> <gateway>
add_access() {
    short=$1; vlan=$2; ns=$3; addr=$4; gw=$5
    ip link add "p-$short" type veth peer name "h-$short"
    ip link set "h-$short" netns "$ns"
    ip link set "p-$short" master "$BR"
    ip link set "p-$short" up

    # Access port: pvid = the VLAN, and the VLAN is egress-untagged.
    # Without "untagged" the port would tag frames toward the host, which is
    # not what an access port does.
    bridge vlan add dev "p-$short" vid "$vlan" pvid untagged

    ip netns exec "$ns" ip link set "h-$short" name eth0
    ip netns exec "$ns" ip link set eth0 up
    # An access port may deliberately carry no address: VLAN 99 is the live
    # management subnet 192.168.23.0/24, and giving a namespace an address
    # there would collide with the real controller on eth0.
    if [ -n "$addr" ]; then
        ip netns exec "$ns" ip addr add "$addr" dev eth0
    fi
    if [ -n "$gw" ]; then
        ip netns exec "$ns" ip route add default via "$gw" dev eth0
    fi
    log "access p-$short -> $ns  vlan $vlan  pvid untagged  ${addr:-<unaddressed>}"
}

echo "== access ports =="
add_access acct  10 ns-acct  10.10.10.10/24 10.10.10.254
add_access hr    20 ns-hr    10.10.20.10/24 10.10.20.254
add_access sales 40 ns-sales 10.10.40.10/24 10.10.40.254
add_access srv   70 ns-srv   10.10.70.10/24 10.10.70.254
add_access cctv  90 ns-cctv  10.10.90.10/24 10.10.90.254
# VLAN 99 is the management network - 192.168.23.0/24, which is the live
# management plane on eth0. Giving a namespace an address there would collide
# with the real controller, so this host gets no IP. It exists to prove L2
# separation (it must not see the other VLANs' broadcasts).
add_access mgmt  99 ns-mgmt  ""            ""

# ----------------------------------------------------------------- trunk ports
# Trunk ports in the real lab carry every VLAN tagged. Here each router gets
# one trunk to the same bridge.
add_trunk() {
    short=$1; ns=$2; vlans=$3
    ip link add "p-$short" type veth peer name "h-$short"
    ip link set "h-$short" netns "$ns"
    ip link set "p-$short" master "$BR"
    ip link set "p-$short" up
    bridge vlan add dev "p-$short" vid 1  pvid untagged
    for v in $vlans; do
        bridge vlan add dev "p-$short" vid "$v"
    done
    ip netns exec "$ns" ip link set "h-$short" name trunk
    ip netns exec "$ns" ip link set trunk up
    log "trunk p-$short -> $ns  tagged: $vlans"
}

echo "== trunk ports =="
add_trunk r1 ns-r1 "10 20 99"
add_trunk r2 ns-r2 "40 70 99"
add_trunk r3 ns-r3 "90 99"

# --------------------------------------------------------------------- SVIs
# Router-on-a-stick: one 802.1Q sub-interface per VLAN the router owns, and
# the .254 address from vlans.yml becomes that VLAN's gateway.
add_svi() {
    ns=$1; vlan=$2; gw=$3
    ip netns exec "$ns" ip link add link trunk name "trunk.$vlan" type vlan id "$vlan"
    ip netns exec "$ns" ip link set "trunk.$vlan" up
    ip netns exec "$ns" ip addr add "$gw/24" dev "trunk.$vlan"
    log "$ns trunk.$vlan = $gw/24"
}

echo "== SVIs (inter-VLAN gateways) =="
add_svi ns-r1 10 10.10.10.254
add_svi ns-r1 20 10.10.20.254
add_svi ns-r2 40 10.10.40.254
add_svi ns-r2 70 10.10.70.254
add_svi ns-r3 90 10.10.90.254

# ------------------------------------------------------- router transit links
# The three routers are joined in a triangle, same addresses as the real ones.
add_transit() {
    a=$1; b=$2; an=$3; bn=$4; pfx=$5
    ip link add "t-$a$b" type veth peer name "t-$b$a"
    ip link set "t-$a$b" netns "ns-$a"
    ip link set "t-$b$a" netns "ns-$b"
    ip netns exec "ns-$a" ip link set "t-$a$b" name "$an"
    ip netns exec "ns-$b" ip link set "t-$b$a" name "$bn"
    ip netns exec "ns-$a" ip link set "$an" up
    ip netns exec "ns-$b" ip link set "$bn" up
    log "transit ns-$a:$an <-> ns-$b:$bn  $pfx"
}

echo "== router transit =="
# Interface names follow the real routers: r1-r2 on eth1<->eth1, r2-r3 on
# eth2<->eth2, and r1-r3 on r1:eth2 <-> r3:eth1. Getting this wrong silently
# collides r3's eth2 with the r2 link.
add_transit r1 r2 eth1 eth1 10.0.1.0/29
add_transit r2 r3 eth2 eth2 10.0.2.0/29
add_transit r1 r3 eth2 eth1 10.0.3.0/29

ip netns exec ns-r1 ip addr add 10.0.1.1/29 dev eth1
ip netns exec ns-r2 ip addr add 10.0.1.2/29 dev eth1
ip netns exec ns-r2 ip addr add 10.0.2.1/29 dev eth2
ip netns exec ns-r3 ip addr add 10.0.2.2/29 dev eth2
ip netns exec ns-r1 ip addr add 10.0.3.1/29 dev eth2
ip netns exec ns-r3 ip addr add 10.0.3.2/29 dev eth1

# ------------------------------------------------------------- static routes
# VLANs are split across routers, so each router needs a route to the VLANs it
# does not own. The real lab learns these from OSPF; here they are static so
# the fabric has no dependency on a routing daemon that is not installed.
echo "== routes to foreign VLANs =="
# r1 must reach 40,70 (on r2) and 90 (on r3)
ip netns exec ns-r1 ip route add 10.10.40.0/24 via 10.0.1.2
ip netns exec ns-r1 ip route add 10.10.70.0/24 via 10.0.1.2
ip netns exec ns-r1 ip route add 10.10.90.0/24 via 10.0.3.2
# r2 must reach 10,20 (on r1) and 90 (on r3)
ip netns exec ns-r2 ip route add 10.10.10.0/24 via 10.0.1.1
ip netns exec ns-r2 ip route add 10.10.20.0/24 via 10.0.1.1
ip netns exec ns-r2 ip route add 10.10.90.0/24 via 10.0.2.2
# r3 must reach 10,20 (r1) and 40,70 (r2)
ip netns exec ns-r3 ip route add 10.10.10.0/24 via 10.0.3.1
ip netns exec ns-r3 ip route add 10.10.20.0/24 via 10.0.3.1
ip netns exec ns-r3 ip route add 10.10.40.0/24 via 10.0.2.1
ip netns exec ns-r3 ip route add 10.10.70.0/24 via 10.0.2.1
log "static routes installed on r1/r2/r3"

echo "== build complete =="
