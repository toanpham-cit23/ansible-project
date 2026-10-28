#!/bin/sh
set -e
cd "$(dirname "$0")"

if [ ! -f .env ]; then
  cat > .env <<EOF
JWT_SECRET=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
NETDB_KEY=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
ADMIN_USER=${ADMIN_USER:-admin}
ADMIN_PASSWORD=${ADMIN_PASSWORD:-$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 16)}
EOF
  echo "admin password: $(grep ADMIN_PASSWORD .env | cut -d= -f2)"
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

for dev, pw in ((1, 'r1'), (2, 'r2'), (3, 'r3'), (10, 'alpinelinux')):
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
