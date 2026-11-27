#!/usr/bin/env python3
"""Docker-based user simulator for the lab.

The lab has three routers and no PCs, and a monitoring dashboard can only draw
a host after that host answered on the wire. Standing up eight virtual machines
for eight pretend workstations is a waste of a machine that has no capacity to
spare, so this does the same job with eight containers.

Each container gets its own MAC address on a macvlan interface hanging off the
controller's lab NIC. To the routers it is indistinguishable from a workstation
that was plugged in: it ARPs, it answers, it holds an address. The poller reads
that address out of the router's ARP table and the dashboard draws the PC.

    doas python3 /root/lab-sim/simctl.py up
    doas python3 /root/lab-sim/simctl.py status
    doas python3 /root/lab-sim/simctl.py down

`watch` is what makes a terminated connection real: it pauses the container
whose lease the operator cut, so the host stops answering instead of merely
being marked offline. It runs as the lab-sim service.

The MAC addresses below are the join key with the dashboard: the topology view
matches a lease to a designed PC by MAC, so do not change one without the other.
"""

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request
import http.cookiejar

# ---------------------------------------------------------------- settings
PARENT = os.environ.get("SIM_PARENT", "eth0")
NETWORK = os.environ.get("SIM_NETWORK", "lab-users")
# Built from files/sim/Dockerfile: alpine + sshd + net-snmp + an entrypoint that
# supervises both. The stock alpine image has neither daemon, so a simulated PC
# could not be logged in to and its SNMP agent could die unnoticed.
IMAGE = os.environ.get("SIM_IMAGE", "lab/simhost:1.0")
BUILD_DIR = os.environ.get("SIM_BUILD_DIR", os.path.dirname(os.path.abspath(__file__)))
SUBNET = os.environ.get("SIM_SUBNET", "192.168.23.0/24")
GATEWAY = os.environ.get("SIM_GATEWAY", "192.168.23.1")
IPRANGE = os.environ.get("SIM_IPRANGE", "192.168.23.144/28")
DASHBOARD = os.environ.get("SIM_DASHBOARD", "http://127.0.0.1:8080")
ENVFILE = os.environ.get("SIM_ENVFILE", "/root/lab/.env")
# Lab-only credential, the same on every simulated host. Not a secret: these
# containers exist to be broken on purpose.
DEV_USER = os.environ.get("SIM_DEV_USER", "root")
DEV_PASS = os.environ.get("SIM_DEV_PASS", "lab12345")
ROUTERS = os.environ.get("SIM_ROUTERS", "192.168.23.11 192.168.23.12 192.168.23.13").split()

# slug, friendly name, designed VLAN, MAC, address. Must match LAB.hosts in
# static/js/app.js or the topology view will not be able to place the host.
HOSTS = [
    ("accounting", "Accounting PC", 10, "02:00:00:00:0A:01", "192.168.23.150"),
    ("hr", "HR PC", 20, "02:00:00:00:14:01", "192.168.23.151"),
    ("sales", "Sales PC", 40, "02:00:00:00:28:01", "192.168.23.152"),
    ("fileserver", "File server", 70, "02:00:00:00:46:01", "192.168.23.153"),
    ("director", "Director PC", 50, "02:00:00:00:32:01", "192.168.23.154"),
    ("ap", "Access point", 60, "02:00:00:00:3C:01", "192.168.23.155"),
    ("nvr", "NVR", 95, "02:00:00:00:5F:01", "192.168.23.156"),
    ("camera", "Camera", 90, "02:00:00:00:5A:01", "192.168.23.157"),
]

# The keepalive used to live here as a shell string passed to `docker run ...
# sh -c LOOP`. It moved into the image's entrypoint.sh when the hosts stopped
# being plain alpine containers: the same process now has to supervise sshd and
# snmpd as well, and `sleep infinity` as PID 1 neither reaps nor restarts. The
# logic is unchanged - ping the routers, one TCP touch - but it is a child of a
# supervisor instead of the whole container.


def sh(args, check=True, capture=True):
    r = subprocess.run(args, shell=False, capture_output=capture, text=True)
    if check and r.returncode != 0:
        raise SystemExit("command failed: %s\n%s" % (" ".join(args), (r.stderr or "").strip()))
    return r


