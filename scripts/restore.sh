#!/bin/bash
# Restore PostgreSQL from backup
# Usage: ./scripts/restore.sh <backup_file>
set -euo pipefail

BACKUP_FILE="${1:?Usage: restore.sh <backup_file>}"
DB_URL="${DATABASE_URL:-postgres://ae_user:ae_password@postgres:5432/autonomous_engine}"

if [[ ! -f "$BACKUP_FILE" ]]; then
    echo "Error: Backup file not found: $BACKUP_FILE"
    exit 1
fi

echo "WARNING: This will overwrite the current database."
echo "Restoring from: $BACKUP_FILE"
echo "Press Ctrl+C to cancel, or wait 5 seconds to continue..."
sleep 5

gunzip -c "$BACKUP_FILE" | psql "$DB_URL"

echo "Restore complete."
