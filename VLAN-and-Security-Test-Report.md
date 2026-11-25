# VLAN Implementation and Security Upgrade — Test Report

**Environment:** 3 × Alpine Linux 3.24.2 / FRR 10.6.1 routers (`192.168.23.11–13`) plus a
controller (`192.168.23.10`) running the Ansible project.
**Method:** everything below was executed against the live lab. Every claim is followed by the
command output or log line that supports it. Where something did not work, it is marked **FAILED**
and the reason is given rather than worked around.
**Date:** 2026-11-16

---

## 1. Summary

| Area | Verdict |
| --- | --- |
| Virtual switch + VLAN data plane (10/20/40/70/90/99) | **Works** — 9/9 routing checks |
| Access ports / trunk ports | **Works** — pvid and tag membership verified |
| Inter-VLAN routing | **Works** — 5 SVIs, static + OSPF |
| ACLs (inter-VLAN, service-specific) | **Works** — 7/7 policy checks |
| Monitoring metrics (CPU/RAM/bw/latency/OSPF/restarts/incidents) | **Collected, with one caveat** |
| SNMPv2c → SNMPv3 | **Works** — 8/8 assertions |
| SSH password → key auth | **Works** — password refused on all 3 |
| Passwords → Ansible Vault | **Works** — round-trip verified |
| Basic ACL → service-specific ACL | **Works** in nftables; **the repo's playbook is broken** |
| Single admin → RBAC | **Works** by a different mechanism than requested |
| No logging → audit logs | **Partial** — `auditd` does not exist here |
| Four-way network segmentation | **Works** — 10/10 |
| **Defect: OSPF ran over the management LAN** | **Found and fixed** |
| **Defect: `05_acl.yml` can never report success** | **Found, not yet fixed in repo** |
| **Defect: `04_snmp.yml` still deploys SNMPv2c** | **Found, not yet fixed in repo** |

---

## 2. Network Infrastructure Upgrade (VLANs)

### 2.1 Constraint found before any of this could be built

The first thing I tested was whether VLAN tags can leave the VM at all, because the design file
says they cannot. They cannot — and I have the packets to prove it rather than taking the file's
word for it.

`tcpdump` on the controller's uplink shows tagged frames being transmitted:

```
ethertype 802.1Q (0x8100), length 102: vlan 999, p 0, ethertype IPv4 ...
```

...while the far side's sub-interface counter stays at exactly zero across ten tagged pings:

```
r1: /sys/class/net/eth0.999/statistics/rx_packets = 0
```

**Root cause:** the VMware port group drops 802.1Q tags. Fixing it requires setting the port group's
VLAN ID to 4095, which is a hypervisor-side change that cannot be made from inside the guest.

**Consequence:** I did not pretend cross-VM trunks work. The fabric is built with the same kernel
primitives a physical switch uses — a Linux bridge with `vlan_filtering 1`, plus network namespaces
standing in for the un-cabled switches — entirely inside the controller VM. This is a real 802.1Q
data plane, not a simulation: frames are actually tagged and filtered by the kernel.

### 2.2 What was built

`/root/vlanlab/build.sh` creates:

- `br-lab` with `vlan_filtering 1`
- 9 namespaces: `ns-acct ns-hr ns-sales ns-srv ns-cctv ns-mgmt ns-r1 ns-r2 ns-r3`
- 6 access ports, each `pvid untagged` on its own VLAN
- 3 trunk ports, carrying tagged VLANs only
- 5 SVIs acting as the VLAN gateways (`.254` in each subnet)
- a router triangle mirroring the real transit links, with static routes to foreign VLANs

Access port (untagged): `bridge vlan add dev p-acct vid 10 pvid untagged`
Trunk member (tagged): `bridge vlan add dev p-r1 vid 10`

### 2.3 Test results

**Routing — `verify.sh`:**

```
PASS  acct -> its gateway 10.10.10.254 (VLAN 10 SVI)
PASS  acct never learns HR's MAC (L2 separated)
PASS  sales(40) -> acct(10) cross-router r2->r1
PASS  p-acct is pvid 10 untagged
PASS  p-r1 trunk carries tagged 10
...
```