def docker(*args, **kw):
    return sh(["docker"] + list(args), **kw)


def container(slug):
    return "sim-" + slug


def running():
    out = docker("ps", "--format", "{{.Names}}", capture=True).stdout.split()
    return {n for n in out if n.startswith("sim-")}


def ensure_image(verbose=True):
    """Build lab/simhost if it is not there yet.

    The stock alpine image cannot be logged in to and has no SNMP agent, so
    `docker run alpine sleep infinity` produces a host that answers ping and
    nothing else. That is the device the dashboard could only ever describe as
    "reachable, unmonitored". Building once and reusing is cheap; the build is
    a package install and a COPY.
    """
    if docker("image", "inspect", IMAGE, check=False).returncode == 0:
        return
    if verbose:
        print("building %s from %s" % (IMAGE, BUILD_DIR))
    docker("build", "-t", IMAGE, BUILD_DIR)


def ensure_network(verbose=True):
    nets = docker("network", "ls", "--format", "{{.Name}}").stdout.split()
    if NETWORK in nets:
        return
    if verbose:
        print("creating macvlan network %s on %s" % (NETWORK, PARENT))
    docker("network", "create", "-d", "macvlan",
           "--subnet", SUBNET, "--gateway", GATEWAY,
           "--ip-range", IPRANGE, "-o", "parent=" + PARENT, NETWORK)


# ------------------------------------------------------------- host shim
#
# A macvlan child is a *sibling* of the host NIC, not a child of it. The parent
# interface cannot reach the containers it hosts: the kernel sends the frame
# out of eth0, the destination MAC belongs to the child, and it never comes
# back. So the controller could not ping, snmpget or ssh the eight workstations
# it had just created, and Monitor drew all of them as an outage while Leases
# - reading the routers' ARP tables - still called them online.
#
# That disagreement was the bug the operator noticed. The fix is the standard
# one: a second macvlan interface on the same parent in bridge mode. It is a
# sibling, so traffic to a child leaves by it and arrives normally.
SHIM_DEV = os.environ.get("SIM_SHIM_DEV", "mvl0")
SHIM_IP = os.environ.get("SIM_SHIM_IP", "192.168.23.200/32")


def shim_routes():
    """Addresses the host must reach through the shim.

    The configured macvlan range plus any extra prefixes (the test devices sit
    outside the range because they predate it).
    """
    extra = os.environ.get("SIM_SHIM_EXTRA", "192.168.23.51/32 192.168.23.52/32 192.168.23.53/32")
    return [IPRANGE] + [e for e in extra.split() if e]


def ensure_shim(verbose=True):
    """Create the host-side macvlan interface. Idempotent.

    Called from `up` and from the boot service, so a reboot cannot leave the
    lab monitored-but-unreachable - which is exactly how it looked before.
    """
    have = sh(["ip", "link", "show", SHIM_DEV], check=False, capture=True)
    if have.returncode != 0:
        if verbose:
            print("creating shim %s on %s (macvlan bridge)" % (SHIM_DEV, PARENT))
        r = sh(["ip", "link", "add", SHIM_DEV, "link", PARENT,
                "type", "macvlan", "mode", "bridge"], check=False)
        if r.returncode != 0:
            # Not fatal: without it the lab is monitored from the routers only.
            print("warning: could not create %s: %s" % (SHIM_DEV, (r.stderr or "").strip()))
            return False

    addr = sh(["ip", "addr", "show", SHIM_DEV], check=False).stdout
    if SHIM_IP.split("/")[0] not in addr:
        sh(["ip", "addr", "add", SHIM_IP, "dev", SHIM_DEV], check=False)
    sh(["ip", "link", "set", SHIM_DEV, "up"], check=False)

    routes = sh(["ip", "route", "show"], check=False).stdout
    for cidr in shim_routes():
        if ("%s dev %s" % (cidr, SHIM_DEV)) not in routes:
            sh(["ip", "route", "add", cidr, "dev", SHIM_DEV], check=False)
    if verbose:
        print("shim ready: %s %s" % (SHIM_DEV, SHIM_IP))
    return True


