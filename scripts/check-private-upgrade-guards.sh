#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

fail() {
  printf 'private upgrade guard failed: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  file="$1"
  expected="$2"
  grep -Fq -- "$expected" "$file" || fail "$file is missing: $expected"
}

assert_not_contains() {
  file="$1"
  forbidden="$2"
  if grep -Fq -- "$forbidden" "$file"; then
    fail "$file must not contain: $forbidden"
  fi
}

workflow=".github/workflows/deploy-server.yml"
compose="deploy/tanzhongyu/docker-compose.yml"
env_example="deploy/tanzhongyu/.env.example"
gateway_routes="backend/internal/server/routes/gateway.go"

assert_contains "$workflow" "workflow_dispatch:"
assert_contains "$workflow" "migration_rehearsal_sha:"
assert_contains "$workflow" 'if: ${{ inputs.deploy == true }}'
assert_contains "$workflow" 'test "$GITHUB_REF" = "refs/heads/main"'
assert_contains "$workflow" "pg_dump --format=custom"
assert_contains "$workflow" "pg_restore --list"
assert_contains "$workflow" "docker load -i /tmp/sub2api-image.tgz"
assert_contains "$workflow" "up -d --no-build sub2api"
assert_contains "$workflow" 'container_health="$(docker inspect --format'
assert_contains "$workflow" 'grep -F "commit: ${RELEASE_SHA}"'
assert_contains "$workflow" "printf '%s\\n' \"\$GITHUB_SHA\" > .release-commit"
assert_contains "$workflow" 'org.opencontainers.image.revision'
assert_contains "$workflow" 'probe_custom_route POST /v1/image-jobs/images/generations 401'
assert_not_contains "$workflow" "  push:"
assert_not_contains "$workflow" "docker-compose.build.yml"
assert_not_contains "$workflow" "docker compose build"

assert_contains "$compose" '${SUB2API_DATA_DIR:-./data}:/app/data'
assert_contains "$compose" '${SUB2API_POSTGRES_DATA_DIR:-./postgres_data}:/var/lib/postgresql/data'
assert_contains "$compose" '${SUB2API_REDIS_DATA_DIR:-./redis_data}:/data'
assert_contains "$compose" 'IMAGE_LOGS_RETENTION_DAYS=${IMAGE_LOGS_RETENTION_DAYS:-7}'
assert_contains "$compose" 'IMAGE_JOBS_RETENTION_DAYS=${IMAGE_JOBS_RETENTION_DAYS:-2}'
assert_contains "$env_example" "IMAGE_LOGS_RETENTION_DAYS=7"
assert_contains "$env_example" "IMAGE_JOBS_RETENTION_DAYS=2"
assert_contains "$env_example" "CHATGPT2API_IMAGES_RETENTION_DAYS=3"
assert_contains "deploy/tanzhongyu/storage-cleanup.sh" "require_safe_target"
test -f deploy/tests/storage-cleanup-test.sh || fail "storage cleanup test is missing"

assert_contains "deploy/tanzhongyu/nginx-sub2api-prod.conf" "client_max_body_size 100m;"
assert_contains "deploy/tanzhongyu/nginx-sub2api-prod.conf" "client_body_timeout 600s;"
assert_contains "deploy/tanzhongyu/nginx-sub2api-prod.conf" "proxy_request_buffering on;"

assert_contains "$gateway_routes" 'POST("/image-jobs/images/generations"'
assert_contains "$gateway_routes" 'POST("/image-jobs/images/edits"'
assert_contains "$gateway_routes" 'GET("/image-jobs/:job_id/result"'
assert_contains "$gateway_routes" 'POST("/image-jobs/:job_id/cancel"'
assert_contains "backend/internal/server/routes/prompt_audit_route_coverage_test.go" '"/image-jobs/images/generations"'
assert_contains "backend/internal/server/routes/prompt_audit_route_coverage_test.go" '"/image-jobs/images/edits"'

test -f backend/internal/service/openai_responses_item_id.go || \
  fail "v0.1.165 Responses item-ID protection is missing"
test -f backend/internal/service/openai_responses_namespace.go || \
  fail "v0.1.165 Responses namespace protection is missing"
test -f backend/internal/service/image_storage.go || \
  fail "v0.1.165 image-storage base is missing"
test -f backend/internal/service/upstream_path_guard.go || \
  fail "v0.1.169 upstream path guard is missing"
test -f backend/migrations/190_add_users_email_alias_dedup_index_notx.sql || \
  fail "v0.1.165 migrations are incomplete"

assert_contains "$gateway_routes" 'guardResponsesSubpath := func(next gin.HandlerFunc) gin.HandlerFunc'
assert_contains "$gateway_routes" 'POST("/responses/*subpath", guardResponsesSubpath('
assert_contains "$gateway_routes" 'r.POST("/responses/*subpath", bodyLimit, clientRequestID, opsErrorLogger, endpointNorm, gin.HandlerFunc(apiKeyAuth), compositeTarget, requireGroupAnthropic, guardResponsesSubpath(responsesHandler))'
assert_contains "$gateway_routes" 'codexDirect.POST("/responses/*subpath", guardResponsesSubpath(responsesHandler))'
assert_contains "$compose" 'no-new-privileges:true'
assert_contains "Dockerfile" 'LABEL org.opencontainers.image.revision="${COMMIT}"'
test -f .github/workflows/database-rehearsal.yml || fail "database rehearsal workflow is missing"
test -f deploy/database-rehearsal.sh || fail "database rehearsal script is missing"

assert_contains "backend/go.mod" "golang.org/x/image v0.43.0"
assert_contains "backend/go.mod" "golang.org/x/text v0.39.0"

expected_migration_sha="b98780ae77a0f90efeda8be70645cb3997112f7bc3a9e1938a16fdd7810d902a"
if command -v sha256sum >/dev/null 2>&1; then
  actual_migration_sha="$(sha256sum backend/migrations/136_image_logs.sql | awk '{print $1}')"
else
  actual_migration_sha="$(shasum -a 256 backend/migrations/136_image_logs.sql | awk '{print $1}')"
fi
test "$actual_migration_sha" = "$expected_migration_sha" || \
  fail "backend/migrations/136_image_logs.sql checksum changed"

test "$(tr -d '\r\n' < backend/cmd/server/VERSION)" = "0.1.169-private.3" || \
  fail "private version marker changed unexpectedly"

printf 'private upgrade guards passed\n'
