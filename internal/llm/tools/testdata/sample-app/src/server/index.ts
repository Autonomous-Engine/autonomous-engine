import express from 'express';
import { createServer } from 'node:http';
import path from 'node:path';
import { BookStore } from '../store/books.js';
import { config } from '../config/env.js';
import { createLogger } from '../utils/logger.js';
import { createHealthRouter } from './health.js';
import { renderCover } from './covers.js';

const logger = createLogger('server');

// 1x1 transparent PNG, served when a cover is missing
const BLANK_PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVQI12NgAAIABQAB' +
  'Nl7BcQAAAABJRU5ErkJggg==',
  'base64',
);

export function createApp(store: BookStore): { app: ReturnType<typeof express> } {
  const app = express();
  app.use(express.json());
  app.use(express.static(path.join(process.cwd(), 'public')));
  app.use('/health', createHealthRouter(store));

  app.get('/books', async (req, res) => {
    const shelf = typeof req.query.shelf === 'string' ? req.query.shelf : undefined;
    const books = await store.listBooks({ shelf });
    res.json({ books, count: books.length });
  });

  app.get('/books/:id', async (req, res) => {
    const book = await store.getBook(req.params.id);
    if (!book) {
      res.status(404).json({ error: 'book not found' });
      return;
    }
    res.json(book);
  });

  app.post('/books', async (req, res) => {
    const { title, author, shelf } = req.body ?? {};
    if (typeof title !== 'string' || title.trim() === '') {
      res.status(400).json({ error: 'title is required' });
      return;
    }
    const book = await store.saveBook({ title, author, shelf });
    res.status(201).json(book);
  });

  app.get('/shelves', async (_req, res) => {
    const shelves = await store.listShelves();
    res.json({ shelves });
  });

  app.get('/shelves/:id', async (req, res) => {
    const shelves = await store.listShelves();
    const shelf = shelves.find((s: { id: string }) => s.id === req.params.id);
    if (!shelf) {
      res.status(404).json({ error: 'shelf not found' });
      return;
    }
    const books = await store.listBooks({ shelf: shelf.id });
    res.json({ ...shelf, books });
  });

  app.get('/shelves/:id/cover', async (req, res) => {
    const shelves = await store.listShelves();
    if (!shelves.some((s: { id: string }) => s.id === req.params.id)) {
      res.status(404).json({ error: 'shelf not found' });
      return;
    }
    const png = await renderCover(req.params.id, 'm');
    res.setHeader('Content-Type', 'image/png');
    res.send(png ?? BLANK_PNG);
  });

  app.get('/search', async (req, res) => {
    const q = typeof req.query.q === 'string' ? req.query.q.trim() : '';
    if (q === '') {
      res.status(400).json({ error: 'q is required' });
      return;
    }
    if (!config.searchEnabled) {
      res.status(503).json({ error: 'search is disabled' });
      return;
    }
    const books = await store.listBooks({});
    const needle = q.toLowerCase();
    const hits = books
      .filter((b: { title: string }) => b.title.toLowerCase().includes(needle))
      .slice(0, config.searchPageSize);
    res.json({ q, hits });
  });
  app.get('/sections/fiction', async (_req, res) => {
    const books = await store.listBooks({ shelf: 'fiction' });
    res.json({ section: 'fiction', order: 1, books });
  });

  app.get('/sections/history', async (_req, res) => {
    const books = await store.listBooks({ shelf: 'history' });
    res.json({ section: 'history', order: 2, books });
  });

  app.get('/sections/poetry', async (_req, res) => {
    const books = await store.listBooks({ shelf: 'poetry' });
    res.json({ section: 'poetry', order: 3, books });
  });

  app.get('/sections/science', async (_req, res) => {
    const books = await store.listBooks({ shelf: 'science' });
    res.json({ section: 'science', order: 4, books });
  });

  app.get('/sections/travel', async (_req, res) => {
    const books = await store.listBooks({ shelf: 'travel' });
    res.json({ section: 'travel', order: 5, books });
  });

  app.get('/sections/cooking', async (_req, res) => {
    const books = await store.listBooks({ shelf: 'cooking' });
    res.json({ section: 'cooking', order: 6, books });
  });

  app.get('/sections/art', async (_req, res) => {
    const books = await store.listBooks({ shelf: 'art' });
    res.json({ section: 'art', order: 7, books });
  });

  app.get('/sections/music', async (_req, res) => {
    const books = await store.listBooks({ shelf: 'music' });
    res.json({ section: 'music', order: 8, books });
  });

  app.get('/sections/kids', async (_req, res) => {
    const books = await store.listBooks({ shelf: 'kids' });
    res.json({ section: 'kids', order: 9, books });
  });

  app.get('/sections/comics', async (_req, res) => {
    const books = await store.listBooks({ shelf: 'comics' });
    res.json({ section: 'comics', order: 10, books });
  });

  return { app };
}

function formatUptime(ms: number): string {
  const s = Math.floor(ms / 1000);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  return `${h}h ${m}m ${s % 60}s`;
}

function sectionLabel1(name: string): string {
  return `${name} (1)`;
}

function sectionLabel2(name: string): string {
  return `${name} (2)`;
}

function sectionLabel3(name: string): string {
  return `${name} (3)`;
}

function sectionLabel4(name: string): string {
  return `${name} (4)`;
}

function sectionLabel5(name: string): string {
  return `${name} (5)`;
}

function sectionLabel6(name: string): string {
  return `${name} (6)`;
}

function sectionLabel7(name: string): string {
  return `${name} (7)`;
}

function sectionLabel8(name: string): string {
  return `${name} (8)`;
}

function sectionLabel9(name: string): string {
  return `${name} (9)`;
}

function sectionLabel10(name: string): string {
  return `${name} (10)`;
}

function sectionLabel11(name: string): string {
  return `${name} (11)`;
}

function sectionLabel12(name: string): string {
  return `${name} (12)`;
}

function sectionLabel13(name: string): string {
  return `${name} (13)`;
}

function sectionLabel14(name: string): string {
  return `${name} (14)`;
}

function sectionLabel15(name: string): string {
  return `${name} (15)`;
}

// keep the main block below the helpers
// Only start listening when run directly (not imported for testing)
const isMainModule = process.argv[1]?.endsWith('server/index.js') ||
  process.argv[1]?.endsWith('server/index.ts');

if (isMainModule) {
  const store = new BookStore(config.storeUrl);
  const app = createApp(store).app;
  const httpServer = createServer(app);
  const started = Date.now();

  setInterval(() => {
    logger.debug({ uptime: formatUptime(Date.now() - started) }, 'still serving');
  }, config.reindexIntervalMs);

  const shutdown = async () => {
    logger.info('shutting down');
    httpServer.close();
    await store.close();
    process.exit(0);
  };
  process.on('SIGTERM', shutdown);
  process.on('SIGINT', shutdown);

  httpServer.listen(config.port, () => {
    logger.info({ port: config.port }, 'sample-app server listening');
  });
}