def owned():
    """Every container this tool created, by label.

    Not by name: a container can end up with a name nobody chose (one did, and
    held the Camera address for an hour while looking like a normal `sim-*`).
    The label is the only reliable marker of ownership.
    """
    out = docker("ps", "-a", "--filter", "label=lab.sim=1",
                 "--format", "{{.Names}}", check=False).stdout.split()
    return set(out)


def restart_policy(cname):
    r = docker("inspect", "--format", "{{.HostConfig.RestartPolicy.Name}}", cname, check=False)
    return r.stdout.strip() if r.returncode == 0 else None


def cmd_up(args):
    ensure_network()
    ensure_image()
    # Without this the hosts come up and the dashboard cannot see them.
    ensure_shim()
    todo = HOSTS[:args.count] if args.count else HOSTS
    expected = {container(h[0]) for h in todo}
    have = running()

    # Anything we own that is not in the plan is in the way - it may still be
    # holding one of the addresses. Remove it before trying to claim them.
    for stray in owned() - expected:
        docker("rm", "-f", stray, check=False)
        print("removed stray container %s" % stray)

    started = 0
    for slug, name, vlan, mac, ip in todo:
        cname = container(slug)
        if cname in have:
            if restart_policy(cname) == "unless-stopped":
                continue
            # It survived from before the policy was added. Recreate it, or the
            # next reboot takes the whole lab down and nobody notices until the
            # dashboard is empty.
            print("recreating %-14s (restart policy was %r)" % (cname, restart_policy(cname)))
            docker("rm", "-f", cname, check=False)
        elif docker("inspect", "--format", "{{.Id}}", cname, check=False).returncode == 0:
            # A stopped container is restarted, not rebuilt: it already has the
            # right address and the right policy.
            docker("start", cname, check=False)
            print("started %-14s %-14s (existing)" % (cname, ip))
            started += 1
            continue
        docker("run", "-d", "--name", cname,
               # The lab is a demo: if the controller reboots, the hosts have to
               # come back or the dashboard shows an empty network and nobody
               # knows why. `unless-stopped` still respects a manual `down`.
               "--restart", "unless-stopped",
               "--network", NETWORK, "--ip", ip, "--mac-address", mac,
               "--memory", "64m", "--cpus", "0.05",
               "--label", "lab.sim=1", "--label", "lab.sim.name=" + name,
               # LABEL is the inventory name, not the container id: snmpd
               # publishes it as sysName and the compliance rule compares the
               # two. ROUTERS/PEER drive the keepalive that keeps this host in
               # the routers' ARP tables. ROOT_PW is the credential the
               # dashboard stores for the Terminal console.
               "-e", "LABEL=" + slug,
               "-e", "ROOT_PW=" + DEV_PASS,
               "-e", "ROUTERS=" + " ".join(ROUTERS),
               "-e", "PEER=" + ROUTERS[0],
               IMAGE)
        print("started %-14s %-14s %s" % (cname, ip, mac))
        started += 1
    if not started:
        print("nothing to do - all %d already running" % len(todo))
    print("\nthe routers learn a MAC on the next poll, about 5 s.")
    print("check with: %s status" % sys.argv[0])


def cmd_down(args):
    removed = 0
    for cname in sorted(owned()):
        docker("rm", "-f", cname, check=False)
        print("removed " + cname)
        removed += 1
    if not removed:
        print("nothing running")
    if args.network:
        docker("network", "rm", NETWORK, check=False)
        print("removed network " + NETWORK)
    else:
        print("\nnetwork %s kept; pass --network to remove it too" % NETWORK)


# ------------------------------------------------------------------ dashboard
def creds():
    user, pw = None, None
    try:
        with open(ENVFILE) as fh:
            for line in fh:
                k, _, v = line.strip().partition("=")
                if k == "ADMIN_USER":
                    user = v
                elif k == "ADMIN_PASSWORD":
                    pw = v
    except OSError:
        pass
    return user, pw


def api(path, method="GET", body=None, jar=None):
    url = DASHBOARD + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if data:
        req.add_header("Content-Type", "application/json")
    opener = urllib.request.build_opener(
        urllib.request.HTTPCookieProcessor(jar)) if jar is not None else urllib.request.build_opener()
    with opener.open(req, timeout=8) as r:
        raw = r.read().decode()
    return json.loads(raw) if raw.strip() else {}


