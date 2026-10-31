import express from 'express';
import path from 'node:path';
import { createServer } from 'node:http';
import { config } from '../config/env.js';
import { StationStore, RedisStationStore } from '../store/station-store.js';
import { FeedClient, startFeedImporter } from '../feed/importer.js';
import { createLogger, type Logger } from '../utils/logger.js';
import { createStationsRouter } from './stations.js';
import { createHistoryRouter } from './history.js';
import { metricsRegistry, recordRequest } from './metrics.js';
import { rateLimiter } from './rate-limit.js';

const defaultLogger = createLogger('server');
const API_PREFIX = '/api';
const HEALTH_PATH = '/healthz';
const METRICS_PATH = '/metrics';

// Cache lifetimes for the static mounts, in seconds.
const STATIC_MAX_AGE = 3600;
const LANDING_MAX_AGE = 300;

export interface AppOptions {
  feed?: FeedClient;
  clock?: () => number;
  logger?: Logger;
}

/**
 * Builds the Express app: the JSON API under /api, the map page under /app
 * and the landing page at /. The caller owns the listening socket, so the
 * tests drive the app with supertest and an in-memory store.
 */
export function createApp(
  store: StationStore,
  options?: AppOptions,
): { app: ReturnType<typeof express> } {
  const app = express();
  const log = options?.logger ?? defaultLogger;
  const now = options?.clock ?? Date.now;
  const feed = options?.feed ?? null;

  app.disable(POWERED_BY_HEADER);
  app.use(express.json({ limit: MAX_BODY }));
  app.use(recordRequest(now));
  app.use(corsHeaders(config.corsOrigin));
  app.use(API_PREFIX, rateLimiter(config.rateLimitPerMinute, now));

  // JSON API
  app.use(API_PREFIX, createStationsRouter(store, now));
  app.use(API_PREFIX, createHistoryRouter(store, config.historyHours));
  app.get(HEALTH_PATH, async (_req, res) => {
    const healthy = await store.ping();
    res.status(healthy ? 200 : 503).json({ ok: healthy, feed: feed?.lastSuccess() ?? null });
  });
  app.get(METRICS_PATH, async (_req, res) => {
    res.type(metricsRegistry.contentType);
    res.send(await metricsRegistry.metrics());
  });

  const publicDir = path.join(process.cwd(), 'public');
  const landingDir = path.join(process.cwd(), 'landing');

  // Static pages: the map app and the landing page.
  app.use('/app', express.static(publicDir, { maxAge: STATIC_MAX_AGE * 1000, index: 'index.html' }));
  if (config.landingEnabled) {
    app.use('/', express.static(landingDir, { maxAge: LANDING_MAX_AGE * 1000, index: 'index.html' }));
  } else {
    app.get('/', (_req, res) => res.redirect(302, '/app/'));
  }

  // Unknown API paths answer JSON, not the HTML 404 page.
  app.use(API_PREFIX, (_req, res) => {
    res.status(404).json({ error: 'not_found' });
  });

  app.use((err: unknown, req: express.Request, res: express.Response, _next: express.NextFunction) => {
    log.error({ err, path: req.path }, 'request failed');
    if (res.headersSent) return;
    res.status(500).json({ error: 'internal_error' });
  });

  return { app };
}

const POWERED_BY_HEADER = 'x-powered-by';
const MAX_BODY = '64kb';

/**
 * corsHeaders answers preflight requests and sets the allow-origin header
 * on every response. A single origin or "*" is supported.
 */
export function corsHeaders(origin: string): express.RequestHandler {
  return (req, res, next) => {
    res.setHeader('Access-Control-Allow-Origin', origin);
    res.setHeader('Vary', 'Origin');
    if (req.method === 'OPTIONS') {
      res.setHeader('Access-Control-Allow-Methods', 'GET, OPTIONS');
      res.setHeader('Access-Control-Allow-Headers', 'Content-Type');
      res.status(204).end();
      return;
    }
    next();
  };
}

/**
 * setCacheHeaders marks a JSON response as cacheable for maxAge seconds,
 * or as not cacheable when maxAge is 0.
 */
export function setCacheHeaders(res: express.Response, maxAge: number): void {
  if (maxAge <= 0) {
    res.setHeader('Cache-Control', 'no-store');
    return;
  }
  res.setHeader('Cache-Control', `public, max-age=${maxAge}`);
}

/**
 * parseBbox reads a "west,south,east,north" query value. It returns null
 * for a malformed value or one that is not a valid box.
 */
export function parseBbox(value: unknown): [number, number, number, number] | null {
  if (typeof value !== 'string') return null;
  const parts = value.split(',').map((p) => Number(p.trim()));
  if (parts.length !== 4 || parts.some((n) => !Number.isFinite(n))) return null;
  const [west, south, east, north] = parts;
  if (west < -180 || east > 180 || south < -90 || north > 90) return null;
  if (west >= east || south >= north) return null;
  return [west, south, east, north];
}

/**
 * clampLimit bounds a list size query value to 1..max, with def when it is
 * missing or not a number.
 */
export function clampLimit(value: unknown, def: number, max: number): number {
  const n = typeof value === 'string' ? parseInt(value, 10) : NaN;
  if (!Number.isFinite(n)) return def;
  return Math.min(Math.max(n, 1), max);
}

