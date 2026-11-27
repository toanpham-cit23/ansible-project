#!/bin/sh
# Remove the Cloudflare Quick Tunnel and DuckDNS fallback, keeping dpdns.org.
#
# RUN ON THE CONTROLLER. Safe to re-run.
#
#   ./remove-legacy-tunnel.sh --check     # report only, change nothing
#   ./remove-legacy-tunnel.sh             # remove, with backups
#   ./remove-legacy-tunnel.sh --rollback  # put everything back
#
# What this does NOT touch, on purpose:
#   - the Let's Encrypt certificate in /root/lab/db/certs
#   - the TLS listener on :8443
#   - /etc/hosts or the local DNS record for the dpdns name
#   - acme.sh itself
#
# Two different certificates exist. Keep them apart:
#
#   public  :443   issued by Pangolin at the 25/11 cutover for
#                  *.ansible.dpdns.org (valid to 23/02/2027). Pangolin renews
#                  it; nothing on this controller is involved.
#   controller     /root/lab/db/certs, issued by acme.sh's dns_duckdns hook for
#     :8443        <sub>.duckdns.org. This one needs duckdns.env to renew.
#
# So removing the DuckDNS *tunnel* is safe, and the public site's certificate
# is unaffected either way. Removing duckdns.env only stops renewal of the
# controller's own :8443 certificate - the one the in-lab DNS override points
# at. The script therefore keeps duckdns.env unless you pass
# --drop-duckdns-credentials, and says what breaks when you do.
#
# Exit: 0 ok, 1 something failed, 2 refused (precondition not met).

set -u

LAB=/root/lab
OPS=$LAB/ops
BACKUP=$LAB/.local-backups/tunnel-removal-$(date -u '+%Y%m%dT%H%M%SZ')
MODE=remove
DROP_CREDS=0

for a in "$@"; do
  case "$a" in
    --check)    MODE=check ;;
    --rollback) MODE=rollback ;;
    --drop-duckdns-credentials) DROP_CREDS=1 ;;
    *) echo "unknown argument: $a" >&2; exit 2 ;;
  esac
done

say() { printf '%s\n' "$*"; }
ok()  { printf '  [ok]   %s\n' "$*"; }
skip(){ printf '  [skip] %s\n' "$*"; }
bad() { printf '  [FAIL] %s\n' "$*"; }

# ---------------------------------------------------------------- preconditions
# Before removing the tunnel, prove the dpdns name still answers without it.
# This is the whole safety argument: if it does not answer now, removing the
# tunnel would take the site down with no way back except the backup.
check_public() {
  name=${PUBLIC_NAME:-ansible.dpdns.org}
  say "Checking https://$name answers directly (no tunnel) ..."
  # The rest of the ops layer uses wget (busybox), so curl may not be installed.
  # Fall back rather than reporting 000 and refusing for the wrong reason.
  if command -v curl >/dev/null 2>&1; then
    code=$(curl -s -o /dev/null -m 15 -w '%{http_code}' "https://$name/" 2>/dev/null)
    [ -n "$code" ] || code=000
  else
    body=$(wget -qO- --timeout=15 "https://$name/" 2>/dev/null | head -c 2000)
    if [ -n "$body" ]; then code=200; else code=000; fi
  fi
  if [ "$code" = "200" ]; then
    ok "$name -> HTTP 200"
    return 0
  fi
  bad "$name -> HTTP $code"
  return 1
}

# ---------------------------------------------------------------- rollback
do_rollback() {
  # BACKUP is stamped with the current time at startup, so it points at a
  # directory that cannot exist yet on a fresh invocation. Roll back from the
  # most recent real backup instead, or from $1 when one is named explicitly.
  if [ ! -d "$BACKUP" ]; then
    latest=$(ls -1d "$LAB"/.local-backups/tunnel-removal-* 2>/dev/null | sort | tail -1)
    [ -n "$latest" ] && BACKUP=$latest
  fi
  say "Rolling back from $BACKUP"
  [ -d "$BACKUP" ] || { bad "no backup found under $LAB/.local-backups/"; exit 1; }
  [ -f "$BACKUP/lab-tunnel.initd" ] && cp "$BACKUP/lab-tunnel.initd" /etc/init.d/lab-tunnel \
    && chmod +x /etc/init.d/lab-tunnel && ok "restored /etc/init.d/lab-tunnel"
  [ -f "$BACKUP/cloudflared" ] && cp "$BACKUP/cloudflared" /usr/local/bin/cloudflared \
    && chmod +x /usr/local/bin/cloudflared && ok "restored /usr/local/bin/cloudflared"
  [ -f "$BACKUP/duckdns.env" ] && cp "$BACKUP/duckdns.env" "$OPS/duckdns.env" \
    && chmod 600 "$OPS/duckdns.env" && ok "restored ops/duckdns.env"
  [ -f "$OPS/duckdns.env.removed" ] && mv "$OPS/duckdns.env.removed" "$OPS/duckdns.env" \
    && chmod 600 "$OPS/duckdns.env" && ok "un-hid duckdns.env"
  [ -f "$BACKUP/crontab.before" ] && cp "$BACKUP/crontab.before" /etc/crontabs/root \
    && ok "restored /etc/crontabs/root"
  if [ -x /etc/init.d/lab-tunnel ]; then
    rc-update add lab-tunnel default 2>/dev/null && ok "re-enabled lab-tunnel"
    rc-service lab-tunnel start 2>/dev/null && ok "started lab-tunnel"
  fi
  rc-service crond restart >/dev/null 2>&1
  say "Rollback done."
}