def login():
    user, pw = creds()
    if not user or not pw:
        raise SystemExit("no ADMIN_USER/ADMIN_PASSWORD in %s" % ENVFILE)
    jar = http.cookiejar.CookieJar()
    api("/api/login", "POST", {"username": user, "password": pw}, jar=jar)
    return jar


def leases(jar):
    return api("/api/leases?limit=300", jar=jar).get("leases", [])


def cmd_status(args):
    have = running()
    try:
        jar = login()
        rows = leases(jar)
    except Exception as e:                                  # noqa: BLE001
        print("(dashboard not reachable: %s)\n" % e)
        rows = []
    by_mac = {str(r.get("mac", "")).upper(): r for r in rows}

    print("%-14s %-16s %-18s %-9s %s" % ("CONTAINER", "ADDRESS", "MAC", "RUNNING", "LEASE"))
    seen = 0
    for slug, name, vlan, mac, ip in HOSTS:
        cname = container(slug)
        row = by_mac.get(mac.upper())
        lease = "-"
        if row:
            lease = row.get("ip", "") + (" online" if int(row.get("online") or 0) else " stale")
            if int(row.get("online") or 0):
                seen += 1
        print("%-14s %-16s %-18s %-9s %s" % (cname, ip, mac, "yes" if cname in have else "no", lease))
    print("\n%d/%d simulated PCs hold a live lease" % (seen, len(HOSTS)))
    if seen < len(have & {container(h[0]) for h in HOSTS}):
        print("containers that are up but not yet leased: the routers learn them on the next poll")

    # Say whether the host can reach its own children. Without the shim every
    # lease above can be held while Monitor shows the same host as down, and
    # that contradiction is invisible unless it is printed.
    probe = sh(["ip", "link", "show", SHIM_DEV], check=False)
    if probe.returncode != 0:
        print("\nwarning: no %s shim - this host cannot reach its own macvlan "
              "containers.\n         Monitor will show them as down. Run: "
              "python3 %s shim" % (SHIM_DEV, sys.argv[0]))
    else:
        down = [ip for _s, _n, _v, _m, ip in HOSTS
                if sh(["ping", "-c", "1", "-W", "2", ip], check=False).returncode != 0]
        if down:
            print("\nwarning: shim present but %d address(es) do not answer: %s"
                  % (len(down), " ".join(down)))


def cmd_names(args):
    jar = login()
    rows = leases(jar)
    by_mac = {str(r.get("mac", "")).upper(): r for r in rows}
    changed = 0
    for slug, name, vlan, mac, ip in HOSTS:
        row = by_mac.get(mac.upper())
        if not row:
            continue
        if row.get("device_name") == name:
            continue
        api("/api/leases/%d" % row["id"], "PATCH", {"device_name": name}, jar=jar)
        print("%-16s -> %s" % (row.get("ip"), name))
        changed += 1
    print("%d lease%s named" % (changed, "" if changed == 1 else "s"))


def cmd_logs(args):
    docker("logs", "--tail", "20", container(args.slug), check=False)


# ---------------------------------------------------------------- reconciler

def container_paused(cname):
    r = docker("inspect", "--format", "{{.State.Paused}}", cname, check=False)
    return r.returncode == 0 and r.stdout.strip() == "true"


def cmd_watch(args):
    """Keep the containers in step with the dashboard's terminated leases.

    Terminating a connection has to mean something on the wire, not just in a
    table. The dashboard cannot touch Docker - it runs read-only with no socket
    - so it records the intent and this loop carries it out: a lease marked
    blocked gets its container paused, and a lease restored gets it unpaused.
    A paused container stops answering, its ARP entry ages out on the routers,
    and the poller reports it offline because it genuinely is.
    """
    ensure_network()
    print("watching for terminated connections every %ds - ctrl-c to stop" % args.interval)
    jar = None
    while True:
        try:
            if jar is None:
                jar = login()
            by_mac = {str(r.get("mac", "")).upper(): r for r in leases(jar)}
            for slug, name, vlan, mac, ip in HOSTS:
                cname = container(slug)
                if docker("inspect", "--format", "{{.Id}}", cname, check=False).returncode != 0:
                    continue                      # not started, nothing to reconcile
                row = by_mac.get(mac.upper())
                want_cut = bool(row and row.get("blocked"))
                is_paused = container_paused(cname)
                if want_cut and not is_paused:
                    docker("pause", cname, check=False)
                    print("%s paused  %s %s" % (time.strftime("%H:%M:%S"), cname, ip))
                elif not want_cut and is_paused:
                    docker("unpause", cname, check=False)
                    print("%s resumed %s %s" % (time.strftime("%H:%M:%S"), cname, ip))
        except KeyboardInterrupt:
            return
        except Exception as e:                     # noqa: BLE001
            print("watch: %s (will retry)" % e)
            jar = None                             # session probably expired
        time.sleep(args.interval)


