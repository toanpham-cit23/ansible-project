# Operations layer (SSH-deployed)

Everything here runs on the controller `192.168.23.10`. Nothing is executed on
the workstation; all commands are sent over an SSH session. The dashboard API
has **no shell and no SSH client** — it only reads the JSON status files these
scripts write and leaves request markers in `/data/ops/*.request`.

## Addresses at a glance

The concrete addresses for this install. Anything that needs to be typed into
a browser, a router or an `/etc/hosts` file is here, so it does not have to be
re-derived from the scripts.

| What | Value | Notes |
|---|---|---|
| Lab LAN (controller, `eth0`) | `192.168.23.10` | static; the `:8443` TLS listener lives here |
| Routers | `192.168.23.11` – `.13` | r1, r2, r3 management addresses |
| Public IP | `118.69.10.174` | the NAT gateway; changes with the ISP |
| Public name | `ansible.dpdns.org` → `54.183.47.187` | Pangolin; TLS on `:443`, LE valid to 23/02/2027 |
| In-lab HTTPS origin | `https://192.168.23.10:8443` | the controller's own listener, LE via DuckDNS DNS-01 |
| Retired | `ansiblelabcsn340.duckdns.org`, `*.trycloudflare.com` | removed 27/11/2026 — see "Public access" |

Public access and in-lab access are different paths. `:443` is Pangolin's;
`:8443` is the controller's own listener and is **not** published to the
internet. See "Public access" below.

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
| every 5 min | `network-sync.sh` | detect public/LAN/upstream addresses and refresh the local DNS override (calls `duckdns-update.sh` only while the legacy tunnel exists) |
| every minute | `backup-worker.sh` | consume queued snapshot / retention requests |
| 1 Jan & 1 Jul, 03:00 UTC | `retention.sh --apply` | semi-annual clearance |
| every minute | `fw-guard.sh guard` | enforce the firewall rollback deadline (180 s) |
| 02:17 daily | `acme.sh --cron` | renew the controller's `:8443` certificate; runs the reload command when one is renewed |
| on `eth1` link-up | `/etc/network/if-up.d/lab-network` | sync immediately on a new network instead of waiting for the next tick |

## What happens when the network (and public IP) changes

Three addresses are involved, and only one is public:

| Interface | Address | Behaviour |
|---|---|---|
| `eth0` | `192.168.23.10/24` | **static**, defined in `/etc/network/interfaces`. The lab LAN. Does not change with the upstream network. |
| `eth1` | DHCP (`192.168.220.x` here) | VMware NAT side. Can change when the host/network changes. |
| public | `118.69.10.174` here | the NAT gateway. Changes with the ISP/network. |

**Fully automatic, no action needed:**

- The public name `ansible.dpdns.org` is answered by Pangolin's authoritative
  nameservers, not by anything here — an ISP change does not affect it.
- The certificate is bound to the *name*, not the IP — an IP change never
  invalidates or requires reissuing it. `acme.sh --cron` renews daily when due.
- The lab's own DNS override for the `:8443` name follows `eth0`'s actual
  address, so the in-lab HTTPS entry point keeps working after a move.

**Not automatic — the only manual part:**

- **Getting traffic to Pangolin.** Pangolin forwards to the controller on
  `:8080`; if the upstream address changes, the connector and its route have to
  be checked in the Pangolin dashboard. Nothing on the controller can do that.
  `network-sync.sh` writes the current addresses into `/data/ops/network.json`
  and the *Trust & access* widget shows them.

## Public access — Pangolin on :443 (the tunnel is retired)

The site is reached at **`https://ansible.dpdns.org`**, served by Pangolin:

```
DigitalPlat (ansible.dpdns.org)  NS -> ns1/ns2/ns3.pangolin-ns.net
Pangolin Cloud                   A -> 54.183.47.187
                                 TLS -> LE YR1, CN=*.ansible.dpdns.org (+ apex)
                                 forwards to the controller, HTTP, port 8080
```

Verified 27/11/2026: `https://ansible.dpdns.org/` → HTTP 200 with the dashboard
markup, certificate `CN=*.ansible.dpdns.org`, valid to 23/02/2027. Pangolin
issues and renews that certificate; nothing on this controller is involved.

**The Cloudflare Quick Tunnel and the DuckDNS A record were retired on the same
day** because they had become a second, unused path to the same site. Remove
them with:

