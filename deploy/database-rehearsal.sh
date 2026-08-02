#!/usr/bin/env bash
set -euo pipefail

: "${PRODUCTION_DUMP:?PRODUCTION_DUMP is required}"
: "${CANDIDATE_DIR:?CANDIDATE_DIR is required}"
: "${PREVIOUS_DIR:?PREVIOUS_DIR is required}"
: "${CANDIDATE_SHA:?CANDIDATE_SHA is required}"
: "${PREVIOUS_SHA:?PREVIOUS_SHA is required}"

case "$CANDIDATE_SHA" in
  (*[!0-9a-f]*|'') printf 'invalid CANDIDATE_SHA\n' >&2; exit 1 ;;
esac
case "$PREVIOUS_SHA" in
  (*[!0-9a-f]*|'') printf 'invalid PREVIOUS_SHA\n' >&2; exit 1 ;;
esac
test "${#CANDIDATE_SHA}" -eq 40
test "${#PREVIOUS_SHA}" -eq 40
test -s "$PRODUCTION_DUMP"

suffix="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}"
network="sub2api-rehearsal-${suffix}"
postgres="sub2api-rehearsal-postgres-${suffix}"
redis="sub2api-rehearsal-redis-${suffix}"
candidate_image="sub2api:rehearsal-${CANDIDATE_SHA}"
previous_image="sub2api:rehearsal-${PREVIOUS_SHA}"
database_password="rehearsal-only-password"
active_app=""

cleanup() {
  if test -n "$active_app"; then
    docker rm -f "$active_app" >/dev/null 2>&1 || true
  fi
  docker rm -f "$postgres" "$redis" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
}
trap cleanup EXIT

build_image() {
  source_dir="$1"
  sha="$2"
  image="$3"
  version="$(tr -d '\r\n' < "$source_dir/backend/cmd/server/VERSION")"
  docker build \
    --build-arg "VERSION=$version" \
    --build-arg "COMMIT=$sha" \
    --build-arg "DATE=$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
    --label "org.opencontainers.image.revision=$sha" \
    -t "$image" "$source_dir"
  revision="$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' "$image")"
  test "$revision" = "$sha"
}

wait_for_postgres() {
  for _ in $(seq 1 60); do
    if docker exec "$postgres" pg_isready -U sub2api -d sub2api >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  printf 'isolated PostgreSQL did not become ready\n' >&2
  return 1
}

database_snapshot() {
  docker exec "$postgres" psql -U sub2api -d sub2api -Atqc \
    "SELECT (SELECT COUNT(*) FROM users) || ',' || (SELECT COUNT(*) FROM api_keys) || ',' || (SELECT COUNT(*) FROM accounts) || ',' || (SELECT COUNT(*) FROM groups);"
}

run_application() {
  image="$1"
  sha="$2"
  phase="$3"
  active_app="sub2api-rehearsal-app-${phase}-${suffix}"
  docker run -d \
    --name "$active_app" \
    --network "$network" \
    --security-opt no-new-privileges:true \
    -e AUTO_SETUP=true \
    -e SERVER_HOST=0.0.0.0 \
    -e SERVER_PORT=8080 \
    -e SERVER_MODE=release \
    -e RUN_MODE=simple \
    -e DATABASE_HOST="$postgres" \
    -e DATABASE_PORT=5432 \
    -e DATABASE_USER=sub2api \
    -e DATABASE_PASSWORD="$database_password" \
    -e DATABASE_DBNAME=sub2api \
    -e DATABASE_SSLMODE=disable \
    -e REDIS_HOST="$redis" \
    -e REDIS_PORT=6379 \
    -e JWT_SECRET=rehearsal-only-jwt-secret-32-bytes-minimum \
    -e TOTP_ENCRYPTION_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
    "$image" >/dev/null

  for _ in $(seq 1 90); do
    if docker exec "$active_app" wget -q -T 5 -O /dev/null http://127.0.0.1:8080/health; then
      build_info="$(docker exec "$active_app" /app/sub2api -version 2>&1)"
      printf '%s\n' "$build_info" | grep -F "commit: ${sha}"
      docker rm -f "$active_app" >/dev/null
      active_app=""
      return 0
    fi
    if test "$(docker inspect --format '{{.State.Running}}' "$active_app" 2>/dev/null || true)" != true; then
      break
    fi
    sleep 2
  done
  docker logs --tail 160 "$active_app" >&2 || true
  return 1
}

build_image "$CANDIDATE_DIR" "$CANDIDATE_SHA" "$candidate_image"
build_image "$PREVIOUS_DIR" "$PREVIOUS_SHA" "$previous_image"

docker network create "$network" >/dev/null
docker run -d \
  --name "$postgres" \
  --network "$network" \
  -e POSTGRES_USER=sub2api \
  -e POSTGRES_PASSWORD="$database_password" \
  -e POSTGRES_DB=sub2api \
  postgres:18-alpine >/dev/null
docker run -d --name "$redis" --network "$network" redis:8-alpine >/dev/null
wait_for_postgres

docker exec -i "$postgres" pg_restore \
  --exit-on-error \
  --clean \
  --if-exists \
  --create \
  --no-owner \
  --no-privileges \
  -U sub2api \
  -d postgres < "$PRODUCTION_DUMP"

before_snapshot="$(database_snapshot)"
before_migrations="$(docker exec "$postgres" psql -U sub2api -d sub2api -Atqc 'SELECT COUNT(*) FROM schema_migrations;')"

run_application "$candidate_image" "$CANDIDATE_SHA" candidate-first
after_upgrade_snapshot="$(database_snapshot)"
test "$after_upgrade_snapshot" = "$before_snapshot"
docker exec "$postgres" psql -U sub2api -d sub2api -v ON_ERROR_STOP=1 -Atqc \
  "SELECT 1 FROM schema_migrations WHERE filename = '191_passkey_credentials.sql';" | grep -qx 1
docker exec "$postgres" psql -U sub2api -d sub2api -v ON_ERROR_STOP=1 -Atqc \
  "SELECT to_regclass('public.passkey_user_handles') IS NOT NULL AND to_regclass('public.passkey_credentials') IS NOT NULL;" | grep -qx t

after_migrations="$(docker exec "$postgres" psql -U sub2api -d sub2api -Atqc 'SELECT COUNT(*) FROM schema_migrations;')"
test "$after_migrations" -gt "$before_migrations"

run_application "$previous_image" "$PREVIOUS_SHA" previous-rollback
test "$(database_snapshot)" = "$before_snapshot"

run_application "$candidate_image" "$CANDIDATE_SHA" candidate-second
test "$(database_snapshot)" = "$before_snapshot"
test "$(docker exec "$postgres" psql -U sub2api -d sub2api -Atqc 'SELECT COUNT(*) FROM pg_index WHERE NOT indisvalid;')" = 0

printf 'database rehearsal passed: previous=%s candidate=%s rows=%s migrations=%s->%s\n' \
  "$PREVIOUS_SHA" "$CANDIDATE_SHA" "$before_snapshot" "$before_migrations" "$after_migrations"