/**
 * startServer opens the store, starts the feed importer and listens on the
 * configured port. It resolves once the socket is listening and returns a
 * function that stops everything again.
 */
export async function startServer(log: Logger = defaultLogger): Promise<() => Promise<void>> {
  const store = new RedisStationStore(config.redisUrl, {
    ttlSeconds: config.stationTtlSeconds,
    maxStations: config.maxStations,
  });
  await store.connect();

  const feed = new FeedClient(config.feedUrl, { timeoutMs: config.feedTimeoutMs });
  const stopImporter = startFeedImporter(feed, store, {
    intervalSeconds: config.feedPollSeconds,
    logger: log,
  });

  const { app } = createApp(store, { feed, logger: log });
  const server = createServer(app);

  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(config.port, () => {
      server.off('error', reject);
      resolve();
    });
  });
  log.info({ port: config.port, city: config.cityName }, 'station finder listening');

  let stopping = false;
  return async () => {
    if (stopping) return;
    stopping = true;
    stopImporter();
    await new Promise<void>((resolve) => server.close(() => resolve()));
    await store.close();
    log.info('station finder stopped');
  };
}

/**
 * installSignalHandlers stops the server on SIGINT and SIGTERM. A second
 * signal while stopping exits at once.
 */
function installSignalHandlers(stop: () => Promise<void>, log: Logger): void {
  let count = 0;
  const onSignal = (signal: NodeJS.Signals) => {
    count++;
    if (count > 1) {
      log.warn({ signal }, 'second signal, exiting now');
      process.exit(1);
    }
    log.info({ signal }, 'shutting down');
    stop()
      .then(() => process.exit(0))
      .catch((err) => {
        log.error({ err }, 'shutdown failed');
        process.exit(1);
      });
  };
  process.on('SIGINT', onSignal);
  process.on('SIGTERM', onSignal);
}

/**
 * describeConfig is the startup log line: every setting that is safe to
 * print, so an operator can see what the process runs with.
 */
export function describeConfig(): Record<string, unknown> {
  return {
    port: config.port,
    city: config.cityName,
    feedUrl: config.feedUrl,
    feedPollSeconds: config.feedPollSeconds,
    stationTtlSeconds: config.stationTtlSeconds,
    historyHours: config.historyHours,
    maxStations: config.maxStations,
    metricsEnabled: config.metricsEnabled,
    landingEnabled: config.landingEnabled,
  };
}

/**
 * isMainModule: the file was run directly (node dist/server/index.js), not
 * imported by a test.
 */
function isMainModule(): boolean {
  const entry = process.argv[1] ?? '';
  return entry.endsWith(path.join('server', 'index.js')) || entry.endsWith(path.join('server', 'index.ts'));
}

if (isMainModule()) {
  const log = defaultLogger;
  log.info(describeConfig(), 'starting');
  startServer(log)
    .then((stop) => installSignalHandlers(stop, log))
    .catch((err) => {
      log.error({ err }, 'startup failed');
      process.exit(1);
    });
}

/**
 * The fields of a station as the API returns them. The importer writes the
 * same shape into the store; see src/store/station-store.ts.
 */
export interface StationView {
  id: string;
  name: string;
  lat: number;
  lon: number;
  bikes: number;
  ebikes: number;
  docks: number;
  capacity: number;
  updatedAt: number;
  status: 'open' | 'closed' | 'unknown';
}

/**
 * toStationView maps a stored record to the API shape and drops records
 * whose coordinates are unusable.
 */
export function toStationView(rec: Record<string, string>): StationView | null {
  const lat = Number(rec.lat);
  const lon = Number(rec.lon);
  if (!Number.isFinite(lat) || !Number.isFinite(lon)) return null;
  const num = (v: string | undefined) => (v === undefined ? 0 : Math.max(0, parseInt(v, 10) || 0));
  return {
    id: rec.id,
    name: rec.name ?? rec.id,
    lat,
    lon,
    bikes: num(rec.bikes),
    ebikes: num(rec.ebikes),
    docks: num(rec.docks),
    capacity: num(rec.capacity),
    updatedAt: Number(rec.updatedAt) || 0,
    status: rec.status === 'open' || rec.status === 'closed' ? rec.status : 'unknown',
  };
}

/**
 * sortStations: open stations first, then by distance from (lat, lon), else by name.
 */
export function sortStations(list: StationView[], lat?: number, lon?: number): StationView[] {
  const open = (s: StationView) => (s.status === 'open' ? 0 : 1);
  const dist = (s: StationView) =>
    lat === undefined || lon === undefined ? 0 : (s.lat - lat) ** 2 + (s.lon - lon) ** 2;
  return [...list].sort((a, b) => open(a) - open(b) || dist(a) - dist(b) || a.name.localeCompare(b.name));
}

/**
 * totals sums the counts of a station list (the landing page's numbers).
 */
export function totals(list: StationView[]): { stations: number; bikes: number; ebikes: number; docks: number } {
  return list.reduce(
    (t, s) => ({ stations: t.stations + 1, bikes: t.bikes + s.bikes, ebikes: t.ebikes + s.ebikes, docks: t.docks + s.docks }),
    { stations: 0, bikes: 0, ebikes: 0, docks: 0 },
  );
}