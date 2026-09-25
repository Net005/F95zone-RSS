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
existing images/manifest are picked up as-is; a legacy `data/releases_cache.json` (last-90 snapshot) or
`config/settings.json` are imported once into the database and renamed `.imported`.

## Control panel (`/`)
Dashboard, Releases, Notifications, Live Log, Run History, Feed, Settings.

* **Dashboard** - progress, run/force/full/stop, pause scheduler, storage tools, backfill-older-releases control.
* **Releases** - full scrape history (not just the last 90 in the feed), search, multi-tag AND filter with a
  tag picker + chips, engine filter, sort, saved filter sets (name a combination of filters and recall it
  later - both the current filter and the saved sets persist in the browser across reloads). Infinite scroll
  loads 60 releases at a time and appends more automatically as you near the bottom (with a manual "Load
  more" fallback). Detail view shows the exact feed rendering plus Engine/Version/Developer/Thread-updated
  fields; click any screenshot to open it full-size in a lightbox (Escape or click-outside to close).
* **Notifications** - a card per new release or version change your monitoring caught, same detail layout as
  Releases, with per-item and clear-all/mark-all-read actions. Optional Pushover push (see below).
* **Live Log** - SSE stream, level filter, search, follow/pause, download, clear.
* **Run History** - one row per pipeline run, including new/updated counts.
* **Feed** - URLs, rebuild-from-cache, current feed-window size vs. total releases stored.
* **Settings** - edited live, no restart.

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
backfill listing URL template, fetch mode (`http` or headless `browser`), Chromium path, user agent,
site session cookie, run-on-start, scheduler on/off, reuse unchanged, notification toggles, Pushover.

### Site session cookie (recommended)
F95zone gates some thread content - notably the Genre/tags spoiler block on many threads - behind login, even
for otherwise-public threads. Guest scraping will see an empty Genre and no tags on those threads. To scrape
as your own account, open the site in a browser where you're logged in, copy the request `Cookie:` header
(devtools -> Network -> any request to f95zone.to -> Headers -> Cookie), and paste the whole value into
Settings > Site session cookie. It's sent as-is on every enrichment and backfill request, for both `http` and
`browser` fetch modes, and is never sent anywhere else. Re-scrape a thread (or wait for the next scheduled
run) to pick up tags once the cookie is set. Spoiler-gated fields (Genre and others are often wrapped in a
spoiler tag) are unfolded and used like any other spoiler when the cookie is valid and F95zone serves the
real content; only the "you don't have permission to view the spoiler content" placeholder itself (shown to
guests or with an expired cookie) is filtered back out.

## Release database & backfill
Every release ever scraped is kept in SQLite (`releases`, `release_tags`, `notifications` tables) - the RSS
feed only ever shows the current "feed window" (matching the source feed's own item count), but the Releases
tab and the API see the full history. Use Dashboard > "Backfill older releases" to walk N pages of the
listing at `https://f95zone.to/sam/latest_alpha/#/cat=games/page=N` (configurable in Settings), harvest thread
links, and enrich any not already in the database. That listing is a client-rendered Angular page, so backfill
always uses the headless-browser fetch mode regardless of the configured fetch mode, and - like most of the
site - likely needs the site session cookie above to return full pages. Backfilled items are not sent to
notifications/Pushover. Backfill stops early after several consecutive empty pages.

## Failure alerts
When Pushover is enabled, a pipeline run or backfill that ends in an error (RSS fetch failure, Chromium
launch failure, every item failing enrichment in one run - commonly an expired site cookie or a Cloudflare
block) sends one Pushover alert. Further failures on the same UTC day are suppressed so a scheduler retrying
every few hours doesn't spam your phone with the same underlying problem; the next alert can go out the
following day (or immediately after a run that finishes without error resets nothing - it's purely date-based).

## Tag, engine and version parsing
Genre is parsed from the thread's own metadata block (the same "Thread Updated / Release Date / Developer /
Censored / Version / OS / Language / Store / Genre" block F95zone shows on every game thread) and becomes the
release's Tags, comma-split and trimmed. Labels (things like "COMPLETED", "ABANDONED", the raw title-bracket
text) and Engine (Ren'Py, Unreal, RPGM, Unity, Godot, HTML, Flash, ... matched against title brackets) are
kept as separate fields. Version prefers the thread's own explicit "Version:" field and falls back to the
title-bracket guess only when the thread doesn't state one. Existing rows imported from an older parser
version are automatically re-scraped once after an upgrade like this to pick up corrected values.

## Monitoring & notifications
Every enrichment run compares the new scrape against the stored row for that link: a link seen for the first
time is a "new release" notification, and a version-string change on an existing link is an "update"
notification (either can be turned off in Settings). Notifications appear in the Notifications tab and,
if configured, are pushed via Pushover (Settings > Notifications > enable Pushover, set your user key and
API token from pushover.net, use "Send test notification" to verify). Clear individual notifications or all
of them from the Notifications tab.

## Behaviour notes
* Schedule matches the old cron `hour=*/N` (minute 0, UTC).
* `reuse_unchanged`: threads whose link+title (version) are unchanged reuse the previous description/images instead of
  being re-scraped; "Full re-scrape" bypasses it.
* A cancelled run leaves feed.xml and the release database untouched.
* Fixes vs Python: lock file leak on cache-hit path removed (single in-process run guard), per-item image download no
  longer reuses a stale variable after an enrichment failure.