```sh
sh /root/lab/ops/remove-legacy-tunnel.sh --check   # report only
sh /root/lab/ops/remove-legacy-tunnel.sh           # remove, with backups
sh /root/lab/ops/remove-legacy-tunnel.sh --rollback
```

The script refuses to remove anything unless the dpdns name answers **without**
the tunnel (exit 2), because that is the only thing standing between "cleanup"
and "site down". It backs everything up to
`/root/lab/.local-backups/tunnel-removal-<UTC>/`, re-checks reachability
afterwards, and marks `cloudflared` and `duckdns` as `retired` in the dashboard
so they stop showing as red failures.

`tunnel-install.sh`, `tunnel-status.sh` and `lab-tunnel.initd` are no longer
deployed by `playbooks/21_ops.yml`. Set `legacy_tunnel: true` to bring them back.

### Two certificates, do not confuse them

| Where | Name | Renewed by | Needs |
|---|---|---|---|
| public `:443` | `*.ansible.dpdns.org` | Pangolin | nothing on the controller |
| controller `:8443` | `<sub>.duckdns.org` | `acme.sh --cron` + `dns_duckdns` | `duckdns.env` |

`duckdns.env` is therefore **kept** by the removal script even though the
DuckDNS *record* is retired: it is the only credential that can renew the
controller's own `:8443` certificate. Deleting it stops that renewal silently —
`--drop-duckdns-credentials` is opt-in and says so. If `:8443` is ever dropped,
the file can go too.

### Why a browser may still say "refused to connect"

`:8443` is not published to the internet; only Pangolin's `:443` is. A TLS
handshake directly to `192.168.23.10:8443` with SNI `<sub>.duckdns.org`
succeeds with TLS 1.3 and a matching CN, so the listener and certificate are
fine — there is simply no inbound path to that port from outside.

For LAN-only use, add one line to the workstation's
`C:\Windows\System32\drivers\etc\hosts` (needs an elevated editor):

```
192.168.23.10 <sub>.duckdns.org
```

Then `https://<sub>.duckdns.org:8443` opens with the trusted certificate and no
warning. `eth0` is static, so this address is stable; `eth1` is DHCP and should
not be used here.

## Trusted HTTPS — one command once the credentials exist

`acme-issue.sh` uses **DNS-01** through DuckDNS, so it needs no inbound port.
Inbound 80/443/8080/8443 on the public IP `118.69.10.174` were all tested and
return no response, so HTTP-01 is impossible here and DNS-01 is the only path.

This is the **controller's own `:8443`** certificate. The public site on `:443`
is a different certificate, issued and renewed by Pangolin — see "Two
certificates" above.

**This lab is already configured.** The steps below are what was run once, and
are kept for a rebuild — on an existing install there is nothing to do, and
`duckdns.env` is already present on the controller.

**You need a free DuckDNS account.** Sign in at <https://www.duckdns.org>,
create a subdomain, and copy the token from the dashboard. Then, over SSH:

```sh
# env vars are preferred - arguments would land in shell history and `ps`
DUCKDNS_DOMAIN="ansiblelabcsn340" \
DUCKDNS_TOKEN="<token-from-duckdns.org>" \
ACME_EMAIL="<your-email>" \
  /root/lab/ops/duckdns-setup.sh
```

The token is not stored in this repo, and neither is the ACME email: both are
credentials, and `files/ops/duckdns.env` ships empty on purpose. The real file
lives on the controller at `/root/lab/ops/duckdns.env` (mode 0600).

The helper:

1. Verifies the token against DuckDNS with a real update — a typo changes nothing
2. Writes `duckdns.env` at mode 0600
3. Syncs the public IP (`duckdns-update.sh`)
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

Inside the lab, `https://<sub>.duckdns.org:8443` works via the local DNS
override. From outside, use `https://ansible.dpdns.org` — the `:8443` listener
is not published.

### Why HSTS on `:443` needed more than `r.TLS != nil`

Pangolin terminates the public TLS connection and forwards **plain HTTP** to
`:8080` over the docker bridge, so `r.TLS` is nil on every public request. The
guard `if r.TLS != nil` therefore never fired on the public site: `:8443` sent
HSTS and `https://ansible.dpdns.org` did not.

The same cause broke two more things, both silent:

