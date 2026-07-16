# Tanzhongyu Sub2API Deployment

This directory is the private deployment layer for the Tanzhongyu Sub2API test instance.

## Current Strategy

- Production runs the private image `sub2api:private-prod` built from this repository.
- Docker builds run only on GitHub Actions. The production server only receives the image bundle, runs `docker load`, and restarts the Sub2API container.
- Never run `docker build` or `docker compose build` on the 2-core/4-GB production server.
- The service binds to `127.0.0.1:18080` by default so it is not publicly exposed until Nginx/domain routing is added.
- For a temporary browser preview without changing the production `api.tanzhongyu.asia` gateway, `nginx-sub2api-preview-8443.conf` exposes Sub2API at `https://api.tanzhongyu.asia:8443/` with Basic Auth.
- For the production endpoint, first install `nginx-sub2api-http-precert.conf`, then issue the certificate, then switch to `nginx-sub2api-prod.conf`.

## Production Rule

Do not run `setup-server.sh`, `docker build`, or `docker compose build` on the
production server. Use the manual GitHub Actions `Deploy Server` workflow. The
server-side commands in that workflow are limited to loading the prebuilt image,
waiting for image jobs to become idle, and restarting the Sub2API container.

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

## Private Deployment

Run the GitHub Actions `Deploy Server` workflow manually. Keep `deploy=false` for build-only validation; after CI passes and image jobs are idle, run it again with `deploy=true`.
