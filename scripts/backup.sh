#!/bin/bash
# Backup PostgreSQL database
# Usage: ./scripts/backup.sh [backup_dir]
set -euo pipefail

BACKUP_DIR="${1:-/backups}"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
DB_URL="${DATABASE_URL:-postgres://ae_user:ae_password@postgres:5432/autonomous_engine}"

mkdir -p "$BACKUP_DIR"

echo "Starting PostgreSQL backup..."
pg_dump "$DB_URL" | gzip > "$BACKUP_DIR/autonomous_engine_${TIMESTAMP}.sql.gz"

# Keep only last 30 backups
ls -t "$BACKUP_DIR"/autonomous_engine_*.sql.gz | tail -n +31 | xargs -r rm

echo "Backup complete: $BACKUP_DIR/autonomous_engine_${TIMESTAMP}.sql.gz"
echo "Backups retained: $(ls "$BACKUP_DIR"/autonomous_engine_*.sql.gz | wc -l)"
