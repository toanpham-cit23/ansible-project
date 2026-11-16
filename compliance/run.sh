#!/bin/sh
# Compliance runner.
#
# Prefers the Go binary when it has been built (make -C compliance/go), because
# it has no Python, PyYAML or Ansible dependency. Falls back to the Python
# script so the documented workflow keeps working on a Controller that has only
# had install_alpine.sh run against it.
#
# Any extra arguments are passed through, so `./compliance/run.sh hosts.ini`
# still works and `./compliance/run.sh hosts.ini --native` is also valid.
set -eu
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$ROOT"

INVENTORY="${1:-hosts.ini}"
case "$INVENTORY" in
  -*) INVENTORY="hosts.ini" ;;   # first arg was a flag, not a path
  *)  shift || true ;;
esac

for BIN in "$ROOT/compliance/bin/compliance" "$ROOT/compliance/go/compliance"; do
  if [ -x "$BIN" ]; then
    exec "$BIN" --inventory "$INVENTORY" "$@"
  fi
done

if [ -x "$ROOT/.venv/bin/python" ]; then
  PATH="$ROOT/.venv/bin:$PATH"
  export PATH
  exec "$ROOT/.venv/bin/python" compliance/scripts/run_compliance.py \
    --inventory "$INVENTORY" "$@"
fi

exec python3 compliance/scripts/run_compliance.py --inventory "$INVENTORY" "$@"
