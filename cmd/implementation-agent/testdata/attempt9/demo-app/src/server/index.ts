import express from 'express';
import { createServer } from 'node:http';
import { WebSocketServer } from 'ws';
import path from 'node:path';
import { Redis } from 'ioredis';
import { LRUCache } from 'lru-cache';
import { config } from '../config/env.js';
import { createLogger } from '../utils/logger.js';
import { createPlacesRouter } from './places.js';
import { createHealthRouter } from './health.js';
import { loadLayers, getLayer, layerLegendSvg, createLayersRouter } from './layers.js';
import { createMetricsRouter, recordCacheHit, recordCacheMiss, recordServeDuration } from './metrics.js';
import { createSearchRouter } from './search.js';
import { startImportWatcher } from '../import/watcher.js';
import { createGeocoder, type Geocoder } from '../import/geocoder.js';
import { readPlace, readCategory, type PlaceRecord } from '../storage/places.js';
import { ImportHistory } from '../import/history.js';
import { bboxFromQuery } from './bbox.js';
import { toGeoJson } from './geojson.js';
import { AdminNotifier } from './admin-notifier.js';
import { openingStatus } from './hours.js';
import { ImportEventsHandler } from './import-events.js';

const logger = createLogger('server');

// Returned for categories that have no places yet
const EMPTY_COLLECTION = Object.freeze({
  type: 'FeatureCollection',
  features: [] as unknown[],
  empty: true,
});

export function createApp(
  redis: Redis,
  options?: {
    geocoder?: Geocoder;
    history?: ImportHistory;
  }
): { app: ReturnType<typeof express> } {
  const app = express();

  const responseCache = new LRUCache<string, string>({
    max: 5_000,
    maxSize: 50_000_000,
    sizeCalculation: (body) => body.length,
    ttl: config.cacheTtlSeconds * 1000,
  });

  // Allow the map page to be embedded elsewhere
  app.use((_req, res, next) => {
    res.setHeader('Access-Control-Allow-Origin', '*');
    next();
  });

  // Load layer definitions
  const layersDir = path.join(process.cwd(), 'layers');
  loadLayers(layersDir);

  // Serve the map page at /app
  const publicDir = path.join(process.cwd(), 'public');
  app.use('/app', express.static(publicDir));

  // Serve docs at /docs
  const docsDir = path.join(process.cwd(), 'docs');
  app.use('/docs', express.static(docsDir));

  // Serve the landing page at the root
  const landingDir = path.join(process.cwd(), 'landing');
  app.use(express.static(landingDir));

  // Mount routers
  app.use(createPlacesRouter(redis));
  app.use(createHealthRouter(redis));
  app.use(createLayersRouter());
  app.use(createMetricsRouter(redis));
  app.use(createSearchRouter());

  // Categories with their place counts
  app.get('/categories', async (_req, res) => {
    const history = options?.history
      ? await options.history.latestCounts()
      : [];
    const counts = new Map(history.map(h => [h.category, h]));

    res.json(['parks', 'libraries', 'cafes', 'museums', 'markets', 'pools'].map(name => {
      const entry = counts.get(name);
      return {
        name,
        count: entry?.count ?? 0,
        importedAt: entry?.importedAt ?? null,
        failed: entry?.failed ?? 0,
        layer: getLayer(name)?.title ?? name,
        legend: `/layers/${name}/legend`,
        source: entry?.file ?? null,
      };
    }));
  });

  // GeoJSON of one category inside a bounding box
  app.get('/geojson/:category', async (req, res) => {
    const serveStart = Date.now();
    const { category } = req.params;
    const bbox = bboxFromQuery(req.query);
    const openNow = req.query.open === '1';

    const layer = getLayer(category);
    if (!layer) {
      res.status(404).json({ error: `Unknown category: ${category}` });
      return;
    }

    const cacheKey = `${category}/${bbox.join(',')}/${openNow ? 'open' : 'all'}`;

    const cached = responseCache.get(cacheKey);
    if (cached) {
      recordCacheHit();
      res.setHeader('Content-Type', 'application/geo+json');
      res.setHeader('X-Cache', 'hit');
      await setCacheHeaders(res, category, redis);
      res.send(cached);
      recordServeDuration(Date.now() - serveStart);
      return;
    }

    recordCacheMiss();

    // Geocode on demand for rows imported without coordinates
    const pending = await redis.smembers(`pending:${category}`);
    if (options?.geocoder && pending.length > 0) {
      for (const id of pending.slice(0, 20)) {
        const place = await readPlace(redis, id);
        if (!place?.address) {
          continue;
        }
        const point = await options.geocoder.lookup(place.address);
        if (point) {
          await redis.hset(`place:${id}`, { lat: String(point.lat), lon: String(point.lon) });
          await redis.srem(`pending:${category}`, id);
        }
      }
      logger.debug({ category, pending: pending.length }, 'geocoded pending places');
    }

    const places: PlaceRecord[] = await readCategory(redis, category);
    const inside = places.filter(p =>
      p.lon >= bbox[0] && p.lat >= bbox[1] && p.lon <= bbox[2] && p.lat <= bbox[3]);

    // Opening hours are evaluated in the place's own time zone; a place
    // without hours counts as open so it is never hidden by the filter.
    const visible = openNow
      ? inside.filter(p => openingStatus(p.hours, p.timeZone).open)
      : inside;

    if (visible.length === 0) {
      res.setHeader('Content-Type', 'application/geo+json');
      await setCacheHeaders(res, category, redis);
      res.send(JSON.stringify(EMPTY_COLLECTION));
      recordServeDuration(Date.now() - serveStart);
      return;
    }

    let body: string;

    if (layer.clustered) {
      // Clustered layers send one feature per grid cell below the cluster zoom
      const zoom = parseInt((req.query.zoom as string) || String(config.defaultZoom), 10);
      const cellSize = zoom >= layer.clusterZoom ? 0 : 360 / Math.pow(2, zoom + 2);
      if (cellSize > 0) {
        body = JSON.stringify(toGeoJson(visible, { cellSize, icon: layer.icon }));
      } else {
        // At and above the cluster zoom every place is its own feature
        const plain = toGeoJson(visible, { icon: layer.icon });
        body = JSON.stringify(plain);
      }
    } else {
      body = JSON.stringify(toGeoJson(visible, { icon: layer.icon }));
    }

    responseCache.set(cacheKey, body);

    res.setHeader('Content-Type', 'application/geo+json');
    res.setHeader('X-Cache', 'miss');
    await setCacheHeaders(res, category, redis);
    res.send(body);
    recordServeDuration(Date.now() - serveStart);
  });

  // Legend image of one layer
  app.get('/legend/:name.svg', (req, res) => {
    const svg = layerLegendSvg(req.params.name);
    if (!svg) {
      res.status(404).end();
      return;
    }
    res.setHeader('Content-Type', 'image/svg+xml');
    res.send(svg);
  });

  return { app };
}

