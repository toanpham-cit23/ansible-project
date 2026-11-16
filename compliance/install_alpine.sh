#!/bin/sh
# Install the read-only compliance runner on an Alpine Linux Controller.
#
# Two modes:
#   default        Python runner + Ansible (the historical setup)
#   --go           Go binary only: no Python, PyYAML or Ansible needed. This is
#                  the lighter option and is what --native collection requires.
#
# Both modes collect configuration read-only. Neither writes to a router.
set -eu
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$ROOT"

MODE=python
case "${1:-}" in
  --go) MODE=go ;;
  "") ;;
  *) echo "usage: sh compliance/install_alpine.sh [--go]" >&2; exit 2 ;;
esac

if [ "$(id -u)" -ne 0 ]; then
  echo "Run as root: sh compliance/install_alpine.sh" >&2
  exit 1
fi

if [ "$MODE" = go ]; then
  apk add --no-cache go openssh-client
  make -C compliance/go build
  chmod 700 compliance/reports compliance/reports/actual 2>/dev/null || true
  chmod +x compliance/run.sh
  echo "Installed (Go). Next: copy hosts.ini.example to hosts.ini and set valid SSH access."
  echo "Run: ./compliance/run.sh hosts.ini --native"
  echo "  --native collects over SSH directly, so Ansible is not required."
  exit 0
fi

apk add --no-cache python3 py3-pip py3-virtualenv openssh-client sshpass
python3 -m venv .venv
.venv/bin/python -m pip install --upgrade pip
.venv/bin/python -m pip install ansible-core PyYAML
chmod 700 compliance/reports compliance/reports/actual 2>/dev/null || true
chmod +x compliance/run.sh

echo "Installed. Next: copy hosts.ini.example to hosts.ini and set valid SSH access."
echo "Run: ./compliance/run.sh hosts.ini"
echo "Optional: make -C compliance/go build && ./compliance/run.sh hosts.ini --native"
echo "          (no Python or Ansible at runtime)"