def cmd_rebuild(args):
    """Replace every simulated host with a container built from the current image.

    `up` is idempotent on purpose: it leaves a running container alone, because
    restarting a lab on every invocation would be surprising. That also means
    `up` cannot pick up a new image - a container created from the old
    `alpine:3.20 sleep infinity` stays exactly as it is. Changing what a
    simulated host *is* (sshd, snmpd, a supervisor that reaps) therefore needs
    an explicit rebuild, which is this command.

    The MAC address is preserved, so the routers' ARP entries and the existing
    leases stay valid; only the container changes underneath.
    """
    ensure_network()
    ensure_image()
    ensure_shim()
    todo = HOSTS[:args.count] if args.count else HOSTS
    for slug, name, vlan, mac, ip in todo:
        cname = container(slug)
        docker("rm", "-f", cname, check=False)
        docker("run", "-d", "--name", cname,
               "--restart", "unless-stopped",
               "--network", NETWORK, "--ip", ip, "--mac-address", mac,
               "--memory", "64m", "--cpus", "0.05",
               "--label", "lab.sim=1", "--label", "lab.sim.name=" + name,
               "-e", "LABEL=" + slug,
               "-e", "ROOT_PW=" + DEV_PASS,
               "-e", "ROUTERS=" + " ".join(ROUTERS),
               "-e", "PEER=" + ROUTERS[0],
               IMAGE)
        print("rebuilt %-14s %-14s %s" % (cname, ip, mac))
    print("\nsshd and snmpd are up on each host; the routers learn the MACs within a poll cycle.")
    print("check with: %s status" % sys.argv[0])


def main():
    p = argparse.ArgumentParser(description="Docker-based user simulator for the lab")
    sub = p.add_subparsers(dest="cmd", required=True)

    up = sub.add_parser("up", help="start the simulated PCs")
    up.add_argument("count", nargs="?", type=int, default=0, help="how many to start (default: all)")
    up.set_defaults(fn=cmd_up)

    down = sub.add_parser("down", help="stop and remove them")
    down.add_argument("--network", action="store_true", help="also remove the macvlan network")
    down.set_defaults(fn=cmd_down)

    rb = sub.add_parser("rebuild", help="replace every host with one built from the "
                                       "current image (up leaves running ones alone)")
    rb.add_argument("count", nargs="?", type=int, default=0, help="how many (default: all)")
    rb.set_defaults(fn=cmd_rebuild)

    st = sub.add_parser("status", help="containers vs leases")
    st.set_defaults(fn=cmd_status)

    sh_ = sub.add_parser("shim", help="(re)create the host macvlan shim so this "
                                     "host can reach its own containers")
    sh_.set_defaults(fn=lambda a: ensure_shim())

    nm = sub.add_parser("names", help="copy the friendly names into the lease table")
    nm.set_defaults(fn=cmd_names)

    lg = sub.add_parser("logs", help="tail one container")
    lg.add_argument("slug")
    lg.set_defaults(fn=cmd_logs)

    wt = sub.add_parser("watch", help="pause containers whose lease is terminated")
    wt.add_argument("--interval", type=int, default=5, help="seconds between checks")
    wt.set_defaults(fn=cmd_watch)

    a = p.parse_args()
    if os.geteuid() != 0:
        raise SystemExit("needs root for docker: doas python3 %s %s" % (sys.argv[0], a.cmd))
    a.fn(a)


if __name__ == "__main__":
    main()
