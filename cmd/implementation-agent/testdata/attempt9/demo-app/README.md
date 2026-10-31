# Demo App

A small map of community places: parks, libraries, cafes and more. No API keys. No sign-up.

**[Documentation](docs/README.md)** | **[Changelog](CHANGELOG.md)** | **[Issues](docs/issues.md)**

![Demo App](public/screenshot.png)

## Features

- **Places map** - every imported place as a marker, clustered at low zoom
- **Categories** - parks, libraries, cafes, museums, markets and pools out of the box
- **Search** - prefix search over names and addresses, served from Redis
- **Opening hours** - today's hours on each place card, with a closed badge
- **CSV import** - drop a CSV into the data directory and it is picked up
- **Geocoding** - optional address lookup for rows without coordinates
- **Layers** - toggle categories on and off, with a legend per layer
- **Live updates** - WebSocket push when an import finishes
- **Admin page** - import history and per-category counts
- **Keyboard friendly** - every control reachable without a mouse
- **Small footprint** - one Node process and one Redis

## Quick Start

```bash
npm install
cp .env.example .env
docker compose up -d
```

Open `http://localhost:3080`. The sample places load within a few seconds; your own CSV files are imported on the next refresh.

## How it fits together

```
CSV files                         Browser
   |                                 ^
Importer -> geocoder (optional)      |
   |                                 |
   v                                 |
Redis  <---------------  Express server (:3080)
                         /places, /search, /layers
                         WebSocket /ws
```

## Endpoints

```
GET /places?category=parks&bbox=minLon,minLat,maxLon,maxLat
GET /places/:id
GET /search?q=library
GET /layers
GET /health
WS  /ws -> { type: "import-finished" }
```

Endpoint details: [docs/endpoints.md](docs/endpoints.md)

## Documentation

- [Getting Started](docs/getting-started.md) - prerequisites, first run
- [Settings](docs/configuration.md) - environment variables
- [Endpoints](docs/endpoints.md) - request and response shapes
- [Importing](docs/importing.md) - CSV columns and the geocoder
- [Layers](docs/layers.md) - categories, icons and legends
- [Deployment](docs/deployment.md) - running behind a reverse proxy
- [Troubleshooting](docs/troubleshooting.md) - common problems

## Development

```bash
npm run dev     # server with reload
npm test        # vitest
npm run lint    # eslint
```

## Contributing

Bug reports and pull requests are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT - see [LICENSE](LICENSE)
