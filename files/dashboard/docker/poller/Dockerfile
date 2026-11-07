FROM python:3.12-alpine

# net-snmp-tools: the walks and gets this poller is built around.
# openssh-client: fallback for the syslog tail behind automated error detection.
# paramiko is the primary path there; the ssh binary keeps it working if the
# wheel cannot be built for the target platform.
RUN apk add --no-cache net-snmp-tools openssh-client tini \
 && pip install --no-cache-dir cryptography==43.* paramiko==3.5.*

WORKDIR /app
COPY poller.py .

RUN adduser -D -u 10002 poller && chown -R poller:poller /app
USER poller

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["python", "-u", "poller.py"]
