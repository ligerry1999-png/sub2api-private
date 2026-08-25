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
migration_220_preexisting=false

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

assert_equal() {
  label="$1"
  expected="$2"
  actual="$3"
  if test "$actual" != "$expected"; then
    printf '%s mismatch: expected=%s actual=%s\n' "$label" "$expected" "$actual" >&2
    return 1
  fi
}

assert_query_equal() {
  label="$1"
  expected="$2"
  query="$3"
  actual="$(docker exec "$postgres" psql -U sub2api -d sub2api -v ON_ERROR_STOP=1 -Atqc "$query")"
  assert_equal "$label" "$expected" "$actual"
}

database_snapshot() {
  docker exec "$postgres" psql -U sub2api -d sub2api -Atqc \
    "SELECT (SELECT COUNT(*) FROM users) || ',' ||
            (SELECT COUNT(*) FROM api_keys) || ',' ||
            (SELECT COUNT(*) FROM accounts) || ',' ||
            (SELECT COUNT(*) FROM groups) || ',' ||
            (SELECT COUNT(*) FROM usage_logs) || ',' ||
            (SELECT COUNT(*) FROM image_logs);"
}

private_config_snapshot() {
  # Hash sensitive account credentials and the private group routing/config JSON
  # inside PostgreSQL. Only the digest leaves the isolated database container.
  docker exec "$postgres" psql -U sub2api -d sub2api -Atqc \
    "SELECT md5(
       COALESCE((
         SELECT string_agg(
           id::text || ':' || COALESCE(credentials::text, 'null') || ':' || COALESCE(extra::text, 'null'),
           E'\\n' ORDER BY id
         )
         FROM accounts
       ), '') || E'\\n--groups--\\n' || COALESCE((
         SELECT string_agg(
           id::text || ':' || platform || ':' ||
           COALESCE(models_list_config::text, 'null') || ':' ||
           COALESCE(reasoning_effort_mappings::text, 'null') || ':' ||
           COALESCE(model_routing::text, 'null') || ':' ||
           COALESCE(messages_dispatch_model_config::text, 'null'),
           E'\\n' ORDER BY id
         )
         FROM groups
       ), '')
     );"
}

video_price_snapshot() {
  scope="$1"
  table_name="$2"
  id_column="$3"
  model_prices_expression="${4:-video_model_prices}"
  case "$scope" in
    non-grok)
      platform_predicate="platform IS DISTINCT FROM 'grok' AND platform IS DISTINCT FROM 'composite'"
      ;;
    grok)
      platform_predicate="platform IN ('grok', 'composite')"
      ;;
    *)
      printf 'invalid video price snapshot scope: %s\n' "$scope" >&2
      return 1
      ;;
  esac
  docker exec "$postgres" psql -U sub2api -d sub2api -Atqc \
    "SELECT COALESCE(jsonb_agg(jsonb_build_array(${id_column}, platform, video_price_480p, video_price_720p, video_price_1080p, ${model_prices_expression}) ORDER BY ${id_column})::text, '[]') FROM ${table_name} WHERE ${platform_predicate} AND (video_price_480p IS NOT NULL OR video_price_720p IS NOT NULL OR video_price_1080p IS NOT NULL OR ${model_prices_expression} IS NOT NULL);"
}

