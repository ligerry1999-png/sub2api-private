# Tanzhongyu Sub2API Deployment

This directory is the private deployment layer for the Tanzhongyu Sub2API test instance.

## Current Strategy

- Default runtime uses the upstream Docker image: `weishaw/sub2api:latest`.
- The full source code is kept in this private repository for future customization.
- When source edits are needed, build with `docker-compose.build.yml` and switch `SUB2API_IMAGE` to `sub2api:private-prod`.
- The service binds to `127.0.0.1:18080` by default so it is not publicly exposed until Nginx/domain routing is added.
- For a temporary browser preview without changing the production `api.tanzhongyu.asia` gateway, `nginx-sub2api-preview-8443.conf` exposes Sub2API at `https://api.tanzhongyu.asia:8443/` with Basic Auth.
- For the production endpoint, first install `nginx-sub2api-http-precert.conf`, then issue the certificate, then switch to `nginx-sub2api-prod.conf`.

## Server Commands

```bash
cd /opt/sub2api-src/deploy/tanzhongyu
./setup-server.sh
docker compose ps
docker compose logs -f sub2api
```

## Temporary Preview Nginx

```bash
mkdir -p /etc/nginx/auth
printf 'sub2preview:%s\n' "$(openssl passwd -apr1 'replace-with-a-strong-password')" > /etc/nginx/auth/sub2api-preview.htpasswd
cp nginx-sub2api-preview-8443.conf /etc/nginx/sites-available/sub2api-preview
ln -sf /etc/nginx/sites-available/sub2api-preview /etc/nginx/sites-enabled/sub2api-preview
nginx -t
systemctl reload nginx
```

## Production Nginx

```bash
mkdir -p /var/www/letsencrypt
cp nginx-sub2api-http-precert.conf /etc/nginx/sites-available/sub2api
ln -sf /etc/nginx/sites-available/sub2api /etc/nginx/sites-enabled/sub2api
nginx -t
systemctl reload nginx

# After DNS points sub2api.tanzhongyu.asia to the server:
certbot certonly --webroot -w /var/www/letsencrypt -d sub2api.tanzhongyu.asia
cp nginx-sub2api-prod.conf /etc/nginx/sites-available/sub2api
nginx -t
systemctl reload nginx
```

## Private Build

```bash
cd /opt/sub2api-src/deploy/tanzhongyu
docker compose -f docker-compose.yml -f docker-compose.build.yml build sub2api
SUB2API_IMAGE=sub2api:private-prod docker compose up -d
```
