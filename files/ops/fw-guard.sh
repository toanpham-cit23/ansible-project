#!/bin/sh
# Host firewall with a mandatory timed rollback guard.
#
#   fw-guard.sh apply     install rules; auto-rollback in 180 s unless confirmed
#   fw-guard.sh confirm   keep the rules permanently
#   fw-guard.sh guard     cron hook: enforce the deadline, never called by hand
#   fw-guard.sh rollback  remove everything we added, immediately
#   fw-guard.sh status    write the status file the dashboard reads
#
# Why the guard exists: this is the only path into the controller. One bad
# INPUT rule and the SSH session is gone for good. Nothing here is applied
# without a deadline file, and crond will undo it even if the session dies.
#
# Scope note: INPUT covers services bound on the host (sshd, SNMP). Docker's
# published ports are DNATed through FORWARD/DOCKER chains, so they are not
# affected by these rules - restricting them would also block the public HTTPS
# endpoint this firewall is meant to protect.
set -u

LAB=/root/lab
OPS=$LAB/ops
S=$OPS/status.py
D=$LAB/db/ops
CHAIN=LABFW
DEADLINE=$D/fw.deadline
CONFIRMED=$D/fw.confirmed
GUARD_SECONDS=180
CRON_LINE="* * * * * $OPS/fw-guard.sh guard"
mkdir -p "$D"

NOW=$(date -u '+%Y-%m-%dT%H:%M:%SZ')

write_status() {
  # $1 state, $2 ok, $3 message
  DL=""
  [ -f "$DEADLINE" ] && DL=$(cat "$DEADLINE")
  python3 "$S" firewall state="$1" ok="$2" message="$3" \
    chain="$CHAIN" deadline="${DL:-0}" guard_seconds="$GUARD_SECONDS" \
    checked_at="$NOW"
}

ensure_cron() {
  touch /etc/crontabs/root
  grep -Fq "$CRON_LINE" /etc/crontabs/root || {
    printf '%s\n' "$CRON_LINE" >> /etc/crontabs/root
  }
  rc-service crond status >/dev/null 2>&1 || rc-service crond start >/dev/null 2>&1
}

rollback() {
  iptables -D INPUT -j "$CHAIN" 2>/dev/null || true
  iptables -F "$CHAIN" 2>/dev/null || true
  iptables -X "$CHAIN" 2>/dev/null || true
  rm -f "$DEADLINE"
}

case "${1:-status}" in
  apply)
    rollback
    iptables -N "$CHAIN" 2>/dev/null || iptables -F "$CHAIN"
    # One bad rule must not leave a DROP behind with SSH still un-allowed, so
    # every append is checked and any failure rolls the whole thing back.
    add() {
      if ! iptables -A "$CHAIN" "$@" 2>/dev/null; then
        rollback
        write_status rolled_back false "Rule rejected by iptables - no rules left applied"
        echo "RULE FAILED - ROLLED BACK" >&2
        exit 1
      fi
    }
    # Never interfere with Docker's own bridge, loopback or replies to
    # connections this host opened (SNMP walks, NTP, package fetches).
    add -i lo -j ACCEPT
    add -i docker0 -j ACCEPT
    add -m state --state ESTABLISHED,RELATED -j ACCEPT
    # Management subnet keeps SSH reachable; the whole lab lives on it.
    add -p tcp --dport 22 -s 192.168.23.0/24 -j ACCEPT
    add -p udp --dport 53 -j ACCEPT
    add -p udp --dport 161 -j ACCEPT
    add -p tcp --dport 8080 -j ACCEPT
    add -p tcp --dport 8443 -j ACCEPT
    add -p icmp -j ACCEPT
    add -j DROP
    if ! iptables -I INPUT 1 -j "$CHAIN" 2>/dev/null; then
      rollback
      write_status rolled_back false "Could not attach the lab chain to INPUT"
      echo "ATTACH FAILED - ROLLED BACK" >&2
      exit 1
    fi
    rm -f "$CONFIRMED"
    echo $(( $(date +%s) + GUARD_SECONDS )) > "$DEADLINE"
    ensure_cron
    write_status pending true "Rules applied - auto-rollback in ${GUARD_SECONDS}s unless fw-guard.sh confirm is run"
    echo "APPLIED - confirm within ${GUARD_SECONDS}s"
    ;;

  confirm)
    if ! iptables -nL INPUT 2>/dev/null | grep -q "$CHAIN"; then
      write_status inactive false "Nothing to confirm; rules were rolled back"
      echo "NOTHING TO CONFIRM"
      exit 1
    fi
    touch "$CONFIRMED"
    rm -f "$DEADLINE"
    write_status active true "Rules confirmed and kept"
    echo "CONFIRMED"
    ;;

  guard)
    [ -f "$DEADLINE" ] || exit 0
    if [ -f "$CONFIRMED" ]; then
      rm -f "$DEADLINE"
      write_status active true "Rules confirmed and kept"
      exit 0
    fi
    if [ "$(date +%s)" -ge "$(cat "$DEADLINE")" ]; then
      rollback
      write_status rolled_back true "Guard expired without confirmation - original rules restored"
      echo "ROLLED BACK"
    fi
    ;;

  rollback)
    rollback
    rm -f "$CONFIRMED"
    write_status rolled_back true "Manual rollback - original rules restored"
    echo "ROLLED BACK"
    ;;

  status)
    if iptables -nL INPUT 2>/dev/null | grep -q "$CHAIN"; then
      if [ -f "$DEADLINE" ]; then
        write_status pending true "Awaiting confirmation"
      else
        write_status active true "Rules confirmed and kept"
      fi
    else
      write_status inactive true "No lab firewall rules active"
    fi
    ;;
  *)
    echo "usage: fw-guard.sh apply|confirm|guard|rollback|status" >&2
    exit 2
    ;;
esac