prepare_migration_220_rehearsal() {
  # A production dump from v0.1.173 has already applied migration 220. In that
  # case preserve and compare its existing backup table instead of inserting a
  # canary that the already-recorded migration can never see. Older dumps still
  # exercise the original backup-and-clear path with an isolated canary.
  if test "$(docker exec "$postgres" psql -U sub2api -d sub2api -Atqc \
    "SELECT COUNT(*) FROM schema_migrations WHERE filename = '220_clear_non_grok_video_generation_config.sql';")" -eq 1; then
    migration_220_preexisting=true
    assert_query_equal "preexisting migration 220 backup table" t \
      "SELECT to_regclass('public.groups_video_price_backup_220') IS NOT NULL;"
    return 0
  fi

  migration_220_preexisting=false
  docker exec "$postgres" psql -U sub2api -d sub2api -v ON_ERROR_STOP=1 -Atqc \
    "DROP TABLE IF EXISTS groups_video_price_backup_220;"
  docker exec "$postgres" psql -U sub2api -d sub2api -v ON_ERROR_STOP=1 -Atqc \
    "UPDATE groups
       SET video_price_480p = 0.987654
     WHERE id = (
       SELECT id FROM groups
       WHERE platform IS DISTINCT FROM 'grok'
         AND platform IS DISTINCT FROM 'composite'
       ORDER BY id
       LIMIT 1
     )
       AND video_price_480p IS NULL
       AND video_price_720p IS NULL
       AND video_price_1080p IS NULL;"
  test "$(docker exec "$postgres" psql -U sub2api -d sub2api -Atqc \
    "SELECT COUNT(*) FROM groups WHERE platform IS DISTINCT FROM 'grok' AND platform IS DISTINCT FROM 'composite' AND (video_price_480p IS NOT NULL OR video_price_720p IS NOT NULL OR video_price_1080p IS NOT NULL);")" -gt 0
}

restore_production_dump() {
  docker exec -i "$postgres" pg_restore \
    --exit-on-error \
    --clean \
    --if-exists \
    --create \
    --no-owner \
    --no-privileges \
    -U sub2api \
    -d postgres < "$PRODUCTION_DUMP"
}

