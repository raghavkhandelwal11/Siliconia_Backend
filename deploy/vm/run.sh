#!/usr/bin/env bash
# Runs a backend command on the VM with the right Compose files and image:
#   run.sh demo                                   # demo store, 12 categories, a launch
#   run.sh admin create --phone 9876543210 --name "Asha"
#   run.sh migrate up
set -euo pipefail
ROOT=/opt/siliconics
BACK="$ROOT/siliconia_backend"
ENVF="$BACK/deploy/compose/.env"
PROFILE=$(grep '^SILICONICS_PROFILE=' "$ENVF" | tail -1 | cut -d= -f2-); PROFILE=${PROFILE:-full}
cmd="${1:?usage: run.sh <demo|admin|migrate|seed> [args...]}"; shift
if [ "$PROFILE" = lite ]; then
  export BACKEND_IMAGE="ghcr.io/raghavkhandelwal11/siliconia-backend:$(git -C "$BACK" rev-parse HEAD)"
  extra="$BACK/deploy/compose/docker-compose.lite.yml"
else
  extra="$BACK/deploy/compose/docker-compose.prod.yml"
fi
exec docker compose -f "$BACK/deploy/compose/docker-compose.yml" -f "$extra" --env-file "$ENVF" \
  run --rm --no-deps api "/usr/local/bin/$cmd" "$@"
