#!/bin/sh
# Wrapper for the semi-annual clearance. Defaults to a dry run so nothing
# destructive can happen by accident; crond calls it with --apply on
# 1 January and 1 July at 03:00 UTC.
set -u
LAB=/root/lab
MODE=""
[ "${1:-}" = "--apply" ] && MODE="--apply"
python3 "$LAB/ops/retention.py" --days "${RETENTION_DAYS:-180}" $MODE