Before ACLs were applied this was **9/9**. The fabric routes every VLAN to every VLAN, which is
what proves the routing is correct before policy is layered on top.

**L2 separation — deliberately tested as a MAC-learning property, not a ping.**

My first version of this test asserted "Accounting cannot ping HR" and it failed, because with
inter-VLAN routing enabled Accounting *should* be able to reach HR — that is a Layer 3 property, not
Layer 2. The correct L2 assertion is that Accounting never learns HR's MAC address:

```
HRMAC=1e:80:ec:10:08:e2
PASS  acct never learns HR's MAC (acct's neighbour table contains only r1's trunk.10 MAC)
```

The bridge FDB confirms HR's MAC is learned in `vlan 20` only.

**Policy — `policy.sh`, after `acl.sh`:**

```
PASS  acct cannot ping HR host 10.10.20.10
PASS  control: sales(40) CAN ping HR, so the acct block is a policy not a broken route
PASS  acct -> srv:80  ALLOWED (published service)
PASS  acct -> srv:443 ALLOWED (published service)
PASS  acct -> srv:22  DROPPED (unpublished service, service-specific ACL)
PASS  acct -> srv:3306 DROPPED (unpublished service, service-specific ACL)
PASS  cctv cannot reach acct (VLAN 90 isolated)
---- policy result: 7 passed, 0 failed ----
```

**This is the requested acceptance criterion, met:** a host in Accounting cannot freely reach HR,
while still reaching Servers on 80/443 — and the negative control (Sales *can* reach HR) proves the
block is policy, not a broken route.

### 2.4 A test-harness bug that mattered

Re-running `verify.sh` after ACLs were applied produced 4 FAILs. Those failures are **correct
behaviour** — ICMP is now blocked between those VLANs. It is a harness ordering problem, not a
fabric problem, and `run.sh` now enforces `build → verify → acl → policy` and documents why.

Worth stating plainly: had I not had the negative control, I could easily have "fixed" a working
fabric to make a broken test pass.

### 2.5 Probe semantics (a trap for anyone re-running this)

`CONNECTED` / `ConnectionRefused` = packet forwarded, ACL permitted.
`TimeoutError` = dropped, ACL denied.

A blocked port therefore looks like a **timeout**, not a refusal. My first `tcp_ok` helper used
`exec 3<>/dev/tcp/...`, which busybox `ash` does not support — it failed on every call, making every
blocked case look like a pass. The tests now use `python3 socket`.

---

## 3. Metrics to Monitor

All figures read from the live routers.

### 3.1 CPU and RAM

Sampled over a 10-second window from `/proc/stat` and `/proc/meminfo`:

| Router | CPU % | RAM used % | RAM available | Processes | Uptime |
| --- | --- | --- | --- | --- | --- |
| r1 | 2.4 % | 78.5 % | 22 MB | 186 | 8 d 13 h |
| r2 | 0.6 % | 71.1 % | 30 MB | 187 | 8 d 13 h |
| r3 | 0.6 % | 74.9 % | 26 MB | 185 | 8 d 13 h |

Each router has 104 MB RAM total. **78 % "used" with 22 MB available is not a problem** — most of
that is page cache and reclaimable. The number to alarm on is `MemAvailable`, not `MemUsed`.

### 3.2 Per-interface bandwidth

Sampled twice, 5 seconds apart, computing a delta:

| Router | Interface | Role | rx bytes | tx bytes | Rate |
| --- | --- | --- | --- | --- | --- |
| r1 | eth0 | mgmt | 1,361,069,611 | 1,356,073,603 | ~10.2 / 9.4 kbit/s |
| r2 | eth0 | mgmt | 858,911,251 | 931,940,764 | ~4.2 / 4.4 kbit/s |
| r3 | eth0 | mgmt | 858,896,990 | 931,973,226 | ~4.3 / 4.5 kbit/s |
| all | eth1, eth2 | transit | ~80,868 | ~14,900 | OSPF hellos only |

