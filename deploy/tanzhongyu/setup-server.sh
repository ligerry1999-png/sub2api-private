#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

mkdir -p data postgres_data redis_data

if [ ! -f .env ]; then
  cp .env.example .env
  POSTGRES_PASSWORD="$(openssl rand -hex 24)"
  JWT_SECRET="$(openssl rand -hex 32)"
  TOTP_ENCRYPTION_KEY="$(openssl rand -hex 32)"
  ADMIN_PASSWORD="$(openssl rand -base64 18 | tr -d '=+/' | cut -c1-20)"

  sed -i "s/replace-with-generated-password/${POSTGRES_PASSWORD}/" .env
  sed -i "s/replace-with-generated-jwt-secret/${JWT_SECRET}/" .env
  sed -i "s/replace-with-generated-totp-key/${TOTP_ENCRYPTION_KEY}/" .env
  sed -i "s/replace-with-generated-admin-password/${ADMIN_PASSWORD}/" .env
fi

docker compose up -d

echo "Sub2API deployed locally on server: http://127.0.0.1:18080"
echo "Admin email: $(grep '^ADMIN_EMAIL=' .env | cut -d= -f2-)"
echo "Admin password: $(grep '^ADMIN_PASSWORD=' .env | cut -d= -f2-)"

