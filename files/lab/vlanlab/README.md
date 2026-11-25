# VLAN fabric test harness

These scripts build and prove an 802.1Q VLAN fabric **inside the controller VM**,
using a Linux bridge with `vlan_filtering 1` plus network namespaces.

## Why namespaces instead of real switches

The design file (`files/lab/vlans.yml`) correctly states that tagged frames do not
reach the VLANs today, and this was confirmed rather than assumed: `tcpdump` shows
the controller transmitting `ethertype 802.1Q (0x8100) ... vlan 999` while the far
side's `eth0.999` RX counter stays at exactly 0 across ten tagged pings.

The cause is the VMware port group dropping 802.1Q tags. Fixing it requires setting
the port group's VLAN ID to 4095, which cannot be done from inside a guest.

So the fabric is built from the same kernel primitives a physical switch uses, in one
VM. Frames are genuinely tagged and filtered by the kernel — this is a real data
plane, not a simulation of one.

## Run order matters

```sh
sh run.sh           # build -> verify -> acl -> policy
sh segmentation.sh  # four-plane proof (management / transit / server / user)
```

`run.sh` enforces the order on purpose. `verify.sh` asserts that **everything reaches
everything**, which proves the routing is correct; `acl.sh` then installs the policy;
`policy.sh` asserts the same paths are now **blocked**. Running `verify.sh` after
`acl.sh` produces four FAILs that are correct behaviour, not faults.

## Files

| File | Purpose |
| --- | --- |
| `build.sh` | Creates `br-lab`, 9 namespaces, 6 access ports, 3 trunks, 5 SVIs, transit triangle |
| `verify.sh` | 9 checks: L2 isolation (by MAC learning, not ping), SVI gateways, cross-router routing, trunk tagging |
| `acl.sh` | nftables per router: established accept, CCTV isolation, Accounting↔HR block, Servers limited to 80/443/53 |
| `policy.sh` | The acceptance test: acct→HR blocked, acct→srv 80/443 allowed, 22/3306 dropped, with a negative control |
| `segmentation.sh` | Asserts the four planes are separate, mainly by proving what *cannot* reach what |
| `srvlisten.py` | Forking listener — busybox `nc -l` exits after one connection |
| `snmpv3_apply.sh` | v2c→v3 migration for one agent |

## Reading the output

`CONNECTED` / `ConnectionRefused` = forwarded, ACL permitted.
`TimeoutError` = dropped, ACL denied.

A blocked port looks like a **timeout**, not a refusal. Do not write a check that
treats "no connection" as a pass without also confirming the permitted case works —
that is how a broken probe turns into a false PASS.

## Two traps

- **busybox has no `/dev/tcp`.** `exec 3<>/dev/tcp/...` fails, which silently makes
  every "blocked" case look like a pass. Use `python3 socket`.
- **Detached background processes die with the SSH session**, and busybox `nc -l`
  exits after one connection. Listeners must be started in the same invocation as
  the probes — `policy.sh` does this.

## Provenance

Executed against the live lab on 2026-11-16. Full results and the defects this
exercise uncovered: see `VLAN-and-Security-Test-Report.md` in the repository root.
