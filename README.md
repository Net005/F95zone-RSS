# F95Zone RSS Enricher - Go edition

Go rewrite of `main.py` with a web control panel styled after F95zone.

## Run with Docker (recommended)
    cp .env.example .env      # optional: set DATA_DIR / PORT / ADMIN_*
    docker compose up -d      # pulls ghcr.io/net005/f95zone-rss:latest
Open http://HOST:6069/ - on first visit you create the administrator account, then configure everything under Settings. To build locally instead, uncomment `build: .` in `docker-compose.yml`
and run `docker compose up -d --build`. The image is published to GHCR by `.github/workflows/docker.yml`.

## Run from source
    go build -o f95zone .
    ./f95zone -base /path/to/base -port 6069

`-base` (env `F95_BASE_DIR`) holds `data/` and `logs/` - point it at the old Python directory and the
existing cache, images and manifest are picked up as-is; the old settings.json is imported once.

## Control panel (`/`)
Dashboard (progress, run/force/full/stop, pause scheduler, storage tools) - Live Log (SSE stream,
level filter, search, follow/pause, download, clear) - Releases (search, tag filter, detail with the exact
feed rendering, re-scrape one thread) - Run History - Feed (URLs, rebuild) - Settings (edited live, no restart).

## Authentication
Form login (session cookie, bcrypt password, login throttling) - no browser Basic/Windows auth prompt.
First visit shows "Create administrator". `ADMIN_USER` + `ADMIN_PASSWORD` env vars optionally pre-create the account
on first start. Change the password under Settings > Account.
Public without login: `/feed.xml` (also `/rss`), `/f95zone/images/<file>`, `/health`. Everything else needs a session.
Behind Caddy/TLS the cookie is marked Secure automatically (`X-Forwarded-Proto: https`).

## Settings
Managed in the web UI (Settings tab) and stored in the SQLite database `data/f95zone.db`.
A legacy `config/settings.json` from the Python version is imported automatically on startup and renamed to
`settings.json.imported`. Options: schedule hours, cache TTL, rate limit, public base URL, source RSS URL,
fetch mode (`http` or headless `browser`), Chromium path, user agent, run-on-start, scheduler on/off, reuse unchanged.

## Behaviour notes
* Schedule matches the old cron `hour=*/N` (minute 0, UTC).
* `reuse_unchanged`: threads whose link+title (version) are unchanged reuse the previous description/images instead of
  being re-scraped; "Full re-scrape" bypasses it.
* A cancelled run leaves feed.xml and the cache untouched.
* Fixes vs Python: lock file leak on cache-hit path removed (single in-process run guard), per-item image download no
  longer reuses a stale variable after an enrichment failure.
