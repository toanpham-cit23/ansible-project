# User simulator

The lab has three routers and no workstations. That is fine for polling but it
leaves the monitoring dashboard drawing an empty segment: a host only appears
after it has answered on the wire, and nothing was answering.

This replaces eight virtual machines with eight containers. Each one owns a real
MAC address on the lab segment, so the routers learn it exactly as they would
learn a PC that somebody plugged in. Nothing in the dashboard knows the
difference, which is the point — the leases are real leases.

## Why macvlan

A container on a normal Docker bridge sits behind NAT on `172.x`. The routers
would never see it. `macvlan` gives the container an interface on the physical
segment with its own MAC, so it appears in the router's ARP table like any other
machine.

The trade-off is that a macvlan container cannot talk to its own host. That does
not matter here: the containers only need to reach the routers, and the
dashboard reaches the routers over SNMP.

## Use

```sh
doas python3 /root/lab-sim/simctl.py up          # all eight
doas python3 /root/lab-sim/simctl.py up 3        # only the first three
doas python3 /root/lab-sim/simctl.py status      # containers vs leases
doas python3 /root/lab-sim/simctl.py names       # copy friendly names into the leases
doas python3 /root/lab-sim/simctl.py down        # stop and remove
doas python3 /root/lab-sim/simctl.py down --network   # ...and the macvlan network
```

`up` is idempotent: a container that is already running is left alone.

After `up`, wait about five seconds (one poll cycle) and open **Monitor**. The
PCs that were showing *no address yet* start showing an address.

## Terminating a connection

In **Leases**, every row has a `terminate` button. It is not a flag in a table
that nothing reads: the API records the cut, the reconciler pauses that
container, the host genuinely stops answering, its ARP entry ages out on the
routers, and the poller reports it offline because it is.

What the operator sees:

* the lease stays in the table, marked **terminated**, with the address and the
  time it was cut
* an `events` row at severity `high` — *connection terminated: HR PC
  (192.168.23.151) - cut by admin*
* a `lease_events` row, so the address history reads sensibly
* in **Monitor**, the PC turns red and dashed, its drop gets a cross, and the
  status strip counts it under *terminated*

`restore` undoes all of it.

If the reconciler is not running the container keeps answering while the table
says terminated. The poller will not warn about that, on purpose: a router keeps
its ARP entry for minutes after the host stops talking, so presence in the ARP
table is not evidence that anything is alive. Check the pause directly instead:

```sh
docker inspect sim-hr --format '{{.State.Paused}}'
```

## The reconciler

`simctl.py watch` runs as the `lab-sim` OpenRC service:

```sh
rc-service lab-sim status
tail -f /var/log/lab-sim.log
```

It polls the dashboard every five seconds and pauses or unpauses containers to
match. The dashboard itself cannot do this: it runs read-only with no Docker
socket, and giving a monitoring API root on the host to save one small daemon
would be a bad trade.

## What each container does

```sh
while :; do
  for t in 192.168.23.11 192.168.23.12 192.168.23.13; do ping -c1 -W1 "$t"; done
  nc -z -w1 192.168.23.11 22
  sleep 3
done
```

Pinging all three routers is deliberate: each router keeps its own ARP table, so
a host that only talks to `r1` is invisible to `r2` and `r3`. The TCP attempt
keeps the traffic from looking like a bare ICMP probe.

## The addresses

| Container | Name | VLAN | MAC | Address |
|---|---|---|---|---|
| sim-accounting | Accounting PC | 10 | 02:00:00:00:0A:01 | 192.168.23.150 |
| sim-hr | HR PC | 20 | 02:00:00:00:14:01 | 192.168.23.151 |
| sim-sales | Sales PC | 40 | 02:00:00:00:28:01 | 192.168.23.152 |
| sim-fileserver | File server | 70 | 02:00:00:00:46:01 | 192.168.23.153 |
| sim-director | Director PC | 50 | 02:00:00:00:32:01 | 192.168.23.154 |
| sim-ap | Access point | 60 | 02:00:00:00:3C:01 | 192.168.23.155 |
| sim-nvr | NVR | 95 | 02:00:00:00:5F:01 | 192.168.23.156 |
| sim-camera | Camera | 90 | 02:00:00:00:5A:01 | 192.168.23.157 |

The MAC addresses are the join key. `static/js/app.js` matches a lease to a
designed PC by MAC, so if you change one here you have to change it there too,
or the PC will fall back to *no address yet* while the lease sits in the table
unplaced.

The VLAN column is the VLAN each PC belongs to **in the design**. The addresses
are all on the flat management segment, because that is the only segment the
lab actually has — there is no trunk to a switch, so tagged frames would go
nowhere. The diagram places the PC under the switch that serves its VLAN; the
address it shows is the one it really holds.

## Limits

* Eight containers use about 8 MB of RAM in total and no measurable CPU. They
  are capped at 24 MB and 0.05 CPU each so they can never crowd out the
  dashboard.
* They carry `--restart unless-stopped`, so a controller reboot brings the lab
  back on its own. Without that the dashboard shows an empty network after a
  reboot and nothing says why. `simctl.py down` still stops them properly.
* The loop wraps its TCP probe in `timeout 2`. busybox `nc` does not always
  honour `-w` on a connect that never completes, and a shell blocked in `nc`
  stops pinging — the host then ages out of the router's ARP table while the
  container still looks healthy. That is how all eight went offline once.
* If the VMware port group blocks forged source MAC addresses, containers will
  start but no lease will ever appear. Check with `status`: if `RUNNING` says
  yes and `LEASE` stays `-`, that is the cause. The port group needs
  *Promiscuous Mode* and *Forged Transmits* set to Allow.
