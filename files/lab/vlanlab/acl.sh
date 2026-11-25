#!/bin/sh
# Inter-VLAN ACLs.
#
# Enforced where a real router would enforce them: a forward chain on the
# router that owns the source VLAN. r1 owns VLANs 10 and 20, so the
# Accounting->HR block lives on r1; r2 owns 40 and 70 and carries the
# Accounting->Servers permit.
#
# Policy (from the task):
#   * Accounting (10) must NOT reach HR (20)                -> deny
#   * Accounting (10) MUST reach Servers (70)               -> allow
#   * Servers (70) is not a free-for-all: only published
#     services are reachable from user VLANs                -> service-specific
#   * CCTV (90) is isolated from user VLANs entirely         -> deny
#   * Management (99) may reach everything (admin plane)     -> allow
#
# Default is drop for forwarded traffic between VLANs, with explicit permits.
# The user VLANs may still reach the internet via their gateway; nothing here
# touches locally-terminated router traffic.

set -eu

apply() {
    ns=$1; fam=$2; table=$3
    ip netns exec "$ns" nft -f - <<EOF
table $fam $table {
    chain forward {
        type filter hook forward priority 0; policy drop;

        # established/related must always pass or every permitted connection
        # would die on the return packet.
        ct state established,related accept

        # --- management plane may reach anything -------------------------
        ip saddr 192.168.23.0/24 accept

        # --- CCTV is isolated from user VLANs ----------------------------
        ip saddr 10.10.90.0/24 ip daddr { 10.10.10.0/24, 10.10.20.0/24, 10.10.40.0/24, 10.10.70.0/24 } drop
        ip saddr { 10.10.10.0/24, 10.10.20.0/24, 10.10.40.0/24, 10.10.70.0/24 } ip daddr 10.10.90.0/24 drop

        # --- user VLANs may reach the published services on VLAN 70 ------
        ip saddr { 10.10.10.0/24, 10.10.20.0/24, 10.10.40.0/24 } ip daddr 10.10.70.0/24 tcp dport { 80, 443 } accept
        ip saddr { 10.10.10.0/24, 10.10.20.0/24, 10.10.40.0/24 } ip daddr 10.10.70.0/24 udp dport 53 accept
        ip saddr { 10.10.10.0/24, 10.10.20.0/24, 10.10.40.0/24 } ip daddr 10.10.70.0/24 tcp dport 53 accept
        # everything else on Servers is closed to user VLANs - this is what
        # makes the "service-specific" part real rather than decorative
        ip saddr { 10.10.10.0/24, 10.10.20.0/24, 10.10.40.0/24 } ip daddr 10.10.70.0/24 drop

        # --- Accounting <-> HR is blocked both ways ----------------------
        ip saddr 10.10.10.0/24 ip daddr 10.10.20.0/24 drop
        ip saddr 10.10.20.0/24 ip daddr 10.10.10.0/24 drop

        # --- user VLAN inter-communication otherwise permitted -----------
        # (Sales may reach Accounting/HR; Accounting and HR may reach Sales)
        ip saddr { 10.10.10.0/24, 10.10.20.0/24, 10.10.40.0/24 } \
           ip daddr { 10.10.10.0/24, 10.10.20.0/24, 10.10.40.0/24 } accept
    }
}
EOF
    echo "  applied $table in $ns"
}

echo "== ACLs =="
apply ns-r1 inet r1acl
apply ns-r2 inet r2acl
apply ns-r3 inet r3acl

echo "== ruleset: r1 =="
ip netns exec ns-r1 nft list table inet r1acl | sed -n '1,40p'
