#!/bin/sh
set -eu

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
tmp_root="$(mktemp -d "${TMPDIR:-/tmp}/sub2api-storage-cleanup-test.XXXXXX")"
trap 'rm -rf "$tmp_root"' EXIT HUP INT TERM

runtime_dir="$tmp_root/runtime"
image_jobs_dir="$runtime_dir/data/image_jobs"
chat_images_dir="$tmp_root/chatgpt2api/images"
mkdir -p "$image_jobs_dir/old-job" "$image_jobs_dir/new-job" "$chat_images_dir"

printf 'old\n' > "$image_jobs_dir/old-job/result.png"
printf 'new\n' > "$image_jobs_dir/new-job/result.png"
printf 'old\n' > "$chat_images_dir/old.png"
printf 'new\n' > "$chat_images_dir/new.png"
touch -t 202001010000 "$image_jobs_dir/old-job/result.png" "$image_jobs_dir/old-job" "$chat_images_dir/old.png"

printf '%s\n' \
  "SUB2API_DATA_DIR=$runtime_dir/data" \
  "IMAGE_JOBS_RETENTION_DAYS=2" \
  "CHATGPT2API_IMAGES_RETENTION_DAYS=3" \
  > "$runtime_dir/.env"

RUNTIME_DIR="$runtime_dir" \
ENV_FILE="$runtime_dir/.env" \
CHATGPT2API_IMAGE_DIR="$chat_images_dir" \
  "$repo_root/deploy/tanzhongyu/storage-cleanup.sh"

test ! -e "$image_jobs_dir/old-job"
test -e "$image_jobs_dir/new-job/result.png"
test ! -e "$chat_images_dir/old.png"
test -e "$chat_images_dir/new.png"

if RUNTIME_DIR="$runtime_dir" \
  ENV_FILE="$runtime_dir/.env" \
  CHATGPT2API_IMAGE_DIR="$tmp_root/chatgpt2api" \
  "$repo_root/deploy/tanzhongyu/storage-cleanup.sh" >/dev/null 2>&1; then
  echo "storage cleanup accepted an unsafe target" >&2
  exit 1
fi

echo "storage cleanup tests passed"
