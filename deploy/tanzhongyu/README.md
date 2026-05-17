# Tanzhongyu Sub2API Deployment

This directory is the private deployment layer for the Tanzhongyu Sub2API test instance.

## Current Strategy

- Default runtime uses the upstream Docker image: `weishaw/sub2api:latest`.
- The full source code is kept in this private repository for future customization.
- When source edits are needed, build with `docker-compose.build.yml` and switch `SUB2API_IMAGE` to `sub2api:private-prod`.
- The service binds to `127.0.0.1:18080` by default so it is not publicly exposed until Nginx/domain routing is added.

## Server Commands

```bash
cd /opt/sub2api-src/deploy/tanzhongyu
./setup-server.sh
docker compose ps
docker compose logs -f sub2api
```

## Private Build

```bash
cd /opt/sub2api-src/deploy/tanzhongyu
docker compose -f docker-compose.yml -f docker-compose.build.yml build sub2api
SUB2API_IMAGE=sub2api:private-prod docker compose up -d
```

