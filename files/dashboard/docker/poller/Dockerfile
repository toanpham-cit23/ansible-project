FROM python:3.12-alpine

RUN apk add --no-cache net-snmp-tools tini \
 && pip install --no-cache-dir cryptography==43.*

WORKDIR /app
COPY poller.py .

RUN adduser -D -u 10002 poller && chown -R poller:poller /app
USER poller

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["python", "-u", "poller.py"]
