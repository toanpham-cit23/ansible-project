# Operations layer (SSH-deployed)

Everything here runs on the controller `192.168.23.10`. Nothing is executed on
the workstation; all commands are sent over an SSH session. The dashboard API
has **no shell and no SSH client** — it only reads the JSON status files these
scripts write and leaves request markers in `/data/ops/*.request`.

## Install

```sh
# from the workstation, over SSH
doas sh /root/ansible-project/files/ops/ops-install.sh
```

This sets permissions, adds the cron lines, seeds every status file and takes an
initial snapshot. Re-running is safe.

## Cron schedule

| When | Script | What |
|---|---|---|
| every 5 min | `network-sync.sh` | detect public/LAN/upstream addresses, refresh the A record (calls `duckdns-update.sh`) and the local DNS override |
| every minute | `backup-worker.sh` | consume queued snapshot / retention requests |
| 1 Jan & 1 Jul, 03:00 UTC | `retention.sh --apply` | semi-annual clearance |
| every minute | `fw-guard.sh guard` | enforce the firewall rollback deadline |
| 02:17 daily | `acme.sh --cron` | renew certificates; runs the reload command when one is renewed |
| on `eth1` link-up | `/etc/network/if-up.d/duckdns` | sync immediately on a new network instead of waiting for the next tick |

## What happens when the network (and public IP) changes

Three addresses are involved, and only one is public:

| Interface | Address | Behaviour |
|---|---|---|
| `eth0` | `192.168.23.10/24` | **static**, defined in `/etc/network/interfaces`. The lab LAN. Does not change with the upstream network. |
| `eth1` | DHCP (`192.168.220.x` here) | VMware NAT side. Can change when the host/network changes. |
| public | `<public-ip>` here | the NAT gateway. Changes with the ISP/network. |

**Fully automatic, no action needed:**

- The DuckDNS A record follows the public IP (5-minute cron + link-up hook).
- The certificate is bound to the *name*, not the IP — an IP change never
  invalidates or requires reissuing it. `acme.sh --cron` renews daily when due.
- The lab's own DNS override for the DuckDNS name follows `eth0`'s actual
  address, so `https://<domain>:8443` keeps working inside the lab.

**Not automatic — the only manual part:**

- **Port forwarding.** Inbound 80/443/8080/8443 on the public IP currently
  return no response. External access needs `public:8443 → eth1:8443` on the
  VMware host, plus `host:8443 → public` on your router. Moving to a different
  network means recreating those two rules on the new network's equipment;
  that cannot be done from inside the VM over SSH. `network-sync.sh` writes the
  current target addresses into `/data/ops/network.json` and the dashboard's
  *Trust & access* widget shows them.
- If the ISP puts you behind **CGNAT** (`100.64.0.0/10`), inbound forwarding is
  impossible. Use a tunnel instead — Cloudflare Tunnel, Tailscale Funnel or
  ngrok — which initiates outbound and needs no port forward at all.

## Cloudflare Quick Tunnel — external access with no port forward

A DuckDNS name alone does not make the site reachable: it resolves to the
public IP, where inbound 8443 is not forwarded. `lab-tunnel` solves that by
dialing **outbound** to Cloudflare, so NAT and CGNAT are irrelevant.

```sh
/root/lab/ops/tunnel-install.sh      # install/start (idempotent)
rc-service lab-tunnel status
tail -20 /var/log/lab-tunnel.log
cat /root/lab/db/ops/cloudflared.json   # current URL + health
```

- Installs the static `cloudflared` build (Alpine has no package for it).
- Runs an OpenRC service, enabled at boot; `tunnel-status.sh` publishes the
  current URL to `/data/ops/cloudflared.json` every minute and the dashboard
  shows it in the status bar and the *Trust & access* widget.
- The origin is `https://<duckdns-name>:8443`, with that name pinned to
  `127.0.0.1` in the controller's `/etc/hosts`. cloudflared therefore verifies
  the real Let's Encrypt certificate, and the API still sees a TLS request —
  so `Secure` cookies and HSTS survive the tunnel instead of being downgraded
  to plain HTTP.
- This is a **Quick Tunnel**: the `*.trycloudflare.com` URL is random and
  changes whenever the service restarts. That is why the current URL is
  surfaced in the dashboard rather than hardcoded anywhere.

For a **stable hostname** on your own domain, replace it with a named tunnel:
`cloudflared tunnel login` (browser OAuth), then `cloudflared tunnel create`,
a `config.yml` with ingress rules, and `TUNNEL_TOKEN` / the origin certificate.
That needs a Cloudflare account and a zone you control — a DuckDNS subdomain
cannot be used because its nameservers cannot be delegated to Cloudflare.

Verified end to end from the workstation: `/healthz` → HTTP 200 `{"ok":true}`,
`/app` → HTTP 200 with dashboard markup, `.NET` validated the Cloudflare
certificate chain, and `POST /api/login` returned
`HttpOnly; Secure; SameSite=Strict` plus HSTS.

### Why the browser says "refused to connect" from the workstation

The DuckDNS name resolves to the **public** IP `<public-ip>`. There is no
port forward there, so TCP :8443 is refused — `ERR_CONNECTION_REFUSED`. The
certificate and the :8443 listener are both fine; a TLS handshake directly to
`192.168.23.10:8443` with SNI `<subdomain>.duckdns.org` succeeds with
TLS 1.3 and a matching CN.