validate_v0183_migrations() {
  expected_migrations="$(docker exec "$postgres" psql -U sub2api -d sub2api -Atqc \
    "SELECT COUNT(*) FROM schema_migrations WHERE filename IN (
      '192_group_profit_control.sql',
      '193_group_profit_control_auth_cache_invalidation.sql',
      '194_add_usage_log_upstream_response_model.sql',
      '195_add_usage_log_upstream_model_mismatch_index_notx.sql',
      '194_channel_monitor_v2.sql',
      '195_channel_monitor_mode.sql',
      '196_channel_monitor_v2_ignored_error_categories.sql',
      '197_channel_monitor_v2_seed_popular_models.sql',
      '198_channel_monitor_v2_health_thresholds.sql',
      '199_channel_monitor_v2_fixed_rollups.sql',
      '200_channel_monitor_v2_rollup_permissions.sql',
      '201_channel_monitor_v2_refresh_5m.sql',
      '202_channel_monitor_v2_full_table_permissions.sql',
      '203_channel_monitor_v2_default_ignore_and_cache.sql',
      '204_channel_monitor_hide_throughput.sql',
      '205_channel_monitor_v2_reset_factory_cache_thresholds.sql',
      '206_channel_monitor_v2_privacy_defaults.sql',
      '217_group_video_model_prices.sql',
      '218_group_audio_voice_pricing.sql',
      '219_group_search_price_per_1k.sql',
      '220_clear_non_grok_video_generation_config.sql',
      '221_group_model_pricing.sql',
      '222_group_usage_daily_rollups.sql',
      '223_group_usage_rollup_timezone.sql',
      '224_user_platform_quotas_add_cn_providers.sql',
      '225_backfill_codex_fingerprint_seed.sql',
      '225_channel_model_time_pricing.sql',
      '226_add_usage_log_effective_model_indexes_notx.sql',
      '226_channel_monitor_quota_mode.sql',
      '227_composite_routes_add_cn_providers.sql',
      '228_channel_pricing_multipliers.sql',
      '229_plugins.sql',
      '230_plugin_artifacts.sql'
    );")"
  assert_equal "v0.1.183 migration count" 33 "$expected_migrations"
  assert_query_equal "migration 220 backup table" t \
    "SELECT to_regclass('public.groups_video_price_backup_220') IS NOT NULL;"
  assert_equal "migration 220 non-Grok backup" "$before_non_grok_video_prices" \
    "$(video_price_snapshot non-grok groups_video_price_backup_220 group_id)"
  assert_equal "migration 220 Grok price preservation" "$before_grok_video_prices" \
    "$(video_price_snapshot grok groups id)"
  assert_query_equal "migration 220 non-Grok price clearing" 0 \
    "SELECT COUNT(*) FROM groups WHERE platform IS DISTINCT FROM 'grok' AND platform IS DISTINCT FROM 'composite' AND (video_price_480p IS NOT NULL OR video_price_720p IS NOT NULL OR video_price_1080p IS NOT NULL OR video_model_prices IS NOT NULL);"
  assert_query_equal "migration 221 group pricing columns" t \
    "SELECT COUNT(*) = 2
       AND bool_and(
         CASE
           WHEN column_name = 'long_context_pricing_enabled'
             THEN is_nullable = 'NO' AND column_default = 'true'
           WHEN column_name = 'model_pricing'
             THEN is_nullable = 'YES' AND column_default IS NULL
           ELSE FALSE
         END
       )
     FROM information_schema.columns
     WHERE table_schema = 'public'
       AND table_name = 'groups'
       AND column_name IN ('long_context_pricing_enabled', 'model_pricing');"
  assert_query_equal "migration 221 long-context backfill" 0 \
    "SELECT COUNT(*) FROM groups WHERE long_context_pricing_enabled IS DISTINCT FROM TRUE;"
  assert_query_equal "migration 221 model pricing initialization" 0 \
    "SELECT COUNT(*) FROM groups WHERE model_pricing IS NOT NULL;"
  assert_query_equal "migrations 222-223 group usage rollups" t \
    "SELECT to_regclass('public.usage_group_daily_rollups') IS NOT NULL
       AND to_regclass('public.usage_group_rollup_state') IS NOT NULL
       AND EXISTS (
         SELECT 1 FROM information_schema.columns
         WHERE table_schema = 'public'
           AND table_name = 'usage_group_rollup_state'
           AND column_name = 'timezone_name'
       );"
  assert_query_equal "migration 225 channel time pricing" 1 \
    "SELECT COUNT(*) FROM information_schema.columns
     WHERE table_schema = 'public'
       AND table_name = 'channel_model_pricing'
       AND column_name = 'time_pricing';"
  assert_query_equal "migration 226 channel monitor quota mode" 3 \
    "SELECT COUNT(*) FROM information_schema.columns
     WHERE table_schema = 'public'
       AND ((table_name = 'channel_monitors' AND column_name IN ('check_mode', 'account_id'))
         OR (table_name = 'channel_monitor_histories' AND column_name = 'quota'));"
  assert_query_equal "migration 228 pricing multipliers" 6 \
    "SELECT COUNT(*) FROM information_schema.columns
     WHERE table_schema = 'public'
       AND ((table_name = 'channel_model_pricing' AND column_name IN ('fast_multiplier', 'flex_multiplier'))
         OR (table_name = 'channel_pricing_intervals' AND column_name IN ('input_multiplier', 'output_multiplier', 'cache_write_multiplier', 'cache_read_multiplier')));"
  assert_query_equal "migrations 229-230 plugin storage" t \
    "SELECT to_regclass('public.sub2api_plugin_installations') IS NOT NULL
       AND to_regclass('public.sub2api_plugin_bindings') IS NOT NULL
       AND EXISTS (
         SELECT 1 FROM information_schema.columns
         WHERE table_schema = 'public'
           AND table_name = 'sub2api_plugin_installations'
           AND column_name = 'artifact_data'
       );"
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
      # ImageLogService is a required Wire dependency: the candidate refuses
      # to initialize when it is missing. Reaching /health therefore proves
      # the startup contract without depending on a fixed log output format.
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

docker network create --internal "$network" >/dev/null
docker run -d \
  --name "$postgres" \
  --network "$network" \
  -e POSTGRES_USER=sub2api \
  -e POSTGRES_PASSWORD="$database_password" \
  -e POSTGRES_DB=sub2api \
  postgres:18-alpine >/dev/null
docker run -d --name "$redis" --network "$network" redis:8-alpine >/dev/null
wait_for_postgres

restore_production_dump
prepare_migration_220_rehearsal

before_snapshot="$(database_snapshot)"
before_private_config_snapshot="$(private_config_snapshot)"
before_migrations="$(docker exec "$postgres" psql -U sub2api -d sub2api -Atqc 'SELECT COUNT(*) FROM schema_migrations;')"
if test "$migration_220_preexisting" = true; then
  before_non_grok_video_prices="$(video_price_snapshot non-grok groups_video_price_backup_220 group_id)"
else
  before_non_grok_video_prices="$(video_price_snapshot non-grok groups id "NULL::jsonb")"
fi
before_grok_video_prices="$(video_price_snapshot grok groups id "NULL::jsonb")"

run_application "$candidate_image" "$CANDIDATE_SHA" candidate-first
after_upgrade_snapshot="$(database_snapshot)"
assert_equal "candidate-first row snapshot" "$before_snapshot" "$after_upgrade_snapshot"
assert_equal "candidate-first private config digest" "$before_private_config_snapshot" "$(private_config_snapshot)"
assert_query_equal "migration 191 record" 1 \
  "SELECT COUNT(*) FROM schema_migrations WHERE filename = '191_passkey_credentials.sql';"
assert_query_equal "migration 191 passkey tables" t \
  "SELECT to_regclass('public.passkey_user_handles') IS NOT NULL AND to_regclass('public.passkey_credentials') IS NOT NULL;"
validate_v0183_migrations

after_migrations="$(docker exec "$postgres" psql -U sub2api -d sub2api -Atqc 'SELECT COUNT(*) FROM schema_migrations;')"
if git -C "$CANDIDATE_DIR" diff --quiet "$PREVIOUS_SHA" "$CANDIDATE_SHA" -- backend/migrations; then
  test "$after_migrations" -eq "$before_migrations"
else
  test "$after_migrations" -gt "$before_migrations"
fi

run_application "$previous_image" "$PREVIOUS_SHA" previous-rollback
test "$(database_snapshot)" = "$before_snapshot"
test "$(private_config_snapshot)" = "$before_private_config_snapshot"

# A real database rollback means restoring the pre-upgrade dump, not only
# proving that the old binary can tolerate the forward-migrated schema.
restore_production_dump
prepare_migration_220_rehearsal
test "$(database_snapshot)" = "$before_snapshot"
test "$(docker exec "$postgres" psql -U sub2api -d sub2api -Atqc 'SELECT COUNT(*) FROM schema_migrations;')" -eq "$before_migrations"
test "$(docker exec "$postgres" psql -U sub2api -d sub2api -Atqc \
  "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'groups' AND column_name IN ('long_context_pricing_enabled', 'model_pricing');")" -eq 0
test "$(docker exec "$postgres" psql -U sub2api -d sub2api -Atqc \
  "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('usage_group_daily_rollups', 'usage_group_rollup_state', 'sub2api_plugin_installations', 'sub2api_plugin_bindings');")" -eq 0
test "$(private_config_snapshot)" = "$before_private_config_snapshot"
run_application "$previous_image" "$PREVIOUS_SHA" previous-restored
test "$(database_snapshot)" = "$before_snapshot"
test "$(private_config_snapshot)" = "$before_private_config_snapshot"

run_application "$candidate_image" "$CANDIDATE_SHA" candidate-second
test "$(database_snapshot)" = "$before_snapshot"
test "$(private_config_snapshot)" = "$before_private_config_snapshot"
validate_v0183_migrations
test "$(docker exec "$postgres" psql -U sub2api -d sub2api -Atqc 'SELECT COUNT(*) FROM pg_index WHERE NOT indisvalid;')" = 0

printf 'database rehearsal passed: previous=%s candidate=%s rows=%s migrations=%s->%s\n' \
  "$PREVIOUS_SHA" "$CANDIDATE_SHA" "$before_snapshot" "$before_migrations" "$after_migrations"
