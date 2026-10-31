// Every setting comes from the environment; see docs/configuration.md.
export const config = {
  redisUrl: process.env.REDIS_URL || 'redis://localhost:6379',
  dataDir: process.env.DATA_DIR || './data',
  port: parseInt(process.env.PORT || '8700', 10),
  logLevel: process.env.LOG_LEVEL || 'info',
  cityName: process.env.CITY_NAME || 'Sample City',
  defaultLat: parseFloat(process.env.DEFAULT_LAT || '40.0'),
  defaultLon: parseFloat(process.env.DEFAULT_LON || '-75.0'),
  defaultZoom: parseInt(process.env.DEFAULT_ZOOM || '13', 10),
  feedUrl: process.env.FEED_URL || 'https://feeds.example.org/stations.json',
  feedPollSeconds: parseInt(process.env.FEED_POLL_SECONDS || '60', 10),
  feedTimeoutMs: parseInt(process.env.FEED_TIMEOUT_MS || '10000', 10),
  stationTtlSeconds: parseInt(process.env.STATION_TTL_SECONDS || '900', 10),
  historyHours: parseInt(process.env.HISTORY_HOURS || '24', 10),
  maxStations: parseInt(process.env.MAX_STATIONS || '5000', 10),
  corsOrigin: process.env.CORS_ORIGIN || '*',
  rateLimitPerMinute: parseInt(process.env.RATE_LIMIT_PER_MINUTE || '120', 10),
  metricsEnabled: process.env.METRICS_ENABLED !== 'false',
  landingEnabled: process.env.LANDING_ENABLED !== 'false',
} as const;

export type Config = typeof config;