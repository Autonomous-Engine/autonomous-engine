const LOG_LEVELS = new Set(['trace', 'debug', 'info', 'warn', 'error', 'fatal', 'silent']);

function logLevelFrom(raw: string | undefined): string {
  const level = (raw ?? 'info').toLowerCase();
  return LOG_LEVELS.has(level) ? level : 'info';
}

export const config = {
  redisUrl: process.env.REDIS_URL || 'redis://localhost:6379',
  dataDir: process.env.DATA_DIR || './var/places',
  port: parseInt(process.env.PORT || '3080', 10),
  searchLimit: parseInt(process.env.SEARCH_LIMIT || '25', 10),
  cacheTtlSeconds: parseInt(process.env.CACHE_TTL_SECONDS || '300', 10),
  importBatchSize: parseInt(process.env.IMPORT_BATCH_SIZE || '500', 10),
  logLevel: logLevelFrom(process.env.LOG_LEVEL),
  refreshIntervalMs: parseInt(process.env.REFRESH_INTERVAL_MS || '60000', 10),
  geocoderEnabled: process.env.GEOCODER_ENABLED !== 'false',  // on unless disabled
  geocoderZoomMin: parseInt(process.env.GEOCODER_ZOOM_MIN || '8', 10),
  geocoderRegions: process.env.GEOCODER_REGIONS || 'all',
  adminPort: parseInt(process.env.ADMIN_PORT || '3081', 10),
  defaultZoom: parseInt(process.env.DEFAULT_ZOOM || '12', 10),
  seedFile: process.env.SEED_FILE || '',
} as const;