The transit links carried ~80 KB in 8.5 days: that is hello traffic and nothing else. Zero errors,
zero drops on every transit interface. The `eth0` RX drops of 16–20 are cumulative over 8 days.

### 3.3 Latency between routers

200 pings per pair, 0 % loss in every case.

| Pair | Before (mgmt-routed) avg | After (transit-routed) avg |
| --- | --- | --- |
| r1 → r2 | 0.361 ms | 0.432 ms |
| r1 → r3 | 0.372 ms | 0.548 ms |
| r2 → r3 | 0.349 ms | 0.521 ms |
| r2 → r1 | 0.364 ms | 0.393 ms |
| r3 → r1 | 0.366 ms | 0.399 ms |

The increase is real and expected: traffic now takes a routed hop instead of crossing a shared
broadcast domain. Sub-millisecond either way. Reporting the "after" number as a regression would be
wrong — it is the cost of doing it correctly.

### 3.4 OSPF neighbour status

```
r1   2.2.2.2  Full/DR      10.0.1.2  eth1
r1   3.3.3.3  Full/DR      10.0.3.2  eth2
r2   1.1.1.1  Full/Backup  10.0.1.1  eth1
r2   3.3.3.3  Full/Backup  10.0.2.2  eth2
r3   1.1.1.1  Full/Backup  10.0.3.1  eth1
r3   2.2.2.2  Full/DR      10.0.2.1  eth2
```

6/6 adjacencies Full. See §3.7 — they were **not** Full over the transit links until I fixed the
config.

### 3.5 Router restarts

| Router | Uptime | Boot time |
| --- | --- | --- |
| r1 | 735,639 s | 2026-11-08 00:47:36 |
| r2 | 735,887 s | 2026-11-08 00:47:34 |
| r3 | 735,888 s | 2026-11-08 00:47:33 |

All three booted within **3 seconds of each other**, 8.5 days ago. That is a simultaneous VM start,
not three independent crashes. No restarts since.

Caveat on the metric itself: FRR was restarted several times during this work (by me, and by
`04_snmp.yml`, which restarts FRR twice per run). **A "router restart" counter that counts FRR
restarts will be noisy and misleading** — it should count host boots (`uptime -s` changes) unless
the intent is specifically to track routing-process flaps.

### 3.6 Historical incident record

There is a real one. `r1`'s sshd log shows **two coordinated sweeps** from eight hosts
(`192.168.23.150–157`):

| Window | Sources | Events per source | Duration | Successful logins |
| --- | --- | --- | --- | --- |
| 12:50:29 – 13:09:39 | 8 | ~371 | ~19 min | **0** |
| 14:06:55 – 14:19:09 | 8 | ~243 | ~12 min | **0** |

All eight began within 3 seconds of each other and stopped within 3 seconds of each other — a
scripted, coordinated sweep, not eight independent attackers. 2,078 sshd events total. **Zero
successful authentications** on all three routers. All eight source addresses are now unreachable.

This is worth flagging operationally: it is exactly the pattern that a brute-force attempt against
`root` with `PasswordAuthentication yes` produces, and it is the strongest argument in this report
for the SSH change in §4.2.

**Logging caveat:** `syslogd` rotates by size with no explicit limit, and the first sweep's detail
was compacted out of the active file during the session. If incident history is a real requirement,
logs must be shipped off-box — see §4.7.

### 3.7 DEFECT FOUND: OSPF was running over the management LAN

This is the most important finding in the networking half.

`show ip ospf neighbor` reported every adjacency on **`eth0`** — the management network — not on the
transit links. The routing table confirmed it:

```
2.2.2.2 nhid 20 via 192.168.23.12 dev eth0 proto ospf metric 20
3.3.3.3 nhid 21 via 192.168.23.13 dev eth0 proto ospf metric 20
```

**Cause:** `playbooks/02_ospf.yml` line 8 hardcodes the management subnet as the OSPF segment:

```yaml
lan_subnet: 192.168.23.0/24
...
- c 'network {{ lan_subnet }} area {{ ospf_area }}'
```

