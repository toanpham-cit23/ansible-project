#!/bin/sh
set -e
cd "$(dirname "$0")"

if [ ! -f .env ]; then
  # ADMIN_PASSWORD is what the API reads (main.go: env("ADMIN_PASSWORD")).
  # It was previously written as ADMIN_HXLPCHZY, which the API never looks at,
  # so the admin password silently fell through to the generated one and the
  # configured value was ignored. It is also generated now rather than hardcoded,
  # because a literal here would be published the moment this file is committed.
  # ADMIN_USER and ADMIN_PASSWORD fall back to generated values but can be
  # supplied by the caller, which is what CI and the Ansible playbook do:
  #   ADMIN_PASSWORD=... ./setup.sh
  ADMIN_PW=${ADMIN_PASSWORD:-$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 16)}
  cat > .env <<EOF
JWT_SECRET=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
NETDB_KEY=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
ADMIN_USER=${ADMIN_USER:-admin}
ADMIN_PASSWORD=$ADMIN_PW
EOF
  chmod 600 .env
  echo "Wrote .env (mode 600). Admin password: $ADMIN_PW"
fi

mkdir -p db
rm -f db/net.db db/net.db-wal db/net.db-shm
python3 - <<'PY'
import os
import sqlite3

c = sqlite3.connect('db/net.db')
c.executescript(open('db/schema.sql', encoding='utf-8').read())
c.executescript(open('db/seed.sql', encoding='utf-8').read())

env = dict(l.split('=', 1) for l in open('.env').read().splitlines() if '=' in l)
key = bytes.fromhex(env['NETDB_KEY'].strip())
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

def enc(text):
    n = os.urandom(12)
    return n + AESGCM(key).encrypt(n, text.encode(), None)

# Device credentials come from the environment, not from this file. DEV_PW_<id>
# overrides a device; the fallback keeps the lab script working out of the box
# without embedding a literal that would ship with the repository.
#   DEV_PW_1=r1 DEV_PW_10=alpinelinux ./setup.sh
DEFAULT_DEV_PW = os.environ.get('DEV_PW_DEFAULT', 'alpine')
for dev in (1, 2, 3, 10):
    pw = os.environ.get(f'DEV_PW_{dev}', DEFAULT_DEV_PW)
    c.execute('UPDATE devices SET password_enc=? WHERE id=?', (enc(pw), dev))
c.commit()
c.close()
print('db ready')
PY

chown -R 10001:10001 db
chmod 700 db
docker compose up -d --build
sleep 8
docker compose ps
