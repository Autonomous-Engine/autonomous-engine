#!/bin/bash
# Run as a cron job for scheduled backups
# Add to crontab: 0 2 * * * /path/to/backup-cron.sh
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
"$SCRIPT_DIR/backup.sh" /backups 2>&1 | logger -t ae-backup
