const VALID_LOG_LEVELS = new Set(['trace', 'debug', 'info', 'warn', 'error', 'fatal', 'silent']);

function parseLogLevel(raw: string | undefined): string {
  const level = raw ?? 'info';
  return VALID_LOG_LEVELS.has(level) ? level : 'info';
}

const config = {
  port: parseInt(process.env.PORT || '8700', 10),
  dataDir: process.env.DATA_DIR || './data',
  storeUrl: process.env.STORE_URL || 'file://./data/books.db',
  logLevel: parseLogLevel(process.env.LOG_LEVEL),
  searchEnabled: process.env.SEARCH_ENABLED !== 'false', // on by default
  searchIndex: process.env.SEARCH_INDEX || 'books',
  searchPageSize: parseInt(process.env.SEARCH_PAGE_SIZE || '20', 10),
  coverSizes: (process.env.COVER_SIZES || 's,m,l').split(','),
  retentionDays: parseInt(process.env.RETENTION_DAYS || '30', 10),
  reindexIntervalMs: parseInt(process.env.REINDEX_INTERVAL_MS || '60000', 10),
} as const;

export type Config = typeof config;
export { config };