# ---------------------------------------------------------------- check
do_check() {
  say "== current state"
  if [ -x /etc/init.d/lab-tunnel ]; then
    say "  lab-tunnel service: present"
    rc-service lab-tunnel status 2>&1 | sed 's/^/    /'
  else
    skip "lab-tunnel service: not installed"
  fi
  if command -v cloudflared >/dev/null 2>&1; then
    say "  cloudflared binary: $(command -v cloudflared)"
  else
    skip "cloudflared binary: not installed"
  fi
  if [ -f "$OPS/duckdns.env" ]; then
    say "  ops/duckdns.env: present"
    # never print the token
    grep -q '^DUCKDNS_TOKEN="\?"\?"\?$' "$OPS/duckdns.env" 2>/dev/null \
      && skip "  token: empty" || say "  token: set (not shown)"
  else
    skip "ops/duckdns.env: absent"
  fi
  say "== certificate (controller, :8443)"
  if [ -f "$LAB/db/certs/fullchain.pem" ]; then
    openssl x509 -noout -subject -enddate -in "$LAB/db/certs/fullchain.pem" 2>/dev/null | sed 's/^/  /'
  else
    skip "no certificate at $LAB/db/certs/fullchain.pem (the :8443 listener is off)"
  fi
  say "== public reachability"
  check_public || bad "the dpdns name does not answer - do NOT remove the tunnel"
  say ""
  say "Removing the tunnel does not affect the public :443 certificate - that"
  say "one is issued and renewed by Pangolin. duckdns.env is kept by default."
  exit 0
}