| Symptom | Cause | Fix |
|---|---|---|
| no `Strict-Transport-Security` on `:443` | `r.TLS` nil behind the proxy | `requestScheme()` reads `X-Forwarded-Proto` |
| session cookie not `Secure` on the public site | same | `Secure: … \|\| requestScheme(r) == "https"` |
| **every** public session logged as `172.20.0.1` | `handleLogin` keyed the limiter on `r.RemoteAddr`, the connector's bridge address | `clientIP()` reads `X-Forwarded-For` |

That third row was the dangerous one: ten wrong passwords from a single
attacker would have locked out **all** public visitors, because they all shared
one rate-limit key.

The forwarding headers are trusted only when the immediate peer is a proxy we
run — `TRUSTED_PROXIES`, default `127.0.0.0/8,::1/128,172.20.0.0/16`. It is
deliberately **not** all of RFC1918: a lab client on `192.168.23.x` reaches
`:8080` directly, so its `X-Forwarded-For` must be ignored or it could pick its
own IP and walk past the limiter.

Verified against the live connector (the `pangolin up site` client proxies
`127.0.0.1:8080` and adds):

```
Host: ansible.dpdns.org
X-Forwarded-For: 118.69.10.174
X-Forwarded-Host: ansible.dpdns.org
X-Forwarded-Port: 443
X-Forwarded-Proto: https
X-Real-Ip: 118.69.10.174
```

Checks after deploying:

```sh
# public site - header present
curl -sD- -o /dev/null https://ansible.dpdns.org/ | grep -i strict-transport
# plain HTTP and a forged header - header absent
curl -sD- -o /dev/null http://192.168.23.10:8080/ | grep -i strict-transport
curl -sD- -o /dev/null -H 'X-Forwarded-Proto: https' http://192.168.23.10:8080/ \
  | grep -i strict-transport
```

The regression tests live in `docker/api-go/proxy_test.go` (11 cases).


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

**Offline verification (2026-11-19):** `test_fw_guard.py` runs a copy of
`fw-guard.sh` against stubbed `iptables`, `rc-service` and `status.py`, so the
deadline logic can be checked without a controller:

```sh
python3 files/ops/test_fw_guard.py     # 6/6 checks passed
```

It covers: `apply` arms a deadline of ~now+180 s and attaches the chain;
an early `guard` tick does nothing; an expired tick rolls back and clears the
deadline; `confirm` clears it; a confirmed chain is reinstalled without
re-arming the deadline; and manual `rollback` clears both the chain and the
confirmation flag.

**Failure demos (2026-11-19):** `demo_rollback.py` answers a different
question from the test above — not "is the timer arithmetic right" but "when
something breaks, what does the guard actually save me from". It replays the
stub's iptables calls into a rule list and walks a simulated SSH SYN from the
controller through it, so "SSH lost" means the rule order really would deny it.

```sh
python3 files/ops/demo_rollback.py     # 3/3 scenarios behaved as designed
```

| Demo | What breaks | What the guard does |
| --- | --- | --- |
| 1 | Operator inserts a `DROP` above the SSH rule and never confirms | After 180 s the guard detaches the chain; SSH is reachable again with nobody logged in |
| 2 | `iptables` rejects a rule halfway through the set | `add()` rolls the whole chain back immediately, so no half-applied firewall is left |
| 3 | Docker restarts the network and a *confirmed* chain vanishes | The guard reinstalls it silently, without starting a new 180 s countdown |

Demo 1 is the one that matters: the host is genuinely unreachable at the point
the guard fires, which is exactly the case a human cannot fix from outside.

## Compliance demos — what the dashboard says when someone edits a router

`demo_compliance.py` answers the question the module's contract leaves open. The
module only checks that the rules declared in
`compliance/config/desired_state.yml` are present in the running-config; it never
judges configuration outside that list and never changes a router. So when an
operator edits a router by hand, what does the operator see?

The demos run the **real** auditor (`compliance/scripts/run_compliance.py`)
against real FRR-shaped snapshots, write to a real SQLite database, and read the
result back with the same queries the Go endpoint uses. The panel printed is what
the Compliance page would render, not a mock-up of it.

```sh
python3 files/ops/demo_compliance.py     # 3/3 demos behaved as designed
```

| Demo | What happens on the router | What the dashboard shows |
| --- | --- | --- |
| 1 | `no network 192.168.23.0/24 area 0` on r1, outside the pipeline | r1 flips to **NON_COMPLIANT**, that rule is listed with a "changed · was compliant" badge; r2 is untouched and reports no change |
| 2 | A banner, a static route and a stray interface added to r2 | r2 stays **COMPLIANT**, nothing marked changed — no declared rule depends on those lines |
| 3 | SSH to r3 fails; the old snapshot is deleted | **ERROR**, not a compliance verdict; the previous known result stands and no rule is marked changed |

