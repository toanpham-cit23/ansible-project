FROM alpine:3.20

# A simulated endpoint that behaves like a real host.
#
# The plain `alpine:3.20` these devices used to run had two problems that made
# the lab lie about itself:
#
#  1. No sshd. The dashboard's Terminal console opens a real SSH session, so
#     every button on every simulated PC failed with "ssh failed: connection
#     refused". A device you cannot log in to is not a device you can test.
#
#  2. No init. The container ran `sleep infinity` as PID 1, which does not reap
#     orphaned children. When snmpd crashed it was left as `[snmpd] <defunct>`
#     and nothing restarted it, so the device answered ICMP while port 161 was
#     silent - "reachable, unmonitored" for ever, with no way to notice the
#     agent had died. entrypoint.sh is a real supervisor for both daemons.
RUN apk add --no-cache \
        openssh-server \
        net-snmp \
        iproute2 \
        busybox-extras \
        iputils \
        procps \
 && ssh-keygen -A \
 && mkdir -p /run/sshd /var/lib/snmp /etc/snmp \
 && rm -f /etc/motd

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh

EXPOSE 22
EXPOSE 161/udp

# No CMD: the entrypoint is the whole story, and a stray `sleep infinity` here
# would silently win over it if someone ever appended arguments.
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
