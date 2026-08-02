#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"

check_application_security_opt() {
  file=$1
  count=$(
    awk '
      $0 == "  sub2api:" {
        in_application = 1
        next
      }
      in_application && $0 ~ /^  [A-Za-z0-9_-]+:$/ {
        in_application = 0
      }
      in_application && $0 == "    security_opt:" {
        in_security_opt = 1
        next
      }
      in_application && in_security_opt && $0 == "      - no-new-privileges:true" {
        count++
      }
      END { print count + 0 }
    ' "$file"
  )

  if [ "$count" -ne 1 ]; then
    printf '%s must enable no-new-privileges exactly once for the sub2api service\n' "$file" >&2
    exit 1
  fi
}

check_async_image_queue_environment() {
  file=$1
  for key in \
    GATEWAY_ASYNC_IMAGE_QUEUE_ENABLED \
    GATEWAY_ASYNC_IMAGE_QUEUE_MAX_PENDING_TASKS \
    GATEWAY_ASYNC_IMAGE_QUEUE_MAX_PENDING_BYTES \
    GATEWAY_ASYNC_IMAGE_QUEUE_WORKER_CEILING \
    GATEWAY_ASYNC_IMAGE_QUEUE_MAX_ATTEMPTS \
    GATEWAY_ASYNC_IMAGE_QUEUE_RETRY_BASE_SECONDS \
    GATEWAY_ASYNC_IMAGE_QUEUE_RETRY_MAX_SECONDS \
    GATEWAY_ASYNC_IMAGE_QUEUE_LEASE_TTL_SECONDS \
    GATEWAY_ASYNC_IMAGE_QUEUE_STALE_AFTER_SECONDS \
    GATEWAY_ASYNC_IMAGE_QUEUE_READY_KEY \
    GATEWAY_ASYNC_IMAGE_QUEUE_DELAYED_KEY \
    GATEWAY_ASYNC_IMAGE_QUEUE_ACTIVE_KEY \
    GATEWAY_ASYNC_IMAGE_QUEUE_PAUSE_KEY \
    GATEWAY_ASYNC_IMAGE_QUEUE_INFLIGHT_KEY_PREFIX \
    GATEWAY_ASYNC_IMAGE_QUEUE_IDEMPOTENCY_KEY_PREFIX
  do
    if ! grep -Fq "      - $key=" "$file"; then
      printf '%s must pass %s into the sub2api container\n' "$file" "$key" >&2
      exit 1
    fi
  done
}

for compose_file in \
  deploy/docker-compose.yml \
  deploy/docker-compose.local.yml \
  deploy/docker-compose.standalone.yml \
  deploy/docker-compose.dev.yml \
  deploy/tanzhongyu/docker-compose.yml
do
  check_application_security_opt "$compose_file"
  check_async_image_queue_environment "$compose_file"
done

if grep -Fq 'ASYNC_IMAGE_MAX_CONCURRENT' \
  deploy/tanzhongyu/docker-compose.yml \
  deploy/tanzhongyu/.env.example; then
  printf 'deprecated ASYNC_IMAGE_MAX_CONCURRENT must not control async admission\n' >&2
  exit 1
fi

if grep -Fq 'upsert_env "$RUNTIME_DIR/.env" "ASYNC_IMAGE_MAX_CONCURRENT"' .github/workflows/deploy-server.yml; then
  printf 'deployment must not configure the deprecated async admission limit\n' >&2
  exit 1
fi

grep -Fq 'Preserving ${pending_jobs} pending image job(s) for recovery after restart' \
  .github/workflows/deploy-server.yml || {
  printf 'deployment must explicitly preserve pending image jobs\n' >&2
  exit 1
}

grep -Fq 'redis-cli SET "$IMAGE_QUEUE_PAUSE_KEY" "$RELEASE_SHA" EX 3600' \
  .github/workflows/deploy-server.yml || {
  printf 'deployment must pause queue dequeue before draining running jobs\n' >&2
  exit 1
}

grep -Fq 'redis-cli DEL "$IMAGE_QUEUE_PAUSE_KEY"' \
  .github/workflows/deploy-server.yml || {
  printf 'deployment must always remove the queue pause key\n' >&2
  exit 1
}

container_healthy_line=$(grep -nF 'test "$container_health" = "healthy"' \
  .github/workflows/deploy-server.yml | tail -n 1 | cut -d: -f1)
final_resume_line=$(grep -nF 'resume_image_queue' \
  .github/workflows/deploy-server.yml | tail -n 1 | cut -d: -f1)
test -n "$container_healthy_line" || {
  printf 'deployment must verify container health\n' >&2
  exit 1
}
test -n "$final_resume_line" || {
  printf 'deployment must resume image dequeue after deploy\n' >&2
  exit 1
}
test "$final_resume_line" -gt "$container_healthy_line" || {
  printf 'deployment must resume image dequeue only after container health passes\n' >&2
  exit 1
}

if grep -Fq '"(pending|running)"' .github/workflows/deploy-server.yml; then
  printf 'deployment must wait for running jobs only, not pending jobs\n' >&2
  exit 1
fi

printf 'docker compose security test passed\n'
