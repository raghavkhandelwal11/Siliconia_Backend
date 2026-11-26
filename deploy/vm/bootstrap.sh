#!/usr/bin/env bash
# One-time setup of the Oracle Cloud Always Free VM (Ubuntu 24.04, ARM).
#   curl -fsSL https://raw.githubusercontent.com/raghavkhandelwal11/Siliconia_Backend/main/deploy/vm/bootstrap.sh | sudo bash -s -- <name>.duckdns.org
# Installs Docker, opens ports 80/443 in the VM firewall, clones both repos to
# /opt/siliconics, writes a production .env with fresh random secrets, installs
# the nightly backup, then starts everything. Safe to run again.
set -euo pipefail
DOMAIN="${1:?usage: bootstrap.sh <name>.duckdns.org}"
ROOT=/opt/siliconics
OWNER="${SUDO_USER:-ubuntu}"

echo "==> Docker"
if ! command -v docker >/dev/null; then
  curl -fsSL https://get.docker.com | sh
fi
usermod -aG docker "$OWNER" || true
apt-get install -y -qq git make openssl >/dev/null

echo "==> Firewall (Oracle images block everything but SSH by default)"
for port in 80 443; do
  iptables -C INPUT -p tcp --dport "$port" -j ACCEPT 2>/dev/null || iptables -I INPUT 5 -p tcp --dport "$port" -j ACCEPT
done
command -v netfilter-persistent >/dev/null && netfilter-persistent save >/dev/null || true

echo "==> Code"
mkdir -p "$ROOT" && chown "$OWNER" "$ROOT"
for repo in Siliconia_Backend:siliconia_backend Siliconia-Frontend:siliconia_frontend; do
  name="${repo%%:*}" dir="$ROOT/${repo##*:}"
  [ -d "$dir/.git" ] || sudo -u "$OWNER" git clone -q "https://github.com/raghavkhandelwal11/$name.git" "$dir"
done

echo "==> Configuration"
ENV="$ROOT/siliconia_backend/deploy/compose/.env"
if [ ! -f "$ENV" ]; then
  cp "$ROOT/siliconia_backend/deploy/compose/.env.example" "$ENV"
  rnd() { openssl rand -hex "${1:-24}"; }
  b64() { openssl rand -base64 32; }
  dbpw=$(rnd 16)
  set_env() { if grep -q "^$1=" "$ENV"; then sed -i "s|^$1=.*|$1=$2|" "$ENV"; else echo "$1=$2" >> "$ENV"; fi; }
  set_env APP_MODE prototype
  set_env POSTGRES_PASSWORD "$dbpw"
  set_env DATABASE_URL "postgres://siliconia:$dbpw@postgres:5432/siliconia?sslmode=disable"
  set_env AUTH_SIGNING_KEY "$(b64)"
  set_env OTP_PEPPER "$(b64)"
  set_env ENCRYPTION_MASTER_KEY "$(b64)"
  set_env INTERNAL_API_KEY "$(rnd 24)"
  set_env COOKIE_SECURE true
  set_env PUBLIC_BASE_URL "https://$DOMAIN"
  set_env SIM_PAY_PUBLIC_URL "https://$DOMAIN"
  set_env ALLOWED_ORIGINS "https://$DOMAIN"
  set_env SITE_ADDRESS "$DOMAIN"
  for k in SIM_PAY_API_KEY SIM_PAY_WEBHOOK_SECRET SIM_SHIP_API_KEY SIM_SHIP_WEBHOOK_SECRET SIM_PAYOUT_API_KEY SIM_PAYOUT_WEBHOOK_SECRET STORAGE_SECRET_KEY TEST_INBOX_PASSWORD; do
    set_env "$k" "$(rnd 16)"
  done
  chown "$OWNER" "$ENV" && chmod 600 "$ENV"
  echo "    wrote $ENV (secrets are only on this server)"
fi

echo "==> Nightly backup and DuckDNS refresh"
install -m 755 "$ROOT/siliconia_backend/deploy/vm/backup.sh" /usr/local/bin/siliconics-backup
echo "15 21 * * * $OWNER /usr/local/bin/siliconics-backup >> /var/log/siliconics-backup.log 2>&1" > /etc/cron.d/siliconics-backup
if [ -n "${DUCKDNS_TOKEN:-}" ]; then
  sub="${DOMAIN%%.duckdns.org}"
  echo "*/10 * * * * root curl -fsS 'https://www.duckdns.org/update?domains=$sub&token=$DUCKDNS_TOKEN' >/dev/null" > /etc/cron.d/duckdns
  chmod 600 /etc/cron.d/duckdns
fi

echo "==> Start"
sudo -u "$OWNER" -H bash "$ROOT/siliconia_backend/deploy/vm/deploy.sh"
echo
echo "Done. Open https://$DOMAIN (the certificate can take a minute on first start)."
echo "Test inbox password: grep TEST_INBOX_PASSWORD $ENV"
echo "Create an admin:     cd $ROOT/siliconia_backend && make admin PHONE=<10 digits> NAME=\"<name>\""
