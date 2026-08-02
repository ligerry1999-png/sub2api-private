# Tanzhongyu Sub2API Deployment

This directory contains the private deployment layer for Sub2API.

## Production rules

- GitHub Actions builds the Docker image. The production server must never run
  `docker build` or `docker compose build`.
- The `Deploy Server` workflow defaults to `deploy=false`, which only verifies
  that the image can be built.
- A production deployment requires an explicit `deploy=true` run after CI,
  security scans, database rehearsal, and a running-image-job drain check have
  passed. The workflow temporarily pauses queue dequeue while continuing to
  accept pending jobs, so durable pending work does not block a deployment.
- The server only receives the release files and prebuilt image, runs
  `docker load`, then restarts the Sub2API container.
- Application files, PostgreSQL, and Redis use fixed paths under
  `/opt/sub2api-src/deploy/tanzhongyu`; replacing a release must not replace
  persistent data.
- Keep the verified PostgreSQL dump and previous image until the upgraded
  version has passed real health, Codex, and image-generation checks.

## Private behavior that deployment must preserve

- Image logs and thumbnails: 7-day retention.
- Asynchronous `/v1/image-jobs/*` terminal results: 2-day retention. Pending,
  running, leased, and unknown-state directories are never removed by the host
  cleanup timer.
- Asynchronous image admission uses a durable Redis-backed queue with capacity
  limits of 2,000 tasks and 5 GiB of stored request bodies. The 64 worker
  ceiling is a process safety bound; actual upstream concurrency still follows
  each account's configured concurrency slots.
- ChatGPT2API image copies: 3-day retention.
- Nginx image uploads: 100 MB request limit and 600-second request-body timeout.
- Production image worker bridge remains disabled by default; native OAuth
  image generation stays available as the fallback path.

## Manual workflow

1. Run `Deploy Server` with `deploy=false`.
2. Confirm the normal CI and security scan are green for the same commit.
3. Rehearse database migration and old-image rollback against a restored copy
   of the production PostgreSQL dump.
4. Confirm there are no running image jobs. Pending jobs stay queued and resume
   after the new container is healthy.
5. Only then run the same commit with `deploy=true`.

The workflow must stop before restart if it cannot create and verify a real
PostgreSQL backup.
