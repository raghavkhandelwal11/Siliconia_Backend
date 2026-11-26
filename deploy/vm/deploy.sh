#!/usr/bin/env bash
# Deploys main on the VM and rolls back if the new version is not healthy
# within 3 minutes. Run by the deploy workflow over SSH, or by hand.
#   SILICONICS_PROFILE=full  (Ampere VM): builds both repos on the server.
#   SILICONICS_PROFILE=lite  (1 GB VM):   pulls the backend image built by the
#                            images workflow; the web app is on Netlify.
set -euo pipefail
ROOT=/opt/siliconics
BACK="$ROOT/siliconia_backend" FRONT="$ROOT/siliconia_frontend"
ENVF="$BACK/deploy/compose/.env"
val() { grep "^$1=" "$ENVF" | tail -1 | cut -d= -f2-; }
PROFILE=$(val SILICONICS_PROFILE); PROFILE=${PROFILE:-full}
DOMAIN=$(val SITE_ADDRESS)
FILES=(-f "$BACK/deploy/compose/docker-compose.yml" -f "$BACK/deploy/compose/docker-compose.$([ "$PROFILE" = lite ] && echo lite || echo prod).yml")
COMPOSE=(docker compose "${FILES[@]}" --env-file "$ENVF")
IMAGE_REPO=ghcr.io/raghavkhandelwal11/siliconia-backend

healthy() {
  for _ in $(seq 1 36); do
    curl -fsS "https://$DOMAIN/api/v1/readyz" >/dev/null 2>&1 && return 0
    sleep 5
  done
  return 1
}

prev_back=$(git -C "$BACK" rev-parse HEAD)
prev_front=""
[ "$PROFILE" = full ] && prev_front=$(git -C "$FRONT" rev-parse HEAD)

echo "==> Updating to main ($PROFILE)"
git -C "$BACK" fetch -q origin main && git -C "$BACK" checkout -q --detach origin/main
[ "$PROFILE" = full ] && git -C "$FRONT" fetch -q origin main && git -C "$FRONT" checkout -q --detach origin/main

up() { # $1 = backend commit to run
  if [ "$PROFILE" = lite ]; then
    export BACKEND_IMAGE="$IMAGE_REPO:$1"
    "${COMPOSE[@]}" pull -q && "${COMPOSE[@]}" up -d --no-build --remove-orphans
  else
    APP_VERSION="${1:0:7}" "${COMPOSE[@]}" up -d --build --remove-orphans
  fi
}

new_back=$(git -C "$BACK" rev-parse HEAD)
echo "    backend ${new_back:0:7}"
if up "$new_back" && healthy; then
  echo "==> Healthy"
  docker image prune -f >/dev/null
  exit 0
fi

echo "!!> Not healthy: rolling back to backend ${prev_back:0:7}"
git -C "$BACK" checkout -q --detach "$prev_back"
[ -n "$prev_front" ] && git -C "$FRONT" checkout -q --detach "$prev_front"
up "$prev_back" || true
healthy && echo "==> Rolled back" || echo "!!> Rollback is not healthy either: check '${COMPOSE[*]} logs'"
exit 1