The transit networks (`10.0.1.0/29`, `10.0.2.0/29`, `10.0.3.0/29`) were configured on the
interfaces but **never advertised**, so they were dead links. Worse, `r1` had no route to
`10.0.2.0/29` at all — `ping 10.0.2.2` from r1 returned *unreachable*.

**This is precisely the anti-pattern the requirements warn about:** "avoid consolidating all types
of traffic onto a single management network." Routing protocol adjacencies, user traffic and admin
access were all sharing one segment.

**Fix applied** (all three routers, `write memory`):

```
router ospf
 no network 192.168.23.0/24 area 0
 network 10.0.1.0/29 area 0
 network 10.0.3.0/29 area 0
```

After the change, all six adjacencies formed over `eth1`/`eth2` and OSPF installed ECMP routes —
r1 to `10.0.2.0/29` now has two equal-cost nexthops, which is exactly what a triangle should give:

```
10.0.2.0/29  metric 20
    nexthop via 10.0.1.2 dev eth1 weight 1
    nexthop via 10.0.3.2 dev eth2 weight 1
```

**Bonus:** the management subnet is no longer advertised, so a compromise in a user VLAN cannot
learn a route to the management plane.

**One correction to my own reasoning:** immediately after the change the neighbours sat at
`2-Way/DROther` with no routes, and I briefly concluded the three transit segments must share one
broadcast domain. That was wrong. It was DR election still in the Wait timer; it settled to Full on
its own. I mention it because the "obvious" explanation was the wrong one.

---

## 4. Security Upgrades

### 4.1 SNMPv2c → SNMPv3 with auth + encryption

**Works — 8/8.** All three routers serve v3 `authPriv` (SHA + AES) and refuse everything else:

| # | Test | Result |
| --- | --- | --- |
| 1 | v3 authPriv, correct creds | `sysName.0 = STRING: r1` ✅ |
| 2 | v3, wrong auth password | `Authentication failure` ✅ |
| 3 | v3, wrong priv password | `Timeout: No Response` ✅ |
| 4 | v2c community `public` | `Timeout` ✅ |
| 5 | v2c community `private` | `Timeout` ✅ |
| 6 | v3 authNoPriv (no encryption) | `authorizationError` ✅ |
| 7 | v3 unknown user | `Unknown user name` ✅ |
| 8 | v3 noAuthNoPriv | `authorizationError` ✅ |

The community string is gone entirely — both `public` and `private` time out.

**Two traps worth recording:**

1. `createUser` in `snmpd.conf` is honoured **only if the user does not already exist**. A stale
   `/var/lib/net-snmp/snmpd.conf` keeps old localized keys and makes every correct password fail
   auth silently. `rm -f /var/lib/net-snmp/snmpd.conf` before restart is mandatory.
2. The client tools (`snmpget`, `net-snmp-create-v3-user`) are in `net-snmp-tools` — **not**
   `net-snmp-utils`, and they belong on the **manager**, not the agent. I wasted time trying to
   install them on the routers, which cannot `apk add` at all (their repos point at a dead mirror
   `foobar.turbo.net.id` and they have no default route).

### 4.2 SSH passwords → SSH key authentication

**Works.** `PasswordAuthentication no` on all three, verified both directions:

```
r1  key-login-ok  PasswordAuthentication no   password refused (BadAuthenticationType)
r2  key-login-ok  PasswordAuthentication no   password refused (BadAuthenticationType)
r3  key-login-ok  PasswordAuthentication no   password refused (BadAuthenticationType)
```

Key login is confirmed live in r1's log, with the fingerprint matching the ops key exactly
(`SHA256:URTB4WqF0F69aJ/mNfftlaobkeZDKEhz5WUvdhAbMhw`):

```
Accepted publickey for root from 192.168.23.1 ... ED25519 SHA256:URTB4WqF0F69aJ/mNfftlaobkeZDKEhz5WUvdhAbMhw
```

