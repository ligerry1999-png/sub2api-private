#!/bin/sh
set -eu

RUNTIME_DIR="${RUNTIME_DIR:-/opt/sub2api-src/deploy/tanzhongyu}"
ENV_FILE="${ENV_FILE:-$RUNTIME_DIR/.env}"
CHATGPT2API_IMAGE_DIR="${CHATGPT2API_IMAGE_DIR:-/opt/chatgpt2api/data/images}"

log() {
  printf '%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"
}

read_env_value() {
  key="$1"
  fallback="$2"
  if [ -f "$ENV_FILE" ]; then
    value="$(grep -E "^${key}=" "$ENV_FILE" | tail -n 1 | cut -d= -f2- || true)"
    if [ -n "${value:-}" ]; then
      printf '%s' "$value"
      return
    fi
  fi
  printf '%s' "$fallback"
}

positive_int_or_default() {
  value="$1"
  fallback="$2"
  case "$value" in
    ''|*[!0-9]*) printf '%s' "$fallback" ;;
    0) printf '%s' "$fallback" ;;
    *) printf '%s' "$value" ;;
  esac
}

resolve_path() {
  value="$1"
  base="$2"
  case "$value" in
    /*) printf '%s' "$value" ;;
    ./*) printf '%s/%s' "$base" "${value#./}" ;;
    *) printf '%s/%s' "$base" "$value" ;;
  esac
}

delete_old_files() {
  dir="$1"
  days="$2"
  label="$3"
  minutes=$((days * 1440))
  if [ ! -d "$dir" ]; then
    log "$label skipped: missing dir=$dir"
    return
  fi
  count="$(find "$dir" -type f -mmin +"$minutes" 2>/dev/null | wc -l | tr -d ' ')"
  log "$label deleting files: dir=$dir retention_days=$days count=$count"
  find "$dir" -type f -mmin +"$minutes" -print0 2>/dev/null | xargs -0 -r rm -f --
  find "$dir" -mindepth 1 -type d -empty -delete 2>/dev/null || true
}

delete_old_dirs() {
  dir="$1"
  days="$2"
  label="$3"
  minutes=$((days * 1440))
  if [ ! -d "$dir" ]; then
    log "$label skipped: missing dir=$dir"
    return
  fi
  count="$(find "$dir" -mindepth 1 -maxdepth 1 -type d -mmin +"$minutes" 2>/dev/null | wc -l | tr -d ' ')"
  log "$label deleting dirs: dir=$dir retention_days=$days count=$count"
  find "$dir" -mindepth 1 -maxdepth 1 -type d -mmin +"$minutes" -print0 2>/dev/null | xargs -0 -r rm -rf --
}

sub2api_data_dir="$(read_env_value SUB2API_DATA_DIR "$RUNTIME_DIR/data")"
sub2api_data_dir="$(resolve_path "$sub2api_data_dir" "$RUNTIME_DIR")"
image_jobs_retention="$(positive_int_or_default "$(read_env_value IMAGE_JOBS_RETENTION_DAYS 2)" 2)"
chatgpt2api_retention="$(positive_int_or_default "$(read_env_value CHATGPT2API_IMAGES_RETENTION_DAYS 3)" 3)"

delete_old_dirs "$sub2api_data_dir/image_jobs" "$image_jobs_retention" "sub2api_image_jobs"
delete_old_files "$CHATGPT2API_IMAGE_DIR" "$chatgpt2api_retention" "chatgpt2api_images"

log "storage cleanup finished"