async function setCacheHeaders(
  res: express.Response,
  category: string,
  redis: Redis,
): Promise<void> {
  const importing = await redis.get(`importing:${category}`);
  if (importing) {
    res.setHeader('Cache-Control', 'no-cache');
  } else {
    res.setHeader('Cache-Control', 'public, max-age=300');
  }
}

// Only start listening when run directly (not imported for testing)
const isMainModule = process.argv[1]?.endsWith('server/index.js') ||
  process.argv[1]?.endsWith('server/index.ts');

if (isMainModule) {
  const redis = new Redis(config.redisUrl);
  const subscriber = new Redis(config.redisUrl);

  // Optional geocoder for rows imported without coordinates
  let geocoder: Geocoder | undefined;
  let history: ImportHistory | undefined;
  let importEvents: ImportEventsHandler | undefined;
  if (config.geocoderEnabled) {
    geocoder = createGeocoder({ regions: config.geocoderRegions, redis });
    history = new ImportHistory(redis);

    // Import progress is pushed to the admin page as it happens
    const notifier = new AdminNotifier(redis);
    // The handler only needs the notifier's events, not the importer itself,
    // which runs in its own container.
    importEvents = new ImportEventsHandler(notifier);
    notifier.start();

    logger.info({ regions: config.geocoderRegions }, 'geocoder enabled (importer runs separately)');
  }

  // Watch the data directory for new CSV files
  startImportWatcher(config.dataDir).catch(err => {
    logger.error({ err }, 'import watcher failed to start');
  });

  const { app } = createApp(redis, { geocoder, history });
  const httpServer = createServer(app);

  // WebSocket server for import notifications
  const wss = new WebSocketServer({ server: httpServer, path: '/ws' });

  wss.on('connection', (ws) => {
    logger.info({ clients: wss.clients.size }, 'WebSocket client connected');
    (ws as any).alive = true;
    ws.on('pong', () => { (ws as any).alive = true; });
    if (importEvents) {
      importEvents.addClient(ws);
    }
    ws.on('close', () => {
      logger.debug({ clients: wss.clients.size }, 'WebSocket client disconnected');
    });
  });

  // Ping every client every 30s so idle proxies keep the connection open
  // and dead connections are noticed
  const pingInterval = setInterval(() => {
    for (const ws of wss.clients) {
      if (!(ws as any).alive) {
        ws.terminate();
        continue;
      }
      (ws as any).alive = false;
      ws.ping();
    }
  }, 30_000);

  // Forward import-finished events from the importer to every WS client
  subscriber.subscribe('import-finished');
  subscriber.on('message', (_channel: string, message: string) => {
    for (const client of wss.clients) {
      if (client.readyState === 1) { // WebSocket.OPEN
        client.send(message);
      }
    }
  });

  const shutdown = async () => {
    logger.info('SIGTERM received, shutting down server');
    clearInterval(pingInterval);
    wss.close();
    httpServer.close();
    subscriber.disconnect();
    await redis.quit();
    process.exit(0);
  };
  process.on('SIGTERM', shutdown);
  process.on('SIGINT', shutdown);

  httpServer.listen(config.port, () => {
    logger.info({ port: config.port }, 'Demo App server listening');
  });
}