The local DNS override lives in the **lab's own DNS responder** (UDP 53 on the
controller). It helps lab devices that use that resolver. A normal Windows
workstation uses its usual DNS and resolves the name to the public IP, so the
override does not apply to the browser.

For LAN-only use, add one line to the workstation's
`C:\Windows\System32\drivers\etc\hosts` (needs an elevated editor):

```
192.168.23.10 <subdomain>.duckdns.org
```

Then `https://<subdomain>.duckdns.org:8443` opens with the trusted
certificate and no warning. `eth0` is static, so this address is stable; `eth1`
is DHCP and should not be used here. Remove the line once real port forwarding
is in place, otherwise that PC will always resolve the name to the private
address even from another network.

## Trusted HTTPS — one command once the credentials exist

`acme-issue.sh` uses **DNS-01** through DuckDNS, so it needs no inbound port.
Inbound 80/443/8080/8443 on the public IP `<public-ip>` were all tested and
return no response, so HTTP-01 is impossible here and DNS-01 is the only path.

**You need a free DuckDNS account.** Sign in at <https://www.duckdns.org>,
create a subdomain, and copy the token from the dashboard. Then, over SSH:

```sh
# env vars are preferred - arguments would land in shell history and `ps`
DUCKDNS_DOMAIN="your-subdomain" \
DUCKDNS_TOKEN="your-token" \
ACME_EMAIL="you@example.com" \
  /root/lab/ops/duckdns-setup.sh
```

The helper:

1. Verifies the token against DuckDNS with a real update — a typo changes nothing
2. Writes `duckdns.env` at mode 0600
3. Syncs the public IP (`duckdns-update.sh`, also on a 5-minute cron)
4. Issues a Let's Encrypt certificate with `acme.sh --dns dns_duckdns`
   (the plugin's variable is `DuckDNS_Token`, confirmed against the installed
   `/usr/share/acme.sh/dnsapi/dns_duckdns.sh`)
5. Installs it into `/root/lab/db/certs/` → `/data/certs` in the container,
   owned by uid 10001
6. Publishes the same name in the lab's own DNS responder pointing at
   `192.168.23.10`, so clients inside the lab also get a matching trusted name
7. Restarts the API; the Go server starts its `:8443` TLS listener only when
   both certificate files exist. `:8080` stays up as a fallback. HSTS is sent on
   TLS requests only, and session cookies become `Secure` on HTTPS logins.

`acme.sh` installs its own renewal cron. Renewal re-runs the reload command,
which restarts the API so the new certificate is picked up.

**Still required outside SSH:** forwarding public 443 to the controller for
external clients. That is a hypervisor/router change, not something this repo
can do. Inside the lab, `https://<domain>.duckdns.org:8443` works via the local
DNS override.

## Firewall

```sh
/root/lab/ops/fw-guard.sh apply      # rules + 180 s auto-rollback deadline
/root/lab/ops/fw-guard.sh confirm    # keep them (run while SSH still works)
/root/lab/ops/fw-guard.sh rollback   # undo immediately
```

The guard is a cron hook, not a background process, so it survives the SSH
session dying. Rules go in a dedicated `LABFW` chain jumped from `INPUT`;
rollback just removes that jump, so Docker's own chains are never flushed.
Loopback, `docker0` and established connections are always allowed. Each rule
is checked as it is appended — one rejection rolls everything back rather than
leaving a DROP with SSH still un-allowed.

A container or host network restart can drop custom INPUT chains. Once rules
have been `confirm`ed, the guard detects the missing chain and reinstalls them
without re-arming the 180 s deadline. A *pending*, unconfirmed firewall still
rolls back on time.

**Credential safety:** `playbooks/21_ops.yml` copies the scripts individually
and seeds `duckdns.env` with `force: false`. A recursive copy would silently
replace a live DuckDNS token with the empty template on every redeploy — that
happened once and is why the env file is handled separately.

Scope: `INPUT` protects host services. Docker-published ports (8080/8443) are
DNATed through `FORWARD`/`DOCKER` and are deliberately not restricted here —
blocking them would also block the public HTTPS endpoint.

## Retention

`retention.py` prunes time-series tables older than 180 days, expired sessions,
old `.eml` files and backups beyond the last 12. Column names are discovered
from the schema. These tables are **never** touched:

```
sites, devices, vlans, subnets, interfaces, links, users, settings,
ip_leases, dns_records, host_design, log_patterns, alert_rules,
shift_admins, sessions (except expired rows)
```

That keeps the node inventory (controller, r1, r2, r3) intact. Default is a
dry run; the dashboard has a **Dry run** button and a **Run clearance** button
that requires the literal `APPLY` in the request body.

## Backups

`snapshot.sh` uses a SQLite *online* backup through the poller container —
copying `net.db` directly would capture an inconsistent WAL — then tars the
snapshot with the ops scripts, `.env` and compose file. Restore remains a
separate, manually confirmed playbook (`playbooks/14_restore.yml`); the widget
reports its status but does not trigger it.

## Where it shows up

- **Status bar**, above every view: DuckDNS, HTTPS, firewall, SQLite size,
  backups, next clearance
- **Dashboard → Operations**: trust/access, backup & restore, maintenance &
  retention, node telemetry, alerts & audit feed
- **Node cards**: live SSH/22 indicator and a Terminal console button
- **Gap analysis** (separate nav section): the structural audit and open items
