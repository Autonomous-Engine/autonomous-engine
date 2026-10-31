# Station Finder

Station Finder shows live bike and dock availability for a city's bike-share
system on a map. A small importer polls the operator's public station feed,
keeps the latest status of every station in Redis, and a Node server serves a
JSON API, the map app and a landing page.

## Features

- Live map of every station with bikes, e-bikes and free docks
- Search by station name or street
- Filters: has bikes, has e-bikes, has free docks
- Favourites stored in the browser
- 24-hour availability chart per station
- Landing page with a small preview map

## Quick start

```bash
cp .env.example .env
docker compose up -d
```

Open http://localhost:8700/ for the landing page and http://localhost:8700/app/
for the map.

## Development

```bash
npm install
npm run dev        # server with reload on change
npm test           # vitest unit tests
npm run lint
npm run build      # compiles to dist/
```

The server entry point is `src/server/index.ts`; `createApp` builds the Express
app without opening a port, so the tests drive it with supertest.

## Layout

| Path | What it holds |
|------|---------------|
| `src/server/` | Express app, API routes, metrics |
| `src/feed/` | Station feed client and importer |
| `src/store/` | Redis and in-memory station stores |
| `src/config/env.ts` | All settings, read from the environment |
| `public/` | The map app (`/app/`) |
| `landing/` | The landing page (`/`) |
| `tests/unit/` | Unit tests |
| `docs/` | Configuration and operations notes |

## Configuration

Every setting is an environment variable; see `.env.example` for the list and
[docs/configuration.md](docs/configuration.md) for details. The defaults run
against the sample feed.

## Deployment

Run the server and the importer behind a reverse proxy with TLS. Both read the
same environment; `docker-compose.yml` wires them to one Redis. The server is
stateless, so several replicas can run behind the proxy. Point the proxy's
health check at `GET /healthz`.

## API

- `GET /api/stations` - every station; `?bbox=west,south,east,north` limits the area
- `GET /api/stations/:id` - one station
- `GET /api/stations/:id/history` - availability samples for the last `HISTORY_HOURS`
- `GET /healthz` - store and feed health
- `GET /metrics` - Prometheus metrics

## Data

Station data comes from the operator's public feed and is shown as published.
Map data (c) OpenStreetMap contributors.

## License

MIT