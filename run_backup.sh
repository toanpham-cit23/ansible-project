#!/bin/sh
export HOME=/root
LOG=/var/log/ansible_backup.log

# keep the log small - last 300 lines if it grows over 1 MB
if [ -f "$LOG" ] && [ "$(wc -c "$LOG")" -gt 1024000 ]; then
  tail -n 300 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
fi

echo "=== $(date) ===" >> "$LOG"
cd /root/ansible-project
AP=$(command -v ansible-playbook || echo /usr/bin/ansible-playbook)
"$AP" playbooks/06_backup.yml >> "$LOG" 2>&1
