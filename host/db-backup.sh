#!/usr/bin/env bash
# Nightly dump of the platform's own database (organizations, users, settings,
# invoices...). Client data is backed up separately by the platform itself.
# Run by platform-db-backup.timer; keeps 14 days in /var/backups/platform.
# Copy that folder off the server too (it is useless if the disk dies).
set -euo pipefail
DIR=${PLATFORM_DIR:-/opt/platform}
OUT=/var/backups/platform
KEEP_DAYS=${KEEP_DAYS:-14}
mkdir -p "$OUT"
chmod 700 "$OUT"
file="$OUT/platform-$(date -u +%Y-%m-%dT%H%MZ).sql.gz"
cd "$DIR/deploy"
docker compose -f compose.prod.yml exec -T postgres pg_dump -U platform_owner --clean --if-exists platform </dev/null | gzip -9 > "$file.tmp"
# A dump that does not end with the completion marker is incomplete: keep the old ones.
if ! gzip -dc "$file.tmp" | tail -n 5 | grep -q "PostgreSQL database dump complete"; then
  rm -f "$file.tmp"
  echo "dump incomplete, previous backups kept" >&2
  exit 1
fi
mv "$file.tmp" "$file"
chmod 600 "$file"
find "$OUT" -name 'platform-*.sql.gz' -mtime +"$KEEP_DAYS" -delete
echo "backup written: $file ($(du -h "$file" | cut -f1))"
