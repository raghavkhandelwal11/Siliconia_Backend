#!/usr/bin/env bash
# Deploys main of both repos on the VM and rolls back if the new version is
# not healthy within 3 minutes. Run by the deploy workflow over SSH, or by hand.
set -euo pipefail
ROOT=/opt/siliconics
BACK="$ROOT/siliconia_backend" FRONT="$ROOT/siliconia_frontend"
COMPOSE=(docker compose -f "$BACK/deploy/compose/docker-compose.yml" -f "$BACK/deploy/compose/docker-compose.prod.yml" --env-file "$BACK/deploy/compose/.env")
DOMAIN=$(grep '^SITE_ADDRESS=' "$BACK/deploy/compose/.env" | cut -d= -f2)

prev_back=$(git -C "$BACK" rev-parse HEAD)
prev_front=$(git -C "$FRONT" rev-parse HEAD)

healthy() {
  for _ in $(seq 1 36); do
    if curl -fsS "https://$DOMAIN/api/v1/readyz" >/dev/null 2>&1 && curl -fsS -o /dev/null "https://$DOMAIN/"; then return 0; fi
    sleep 5
  done
  return 1
}

up() { APP_VERSION="$(git -C "$BACK" rev-parse --short HEAD)" "${COMPOSE[@]}" up -d --build --remove-orphans; }

echo "==> Updating to main"
git -C "$BACK" fetch -q origin main && git -C "$BACK" checkout -q --detach origin/main
git -C "$FRONT" fetch -q origin main && git -C "$FRONT" checkout -q --detach origin/main
echo "    backend $(git -C "$BACK" rev-parse --short HEAD), web $(git -C "$FRONT" rev-parse --short HEAD)"

if up && healthy; then
  echo "==> Healthy"
  docker image prune -f >/dev/null
  exit 0
fi

echo "!!> Not healthy: rolling back to backend ${prev_back:0:7}, web ${prev_front:0:7}"
git -C "$BACK" checkout -q --detach "$prev_back"
git -C "$FRONT" checkout -q --detach "$prev_front"
up
healthy && echo "==> Rolled back" || echo "!!> Rollback is not healthy either: check '${COMPOSE[*]} logs'"
exit 1
