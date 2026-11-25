#!/bin/sh
# SNMP v2c -> v3 on one agent.
#
# The v3 user is created with `net-snmp-create-v3-user`, which handles the
# persistent /var/lib/net-snmp/snmpd.conf user database correctly. Writing
# `createUser` into snmpd.conf by hand only works on first start; on a second
# start the user already exists and the line is ignored, which silently leaves
# v2c working and looks like a success.
set -u
AUTH_PASS=${1:-LabMonAuth2026!}
PRIV_PASS=${2:-LabMonPriv2026!}
USER_NAME=${3:-labmon}

echo "== backup =="
cp -f /etc/snmp/snmpd.conf "/etc/snmp/snmpd.conf.v2bak-$(date +%Y%m%d-%H%M%S)"
ls -1 /etc/snmp/snmpd.conf.v2bak-* | tail -1

echo "== stop agent =="
rc-service snmpd stop >/dev/null 2>&1

echo "== enable snmpd at boot =="
rc-update add snmpd default 2>&1 | tail -1

echo "== write v3-only config =="
cat > /etc/snmp/snmpd.conf <<EOF
# SNMPv3 only - no v2c community of any kind.
# A v2c community travels the wire in cleartext and "public" is the first
# string a scanner tries, so it is removed rather than left as a fallback.
agentaddress udp:161
syslocation Data Center
syscontact admin@company.local

# Access is granted to the v3 user only, and only with auth+priv.
rouser $USER_NAME authpriv

master agentx
EOF
chmod 600 /etc/snmp/snmpd.conf
grep -c rocommunity /etc/snmp/snmpd.conf | sed 's/^/  rocommunity lines remaining: /'

echo "== create the v3 user (MD5/SHA + DES/AES supported by this build) =="
# net-snmp-create-v3-user needs the agent stopped and appends to the
# persistent config, not to snmpd.conf.
net-snmp-create-v3-user -ro -A "$AUTH_PASS" -a SHA -X "$PRIV_PASS" -x AES "$USER_NAME" 2>&1 | tail -6

echo "== start agent =="
rc-service snmpd start >/dev/null 2>&1
sleep 1
rc-service snmpd status 2>&1 | head -2
echo "== persistent user db =="
ls -l /var/lib/net-snmp/snmpd.conf 2>/dev/null
grep -c 'usmUser' /var/lib/net-snmp/snmpd.conf 2>/dev/null | sed 's/^/  usmUser entries: /'
