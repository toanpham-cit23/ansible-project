#!/bin/sh
# Run the whole VLAN fabric exercise in the correct order and record results.
#
#   build   -> fabric exists
#   verify  -> raw routing works, with NO acl in the way
#   acl     -> policy applied
#   policy  -> the acceptance test from the brief
#
# The order is not cosmetic. verify.sh asserts that every VLAN can reach every
# other VLAN; that is what proves routing works. Once ACLs are loaded the same
# pings must FAIL, and a script that ran them afterwards would report false
# failures. Conversely, running policy.sh without ACLs would report false
# passes. So: routing is proven first, then narrowed.
set -u

LOG=/root/vlanlab/results.txt
: > "$LOG"

run() {
    label=$1; shift
    echo "" | tee -a "$LOG"
    echo "===================== $label =====================" | tee -a "$LOG"
    "$@" 2>&1 | tee -a "$LOG"
    rc=$?
    echo "[$label exit=$rc]" | tee -a "$LOG"
    return $rc
}

echo "== 1. build =="
sh /root/vlanlab/build.sh | tail -4 | tee -a "$LOG"

echo "== 2. routing, ACLs absent =="
sh /root/vlanlab/verify.sh 2>&1 | tee -a "$LOG"
VRC=$?

echo "== 3. apply ACLs =="
sh /root/vlanlab/acl.sh 2>&1 | head -5 | tee -a "$LOG"

echo "== 4. acceptance policy =="
sh /root/vlanlab/policy.sh 2>&1 | tee -a "$LOG"
PRC=$?

echo "" | tee -a "$LOG"
echo "===================== SUMMARY =====================" | tee -a "$LOG"
echo "routing-before-acl exit = $VRC" | tee -a "$LOG"
echo "policy-after-acl  exit = $PRC" | tee -a "$LOG"
