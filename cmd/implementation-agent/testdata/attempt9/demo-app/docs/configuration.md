# Settings

## Server Environment Variables

Every service reads the same environment variables (from `docker-compose.yml` or `.env`).

| Variable | Default | Description |
|---|---|---|
| `REDIS_URL` | `redis://localhost:6379` | Redis connection URL. Under Docker Compose use `redis://redis:6379`. |
| `DATA_DIR` | `./var/places` | Directory for imported CSV files and the search index snapshot. |
| `PORT` | `3080` | HTTP port the web server listens on. |
| `SEARCH_LIMIT` | `25` | Maximum number of results `/search` returns. |
| `CACHE_TTL_SECONDS` | `300` | Seconds a cached `/places` response stays fresh. `0` disables the cache. |
| `IMPORT_BATCH_SIZE` | `500` | Rows written per Redis pipeline during an import. |
| `LOG_LEVEL` | `info` | Pino log level. One of: `trace`, `debug`, `info`, `warn`, `error`, `fatal`, `silent`. |
| `REFRESH_INTERVAL_MS` | `60000` | How often the importer looks for new CSV files (milliseconds). |

## Geocoder Variables

| Variable | Default | Description |
|---|---|---|
| `GEOCODER_ENABLED` | `true` | Set to `false` to skip address lookup; rows without coordinates are then dropped. |
| `GEOCODER_ZOOM_MIN` | `8` | Lowest zoom at which geocoded places are shown. |
| `GEOCODER_REGIONS` | `all` | Comma-separated region codes the geocoder may resolve addresses in. |

**Notes:**
- The geocoder caches every lookup in Redis, so re-importing a file costs nothing.
- Limit `GEOCODER_REGIONS` when your data covers one area; lookups get faster and cheaper.

## Admin Variables

| Variable | Default | Description |
|---|---|---|
| `ADMIN_PORT` | `3081` | HTTP port of the admin page. |
| `DEFAULT_ZOOM` | `12` | Zoom level the map opens at. |
| `SEED_FILE` | *(none)* | CSV loaded on first start when Redis is empty. |

## Category: parks

Places in the `parks` category are imported from `$DATA_DIR/parks.csv` and drawn with the `park` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

## Category: libraries

Places in the `libraries` category are imported from `$DATA_DIR/libraries.csv` and drawn with the `librarie` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

## Category: cafes

Places in the `cafes` category are imported from `$DATA_DIR/cafes.csv` and drawn with the `cafe` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

## Category: museums

Places in the `museums` category are imported from `$DATA_DIR/museums.csv` and drawn with the `museum` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

## Category: markets

Places in the `markets` category are imported from `$DATA_DIR/markets.csv` and drawn with the `market` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

## Category: pools

Places in the `pools` category are imported from `$DATA_DIR/pools.csv` and drawn with the `pool` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

## Category: gardens

Places in the `gardens` category are imported from `$DATA_DIR/gardens.csv` and drawn with the `garden` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

## Category: theatres

Places in the `theatres` category are imported from `$DATA_DIR/theatres.csv` and drawn with the `theatre` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

## Category: bakeries

Places in the `bakeries` category are imported from `$DATA_DIR/bakeries.csv` and drawn with the `bakerie` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

## Category: bookshops

Places in the `bookshops` category are imported from `$DATA_DIR/bookshops.csv` and drawn with the `bookshop` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

## Category: playgrounds

Places in the `playgrounds` category are imported from `$DATA_DIR/playgrounds.csv` and drawn with the `playground` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

## Category: galleries

Places in the `galleries` category are imported from `$DATA_DIR/galleries.csv` and drawn with the `gallerie` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

## Category: halls

Places in the `halls` category are imported from `$DATA_DIR/halls.csv` and drawn with the `hall` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

## Category: studios

Places in the `studios` category are imported from `$DATA_DIR/studios.csv` and drawn with the `studio` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

## Category: clinics

Places in the `clinics` category are imported from `$DATA_DIR/clinics.csv` and drawn with the `clinic` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

## Category: trails

Places in the `trails` category are imported from `$DATA_DIR/trails.csv` and drawn with the `trail` icon.

| Column | Required | Notes |
|---|---|---|
| `name` | yes | Shown on the marker tooltip and the place card. |