**Honest note on process:** I initially applied this behind a 240-second auto-revert timer as a
lockout safety net, then failed to kill the timer, so it silently re-enabled password auth behind
me. I caught it by re-checking, and re-applied the change without a timer, using `sshd -t` to
validate the config before restarting instead. The version now in place is the validated one. Had I
not re-verified, this report would have claimed a hardening that was not actually there.

**Root login is still `PermitRootLogin yes`.** Key-only root is a real improvement but not the end
state — see §4.5.

### 4.3 Passwords in inventory → Ansible Vault

**Works.** Encrypt → decrypt round-trip on the real project:

```
--- encrypt ---
--- encrypted header ---
$ANSIBLE_VAULT;1.1;AES256
--- plaintext present in ciphertext? ---
0 occurrences (encrypted)
--- decrypt round-trip ---
ROUND-TRIP IDENTICAL
```

Wrong password is correctly rejected:

```
[ERROR]: Failed to view '/tmp/vars-vault.yml': Decryption failed
         (no vault secrets were found that could decrypt).
```

**Gotcha that could hide a real failure:** `ansible.cfg` sets `vault_password_file`, and in that
configuration **`--vault-id` is silently ignored** — a "wrong password" test can appear to succeed.
My first wrong-password test printed the plaintext for exactly this reason. To test the password
itself you must bypass the config (`ANSIBLE_CONFIG=/tmp/empty-ansible.cfg`). Separately,
`ansible-vault encrypt` requires `--encrypt-vault-id` when `vault_password_file` is set, or it fails
with *"The vault-ids default,default are available to encrypt"*.

### 4.4 Basic ACLs → service-specific and VLAN-specific ACLs

**Works, but not via the mechanism the repo uses.** Two distinct layers:

1. **Inter-VLAN, service-specific** — implemented in nftables inside the fabric, proven by
   `policy.sh`: HTTP/HTTPS to Servers allowed, SSH/MySQL to Servers dropped, Accounting↔HR blocked,
   CCTV isolated. A per-VLAN catch-all `drop` on `10.10.70.0/24` means an unpublished service is
   denied by default rather than allowed by omission.

2. **Router VTY access** — FRR standard ACL 10 (`permit 192.168.23.0/24`, `deny any`) bound to
   `line vty`. Proven by A/B test on the vtysh socket:

   | Config | Response to connect from `127.0.0.1` (not in ACL 10) |
   | --- | --- |
   | `access-class 10` applied | immediate empty response — **DENIED** |
   | ACL temporarily removed | `Hello, this is FRRouting (version 10.6.1)...` — **banner served** |

   Restored afterwards. All three routers now report `DENIED`.

#### DEFECT FOUND: `playbooks/05_acl.yml` can never report success

Three separate bugs, the last of which is the dangerous one:

1. **Wrong syntax.** The task uses `access-class 10 in`, which FRR 10.6 rejects:
   `% Unknown command: access-class 10 in`. FRR's grammar is `access-class WORD` — there is no
   `in`/`out` qualifier. The playbook therefore **never actually applied the ACL**.
2. **The verification can never pass.** It greps for the literal string `access-class 10 in`, which
   can never appear in the config because that is not valid syntax. Even with the ACL correctly
   applied, it reports `VTY_BIND_MISSING`.
3. **The verification also reports `ACL_NOT_FOUND` for an ACL that exists.** `show run` renders the
   entry as `access-list 10 seq 5 permit ...`, but the grep looks for
   `access-list 10 permit 192.168.23.0` — the `seq 5` breaks the match.

All three were confirmed by replaying the playbook's own check against a config where I had applied
the ACL correctly by hand: `ACL_NOT_FOUND` / `VTY_BIND_MISSING`, while the ACL was in fact present.

The dangerous part is that the apply task carries `failed_when: false`, so the playbook exits **green
with the ACL unapplied and unenforced**. A security control that reports success while doing nothing
is worse than no control, because it stops anyone looking.

**Not fixed in the repo** — the fix is trivial (`access-class 10`, and grep for `access-class 10` /
`access-list 10 seq`) but changing a playbook that is part of the deploy pipeline is a decision for
the owner, so I am reporting it rather than shipping it.

