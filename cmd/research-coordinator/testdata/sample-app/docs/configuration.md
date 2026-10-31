# Configuration

Station Finder reads every setting from the environment when the process
starts. Nothing is read from files at runtime except the `.env` file that
`docker compose` passes through. A changed setting needs a restart.

This page lists the settings by area. The defaults in parentheses are the
values `src/config/env.ts` falls back to when a variable is unset or empty.

## Server

### `PORT` (8700)

The HTTP port the server listens on. In the compose file the server listens
on 8700 inside the container and the port is published on the host.

### `LOG_LEVEL` (info)

One of `trace`, `debug`, `info`, `warn`, `error`. Logs are JSON lines on
stdout. `debug` logs every feed poll; `trace` also logs every request.

### `CORS_ORIGIN` (*)

The value of the `Access-Control-Allow-Origin` header on API responses. Set
it to your site's origin when the API should not be used from other sites.

### `RATE_LIMIT_PER_MINUTE` (120)

Requests per minute and client address on `/api`. Requests over the limit
get `429 Too Many Requests` with a `Retry-After` header.

### `LANDING_ENABLED` (true)

When `false`, `/` redirects to the map app at `/app/` and the landing page is
not served.

### `METRICS_ENABLED` (true)

When `false`, `/metrics` answers 404.

## Storage

### `REDIS_URL` (redis://localhost:6379)

Where station status and history are kept. The server and the importer must
point at the same Redis. Use `rediss://` for TLS.

### `DATA_DIR` (./data)

The importer writes a snapshot of the last good feed here, so a restart can
serve stations before the first poll succeeds.

### `STATION_TTL_SECONDS` (900)

A station that has not been updated for this long is dropped from the map.
Operators sometimes remove stations from the feed without notice; this keeps
removed stations from lingering.

### `HISTORY_HOURS` (24)

Hours of availability samples kept per station for the history chart. One
sample is kept per poll, so the history size is
`HISTORY_HOURS * 3600 / FEED_POLL_SECONDS` samples per station.

### `MAX_STATIONS` (5000)

An upper bound on stations accepted from the feed. A feed that suddenly
lists more is treated as broken and the previous snapshot is kept.

## Station feed

### `FEED_URL` (https://feeds.example.org/stations.json)

The operator's station status feed. The importer expects GBFS-style JSON:
`data.stations[]` with `station_id`, `name`, `lat`, `lon`,
`num_bikes_available`, `num_ebikes_available`, `num_docks_available`,
`capacity` and `is_renting`.

### `FEED_POLL_SECONDS` (60)

How often the feed is polled. Most operators update every 30-60 seconds;
polling faster than the feed's own `ttl` gains nothing.

### `FEED_TIMEOUT_MS` (10000)

Timeout of one feed request. A failed poll keeps the previous data and is
retried at the next interval.

## Map

### `CITY_NAME` (Sample City)

Shown in the page title and the landing page header.

### `DEFAULT_LAT`, `DEFAULT_LON` (40.0, -75.0)

Where the map opens when the visitor has not allowed geolocation.

### `DEFAULT_ZOOM` (13)

The map's start zoom level.

## Metrics

`GET /metrics` exposes Prometheus metrics:

- `http_requests_total` by route and status
- `http_request_duration_seconds` by route
- `feed_polls_total` and `feed_poll_failures_total`
- `stations_known` - stations currently in the store
- `feed_last_success_timestamp_seconds`

## Example: production

```bash
REDIS_URL=redis://redis:6379
PORT=8700
LOG_LEVEL=info
CITY_NAME=Sample City
DEFAULT_LAT=40.0
DEFAULT_LON=-75.0
FEED_URL=https://feeds.example.org/stations.json
FEED_POLL_SECONDS=30
CORS_ORIGIN=https://stations.example.org
```

## Example: local development

```bash
REDIS_URL=redis://localhost:6379
PORT=8700
LOG_LEVEL=debug
FEED_URL=http://localhost:8800/sample-feed.json
FEED_POLL_SECONDS=10
LANDING_ENABLED=false
```

The repository ships a sample feed under `tests/fixtures/`; serve it with any
static file server to develop without network access.

## Checking a deployment

1. `GET /healthz` must answer `{"ok": true}` and a recent `feed` time.
2. `GET /api/stations` must list stations.
3. The map at `/app/` must show station markers over the basemap.
4. `GET /metrics` must show `feed_polls_total` increasing.

If the map shows markers but no basemap, check the browser console for tile
errors. If it shows neither, check `/healthz` and the importer logs.

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| `/healthz` answers 503 | Redis is not reachable at `REDIS_URL` |
| No stations on the map | `FEED_URL` is wrong or the feed is down |
| Stations vanish after a while | The feed stopped updating; see `STATION_TTL_SECONDS` |
| `429` from the API | `RATE_LIMIT_PER_MINUTE` is too low for your traffic |
| History chart is empty | The importer has not run for long enough yet |

## Changing settings

Edit `.env` (or the `environment:` list in `docker-compose.yml`) and restart
the affected service. The server and the importer read the same variables;
restart both after a change to `REDIS_URL` or `FEED_URL`.