Demo 2 is the scope boundary made visible. The banner, the static route and the
interface are real changes to the router that this module will never report,
because the audit answers "are the required rules present", not "is this
configuration good". They need review by other means.

Demo 3 is why `ERROR` is a separate state: a network outage must not look like
drift, and a stale snapshot must not produce a confident verdict about a router
nobody could read.

One note on what the page lists. The audit stores the rules that **failed**, plus
a single `All required rules` row for a device that passed — it is not a per-rule
checklist of every router. That is why demo 2's panel shows one summary row
rather than the five declared rules.

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

## Regression tests

Four suites, each written after a real bug. Run from the repository root:

```sh
python3 files/ops/test_fw_guard.py             # 6/6 firewall deadline checks
python3 files/ops/test_audit_css2.py           # 4/4 layout guards
python3 files/ops/test_poller_unknown_rule.py  # deletion rule respects ICMP
node    files/ops/test_dashboard_filter.js     # 22 dashboard filter checks
```

`test_dashboard_filter.js` lifts `isInfra()` and `hasHealth()` out of the
shipped `app.js` rather than copying them, so it fails when the real logic
changes. It also asserts the render loop still calls `all.filter(isInfra)` —
without that, the helpers could be correct and ignored while 16 cards came
back. Verified by mutation: removing the filter fails 1 check, making
`hasHealth` always return true fails 6.

## Where it shows up

- **Status bar**, above every view: public access, DuckDNS (or "retired"),
  HTTPS, firewall, tunnel (hidden once retired), SQLite size, backups,
  next clearance
- **Dashboard → Operations**: trust/access, backup & restore, maintenance &
  retention, node telemetry, alerts & audit feed
- **Node cards**: live SSH/22 indicator and a Terminal console button
- **Gap analysis** (separate nav section): the structural audit and open items

## Device credential reveal — two steps, on purpose

`GET /api/devices/{id}/password` used to return a decrypted password on demand,
which meant one line in DevTools could sweep every device:

```js
for (let i = 1; i <= 16; i++) fetch('/api/devices/' + i + '/password')
```

It now takes two calls, in `reveal.go`:

| Call | Purpose |
|---|---|
| `POST /api/devices/{id}/password/confirm` | mints a token bound to `(user, device)` |
| `GET /api/devices/{id}/password?token=…` | returns the password once, then burns the token |

- **Single use.** The token is consumed before the match is checked, so a
  replayed request fails even with the right value.
- **60-second TTL** (`revealTokenTTL`), swept on each issue.
- **Per-session budget** of 12 reveals (`passwordRevealMax`), reset on login.
  A 16-device sweep stops at 12 with `429`.
- **Bound to the pair**, so a token for `device 3` is useless for `device 4`,
  and one minted for your session is useless in someone else's.

Tests: `go test -run Reveal ./...` — 9 cases covering each of the above, plus a
source-level CSP assertion that rejects `'unsafe-inline'`/`'unsafe-eval'` in
`script-src`.

The UI does this transparently: the reveal button makes both calls, so nothing
in the normal flow changes. This raises the cost of a scripted sweep; it does
not make a stolen session harmless — see the security headers below.

## Security headers

Set in `main.go` for every response:

| Header | Value |
|---|---|
| `Content-Security-Policy` | `default-src 'self'` … `script-src 'self'`, `frame-ancestors 'none'`, `object-src 'none'` |
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `DENY` |
| `Referrer-Policy` | `no-referrer` |
| `Permissions-Policy` | `geolocation=(), microphone=(), camera=()` |
| `Strict-Transport-Security` | `max-age=31536000; includeSubDomains` — sent whenever the request reached us over TLS, including TLS terminated by Pangolin in front of `:8080` (see above) |

`script-src 'self'` means **no inline scripts**. That is why the phone/desktop
classification lives in `/static/js/boot.js` rather than a `<script>` block: one
inline script would have forced `'unsafe-inline'` and made the whole directive
decorative. `style-src` keeps `'unsafe-inline'` because views set `style="…"`
on chart bars — a styling risk, not a script-execution one.

Blocking DevTools is not attempted. The browser belongs to the visitor, so a
key-block is theatre: it breaks screen readers and your own debugging, and
`curl` ignores it. The only boundary that means anything is the server's.