### 4.5 Single admin account → RBAC

**Partially works — implemented by a different mechanism than requested, because the requested one
is not available in this environment.**

What is absent, verified on the routers:

```
doas       MISSING
sudo       MISSING
runuser    MISSING
su         /bin/su
setpriv    /bin/setpriv
```

FRR has **no per-user privilege model** (`username` is not a valid vtysh command in 10.6; there were
0 `username` entries configured). So classic "create admin and operator accounts with different
privilege levels" is not achievable here. A single `root` remains the only login.

What I built instead — role-separated SSH keys enforced by `authorized_keys`, which is a real
control even if it is coarser than `sudo`:

| Test | Read-only role | Result |
| --- | --- | --- |
| permitted `show ip ospf neighbor` | ✅ works | `Neighbor ID  Pri State ...` |
| `id` | ❌ `rbac: command not permitted for this role: id` | blocked |
| `cat /etc/shadow` | ❌ `rbac: command not permitted ...` | blocked |
| `configure terminal` | ❌ `rbac: command not permitted ...` | blocked |
| interactive shell (`-tt`) | ❌ `PTY allocation request failed` | blocked |
| admin role: `id` | ✅ `uid=0(root)` | works |
| admin role: `vtysh` | ✅ `FRRouting 10.6.1 (r1)` | works |

7/7 as intended. All RBAC keys also carry `from="192.168.23.10"`, so they only work from the
controller.

**Honest limitation:** this is command-level role separation, not OS-level privilege separation. A
read-only operator cannot run anything except the three whitelisted `show` commands, but the account
behind it is still `root`. Real RBAC needs a dedicated non-root user plus `doas`, and `doas` would
have to be installed — which, as noted in §4.1, is not currently possible on these routers.

Also still true: `PermitRootLogin yes`. Reducing this to `prohibit-password` is a one-line change
that would close the remaining gap, and I would do it next.

### 4.6 No activity logging → audit logs

**Partial. `auditd` does not exist on this platform.**

```
logger     /usr/bin/logger
syslogd    /sbin/syslogd
rsyslogd   MISSING
auditd     MISSING
auditctl   MISSING
```

So kernel-level audit is not available. What I implemented instead is **FRR command accounting**,
which for a router is arguably the more relevant audit trail — it records every configuration
command, who ran it and at what prompt depth:

```
log commands
log file /var/log/frr/audit.log informational
```

Verified by making a real change and finding it in the log:

```
2026/11/16 13:14:30 OSPF:   [M7Q4P-46WDR] vty[20]@(config-if)# end
2026/11/16 13:14:30 ZEBRA:  [M7Q4P-46WDR] vty[47]@(config)# end
2026/11/16 13:14:30 MGMTD:  [M7Q4P-46WDR] vty[24]@> enable
grep -c 'AUDIT-TEST-MARKER' => 2   (the test change was captured)
```

Rolled out to all three routers. SSH authentication events are already logged to
`/var/log/messages` by sshd — which is how the incident in §3.6 was found.

**What is still missing:** log retention and centralisation. Logs are local, rotate by size, and the
first incident sweep was partly compacted away during this session. An audit log that an attacker
can erase is not an audit log. Shipping to the controller (or the dashboard's existing SQLite
backend) is the next step.

### 4.7 Network segmentation: Management / Transit / Server / User

**Works — 10/10.** Tested by `segmentation.sh`, which asserts *negatives* wherever possible, because
a positive-only test cannot distinguish a segmented network from a flat one.