# ---------------------------------------------------------------- remove
do_remove() {
  if ! check_public; then
    bad "refusing to remove the tunnel: the dpdns name is not answering on its own."
    bad "Removing the tunnel now would take the site down."
    exit 2
  fi

  mkdir -p "$BACKUP" || { bad "cannot create $BACKUP"; exit 1; }
  say "Backup directory: $BACKUP"

  # 1. Cloudflare tunnel service
  if [ -x /etc/init.d/lab-tunnel ]; then
    cp /etc/init.d/lab-tunnel "$BACKUP/lab-tunnel.initd" 2>/dev/null && ok "backed up lab-tunnel initd"
    rc-service lab-tunnel stop 2>/dev/null && ok "stopped lab-tunnel"
    rc-update del lab-tunnel default 2>/dev/null && ok "removed lab-tunnel from default runlevel"
    rm -f /etc/init.d/lab-tunnel && ok "deleted /etc/init.d/lab-tunnel"
  else
    skip "lab-tunnel service not installed"
  fi

  # 2. cloudflared binary. Keep a copy in the backup so rollback does not need
  #    a network download.
  if command -v cloudflared >/dev/null 2>&1; then
    cp "$(command -v cloudflared)" "$BACKUP/cloudflared" 2>/dev/null && ok "backed up cloudflared binary"
    rm -f /usr/local/bin/cloudflared && ok "deleted /usr/local/bin/cloudflared"
  else
    skip "cloudflared not installed"
  fi

  # 3. Cron entries for the Cloudflare tunnel and the DuckDNS A-record updater.
  #
  # The whole ops layer writes to /etc/crontabs/root directly (ops-install.sh,
  # fw-guard.sh, tunnel-install.sh all do). This used to call `crontab -l` and
  # `crontab -`, which reads a *different* store on Alpine/busybox - so the
  # cleanup silently matched nothing and reported success. Read the same file
  # the rest of the layer writes, and back it up before editing.
  #
  # tunnel-status.sh must be matched too: it publishes cloudflared.json every
  # minute, and leaving it behind means the dashboard keeps rewriting "connected"
  # for a tunnel that no longer exists.
  CRONTAB=/etc/crontabs/root
  TUNNEL_RE='tunnel-install\|tunnel-status\|cloudflared'
  if [ -f "$CRONTAB" ] && grep -q "$TUNNEL_RE" "$CRONTAB" 2>/dev/null; then
    cp "$CRONTAB" "$BACKUP/crontab.before" && ok "backed up $CRONTAB"
    grep -v "$TUNNEL_RE" "$CRONTAB" > "$BACKUP/crontab.new" \
      && cat "$BACKUP/crontab.new" > "$CRONTAB" \
      && ok "removed the Cloudflare tunnel cron lines"
    rc-service crond restart >/dev/null 2>&1 && ok "reloaded crond"
  else
    skip "no Cloudflare tunnel cron line"
  fi

  # The DuckDNS A record exists to give the tunnel a stable origin. With the
  # tunnel gone it points at a public IP where nothing listens, so the updater
  # is dead weight. duckdns.env stays - see step 4.
  if [ -f "$CRONTAB" ] && grep -q 'duckdns-update.sh' "$CRONTAB" 2>/dev/null; then
    grep -v 'duckdns-update.sh' "$CRONTAB" > "$BACKUP/crontab.new2" \
      && cat "$BACKUP/crontab.new2" > "$CRONTAB" \
      && ok "removed the DuckDNS A-record cron line"
    rc-service crond restart >/dev/null 2>&1
  else
    skip "no separate duckdns-update cron line"
  fi

  # 3b. The if-up hook was named after DuckDNS when DuckDNS was the only reason
  #     to react to a link change. Rename rather than delete: it still runs
  #     network-sync, which keeps the in-lab DNS override pointed at eth0.
  if [ -f /etc/network/if-up.d/duckdns ]; then
    cat > /etc/network/if-up.d/lab-network <<'HOOK'
#!/bin/sh
[ "$IFACE" = "eth1" ] || exit 0
/root/lab/ops/network-sync.sh >/dev/null 2>&1
HOOK
    chmod +x /etc/network/if-up.d/lab-network
    rm -f /etc/network/if-up.d/duckdns
    ok "renamed if-up.d/duckdns -> if-up.d/lab-network"
  else
    skip "no if-up.d/duckdns hook"
  fi

  # 4. DuckDNS credentials. NOT removed by default - see the header.
  if [ -f "$OPS/duckdns.env" ]; then
    cp "$OPS/duckdns.env" "$BACKUP/duckdns.env" && ok "backed up duckdns.env"
    if [ "$DROP_CREDS" = "1" ]; then
      mv "$OPS/duckdns.env" "$OPS/duckdns.env.removed" \
        && ok "moved duckdns.env aside (.removed)"
      say "  WARNING: this stops renewal of the controller's own :8443"
      say "  certificate (<sub>.duckdns.org, expires 23/02/2027). The public"
      say "  site on :443 is issued and renewed by Pangolin and is unaffected."
      say "  To keep :8443 trusted after that date, point acme-issue.sh at a"
      say "  DNS plugin that can write for the dpdns zone, or drop :8443."
    else
      skip "kept duckdns.env (pass --drop-duckdns-credentials to remove)"
    fi
  else
    skip "no duckdns.env"
  fi

  # 5. Tell the dashboard the truth. Without this it keeps rendering the last
  #    written status, so a decommissioned tunnel shows as a red row forever
  #    and DuckDNS keeps claiming "synced" for a name nothing answers on.
  if [ -f "$OPS/status.py" ]; then
    python3 "$OPS/status.py" cloudflared ok=false state=retired url="" \
      message="Retired - the site is served directly by Pangolin on :443" \
      checked_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >/dev/null 2>&1 \
      && ok "status: cloudflared marked retired"
    python3 "$OPS/status.py" duckdns ok=false state=retired domain="" \
      message="Retired with the tunnel - not used for public access" \
      checked_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >/dev/null 2>&1 \
      && ok "status: duckdns marked retired"
  fi

  # 6. Prove the site is still up after removal
  say "== verifying after removal"
  if check_public; then
    ok "site still reachable - removal was safe"
  else
    bad "site is NOT reachable after removal - run --rollback"
    exit 1
  fi

  say ""
  say "Done. Backup kept at $BACKUP"
  say "Rollback with: $0 --rollback"
}

case "$MODE" in
  check)    do_check ;;
  rollback) do_rollback ;;
  remove)   do_remove ;;
esac
