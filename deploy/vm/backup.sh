#!/usr/bin/env bash
# Nightly PostgreSQL backup on the VM (cron, 02:45 IST): a compressed dump in
# /opt/siliconics/backups, keeping the last 7. Restore with:
#   gunzip -c <file> | docker compose ... exec -T postgres psql -U siliconia siliconia
set -euo pipefail
ROOT=/opt/siliconics
DIR="$ROOT/backups"
mkdir -p "$DIR"
file="$DIR/siliconics-$(date -u +%Y%m%d-%H%M).sql.gz"
docker compose -f "$ROOT/siliconia_backend/deploy/compose/docker-compose.yml" --env-file "$ROOT/siliconia_backend/deploy/compose/.env" \
  exec -T postgres pg_dump -U siliconia --clean --if-exists siliconia | gzip > "$file"
ls -1t "$DIR"/siliconics-*.sql.gz | tail -n +8 | xargs -r rm --
echo "$(date -u +%FT%TZ) backup $(du -h "$file" | cut -f1) $file"