```
---- plane 1: Management --------------------------------------------
PASS  controller -> router mgmt 192.168.23.11                    (yes)
PASS  controller -> router mgmt 192.168.23.12                    (yes)
PASS  controller -> router mgmt 192.168.23.13                    (yes)

---- plane 2: Router Transit ---------------------------------------
PASS  transit r1 10.0.1.1 -> r2 10.0.1.2 (OSPF link up)          (yes)
PASS  transit r1 10.0.3.1 -> r3 10.0.3.2 (OSPF link up)          (yes)

---- plane 3+4: User and Server planes cannot reach Management ----
PASS  USER-vlan10   cannot reach management 192.168.23.11        (blocked, correct)
PASS  SERVER-vlan70 cannot reach management 192.168.23.11        (blocked, correct)
PASS  CCTV-vlan90   cannot reach management 192.168.23.11        (blocked, correct)

---- plane 3 vs 4: Server plane is not a user plane ---------------
PASS  acct never learns Server MAC (L2 separated)                (clean)
PASS  acct never learns HR MAC     (L2 separated)                (clean)

---- summary: 10 passed, 0 failed -----------------------------------
```

The four planes are now genuinely separate:

| Plane | Subnet | Carries |
| --- | --- | --- |
| Management | `192.168.23.0/24` | controller ↔ router admin only; **not in OSPF** |
| Router Transit | `10.0.1/2/3.0/29` | OSPF + routed traffic only |
| Server | `10.10.70.0/24` | published services (80/443), reachable per ACL |
| User | `10.10.10/20/40.0/24` | workstations, mutually reachable, blocked from Servers except published ports |

This only became true after the §3.7 fix. Before it, management carried everything.

---

## 5. Things that did not work, and why

| Item | Status | Reason |
| --- | --- | --- |
| VLAN tags across VMs | **Not possible** | VMware port group strips 802.1Q; needs port group VLAN 4095, unreachable from the guest. Worked around with in-VM bridge + namespaces. |
| `apk add` on routers | **Not possible** | Dead mirror `foobar.turbo.net.id`, no DNS, no default route. Worked around by putting client tools on the manager. |
| `16_vlan.yml` as designed | **Not runnable** | Requires packages the routers cannot install; correctly excluded from `master.yml`. The working implementation lives in `/root/vlanlab/` on the controller. |
| `auditd` | **Absent** | Not packaged for this image. Substituted FRR command logging. |
| `doas` / `sudo` | **Absent** | Cannot install. RBAC done via SSH forced commands instead. |
| Native FRR RBAC | **Absent** | No per-user privilege model in FRR 10.6. |
| `05_acl.yml` | **Broken** | Wrong syntax + two unfixable verification greps + `failed_when: false`. Never applies the ACL, never reports the failure. |
| `04_snmp.yml` | **Unchanged** | Still deploys v2c `rocommunity public`. My v3 migration was applied by hand, not through the pipeline. |

---

## 6. Recommended next steps

1. **Fix `05_acl.yml`** — change `access-class 10 in` → `access-class 10`, fix both verification
   greps, and remove `failed_when: false` from the apply task so a failed application cannot pass
   silently.
2. **Fold the SNMPv3 work into `04_snmp.yml`** so a fresh deploy does not reintroduce `public`.
3. **Fix `02_ospf.yml`** — `lan_subnet` must be the transit networks, not `192.168.23.0/24`.
4. **Set `PermitRootLogin prohibit-password`** on all three routers (one line, closes the remaining
   SSH gap).
5. **Move `hosts.ini` passwords into Vault** — the mechanism works (§4.3); it just needs adopting
   via the `hosts.ini.example` template that already references `{{ vault_router_password }}`.
6. **Ship logs off-box** so incident history survives rotation.
7. **Rotate the credentials listed in `SECURITY.md`** — the incident in §3.6 is a live reminder that
   they are being actively tried.

---

## 7. Reproducing this

On the controller, as root:

```sh
cd /root/vlanlab
sh run.sh              # build -> verify -> acl -> policy (order matters)
sh segmentation.sh     # four-plane proof
```

Individual stages: `build.sh`, `verify.sh`, `acl.sh`, `policy.sh`.

Router-side changes made during this work are all `write memory`'d, with pre-change backups at
`/root/frr.conf.bak-preospf`, `/root/frr.conf.pre-vtyacl`, `/root/sshd_config.prekeyauth2`, and
`/etc/snmp/snmpd.conf.v2bak-<ts>`.
