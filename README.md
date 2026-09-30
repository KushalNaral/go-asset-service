# Asset Service – caching image proxy

Sits in front of the shop's `/storage` and keeps the files in memory, so the API stack only serves each image once per TTL.
It also resizes on the fly (`?w=&h=&q=&format=&fit=`) and records who asks for what, which the admin dashboard shows under **Settings → Image cache**.

```
browser ─▶ host nginx /storage ─▶ asset-service :8087 ─(miss)─▶ ecommerce_nginx /storage
                                        │
                                        └── memory cache (LRU, bounded by bytes)
```

## Features

- Caching reverse proxy for `ORIGIN_URL` (or reads `BASE_PATH` from disk when no origin is set)
- Memory cache bounded by entries **and** bytes, least recently used evicted first
- Expired entries are revalidated with `If-None-Match` / `If-Modified-Since` — unchanged files are not downloaded again
- Serves the expired copy for `STALE_IF_ERROR` when the origin is down
- Remembers missing files for `NEGATIVE_TTL`, so broken links don't hammer the origin
- Concurrent misses for the same file share one origin request
- Files over `MAX_OBJECT_MB` (videos) are streamed through with `Range` support, never cached
- Range, `If-None-Match`, `If-Modified-Since` and `HEAD` answered from memory
- Resizing with libvips: `?w=800&h=600&q=80&format=webp&fit=cover|contain|scale-down`
- Stats: hits/misses, bytes, latency percentiles, per-minute/hour/day traffic, referring sites and pages, client IPs,
  devices, browsers and bots, per-file history, missing files, live request log; saved to `STATS_FILE`
- `X-Cache` response header: `HIT`, `MISS`, `REVALIDATED`, `STALE`, `BYPASS`, `NOT_FOUND`, `ERROR`

## Production

1. Configure and start (joins the API's `ecommerce_network`, origin = `http://ecommerce_nginx/storage`):

   ```bash
   cp .env.example .env   # set ADMIN_KEY to a long random string
   docker compose up -d --build
   curl -sI http://127.0.0.1:8087/asset/<some/stored/file.jpg> | grep -i x-cache
   ```

2. Point the host nginx `/storage` at it (`/etc/nginx/conf.d/madanfurniture.conf`):

   ```nginx
   location /storage/ {
       proxy_pass http://127.0.0.1:8087/asset/;
       proxy_set_header Host $host;
       proxy_set_header X-Real-IP $remote_addr;
       proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
       proxy_set_header X-Forwarded-Proto $scheme;
   }
   ```

   `nginx -t && systemctl reload nginx`. To roll back, point it at `http://127.0.0.1:7032/storage/` again.

3. In the API `.env`: `ASSET_SERVICE_URL=http://asset-service:8080` and `ASSET_SERVICE_ADMIN_KEY=<same ADMIN_KEY>`, then
   `php artisan config:clear`.

## Admin API

All routes need `X-Admin-Key: $ADMIN_KEY` (or `Authorization: Bearer`). They are off when `ADMIN_KEY` is empty.

| Route | What |
| --- | --- |
| `GET /admin/overview` | totals, cache, origin, series (60 min / 48 h / 14 days), top referers, clients, devices, browsers, bots, types, folders, top and missing files |
| `GET /admin/entries?search=&kind=original\|variant\|missing&state=fresh\|stale&sort=hits\|bytes\|created\|expires\|last_hit&dir=&page=&per_page=` | what is in memory now |
| `GET /admin/paths?search=&status=ok\|missing\|errors\|cached\|uncached&sort=requests\|hits\|misses\|not_found\|bytes\|last_seen&page=` | every file seen, cached or not |
| `GET /admin/path?path=products/1/a.webp` | one file: stats, cached copies, referers, sizes asked for, recent requests |
| `GET /admin/requests?limit=&outcome=&device=&search=` | latest requests, newest first (last 1000 kept) |
| `POST /admin/purge` | `{"all":true}`, `{"path":"…"}` (with its resized copies), `{"prefix":"products/12/"}` or `{"key":"…"}` |
| `POST /admin/warm` | `{"paths":["products/1/a.jpg"]}` — fetch into the cache ahead of visitors |
| `POST /admin/stats/reset` | start counting again |
| `GET /admin/health` | source reachable, caching on/off, memory, recent errors (503 when the source is down) |
| `GET /admin/settings`, `POST /admin/settings` | `{"enabled":false,"purge":true}` — switch caching off (requests go straight to the source) or on; saved to `SETTINGS_FILE` (defaults next to `STATS_FILE`) |
| `GET /admin/clear-cache?secret=` | legacy full purge |

## Configuration

See `.env.example`. The important ones:

| Variable | Default | |
| --- | --- | --- |
| `ORIGIN_URL` | – | e.g. `http://ecommerce_nginx/storage`; empty = read `BASE_PATH` |
| `CACHE_MEMORY_MB` / `CACHE_SIZE` | 512 / 1024 | memory and entry limits |
| `CACHE_TTL` | 24h (`CACHE_TTL_HOURS` still read) | fresh period before revalidation |
| `MAX_OBJECT_MB` | 20 | larger files are streamed |
| `NEGATIVE_TTL` / `STALE_IF_ERROR` / `BROWSER_MAX_AGE` | 1m / 24h / 168h | |
| `ADMIN_KEY` | – (`CLEAR_KEY` still read) | admin API key |
| `STATS_FILE` | – | where stats are saved every 5 minutes and on shutdown |

## Development

Resizing needs libvips (`apt install libvips-dev pkg-config`). Without it, build and test with the `novips` tag —
originals are served and resize parameters are ignored:

```bash
go test -tags novips -race ./...
ORIGIN_URL=https://newmadanfurnishers.com/storage ADMIN_KEY=dev go run -tags novips .
```
