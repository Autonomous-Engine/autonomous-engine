import { describe, it, expect, vi, beforeAll } from 'vitest';
import request from 'supertest';

// Mock ioredis before importing server
vi.mock('ioredis', () => {
  const mockRedis = {
    get: vi.fn(),
    hgetall: vi.fn(),
    smembers: vi.fn(),
    hset: vi.fn(),
    quit: vi.fn(),
  };
  return {
    Redis: vi.fn(() => mockRedis),
  };
});

import { createApp } from '../../src/server/index.js';
import { Redis } from 'ioredis';

describe('Places Server', () => {
  let app: ReturnType<typeof createApp>['app'];
  let mockRedis: any;

  beforeAll(() => {
    mockRedis = new Redis() as any;
    const result = createApp(mockRedis);
    app = result.app;
  });

  describe('GET /places/:id', () => {
    it('returns one place', async () => {
      mockRedis.hgetall.mockResolvedValue({
        name: 'Riverside Library',
        category: 'libraries',
        lat: '51.5072',
        lon: '-0.1276',
      });

      const res = await request(app).get('/places/lib-17');
      expect(res.status).toBe(200);
      expect(res.body.name).toBe('Riverside Library');
      expect(res.body.category).toBe('libraries');
    });

    it('returns 404 for an unknown place', async () => {
      mockRedis.hgetall.mockResolvedValue({});
      const res = await request(app).get('/places/nope');
      expect(res.status).toBe(404);
    });
  });

  describe('GET /places', () => {
    it('lists the places of a category', async () => {
      mockRedis.smembers.mockResolvedValue(['park-1', 'park-2']);
      mockRedis.hgetall
        .mockResolvedValueOnce({ name: 'North Park', lat: '1', lon: '2' })
        .mockResolvedValueOnce({ name: 'South Park', lat: '3', lon: '4' });

      const res = await request(app).get('/places?category=parks');
      expect(res.status).toBe(200);
      expect(res.body.places).toHaveLength(2);
      expect(res.body.places[0].name).toBe('North Park');
    });
  });

  describe('GET /health', () => {
    it('reports ok', async () => {
      mockRedis.hgetall.mockResolvedValue({
        lastImport: String(Date.now()),
        failedImports: '0',
      });
      mockRedis.get.mockResolvedValue('42');

      const res = await request(app).get('/health');
      expect(res.status).toBe(200);
      expect(res.body.status).toBe('ok');
    });
  });

  describe('GET /search', () => {
    it('returns an empty list for no match', async () => {
      mockRedis.smembers.mockResolvedValue([]);

      const res = await request(app).get('/search?q=zzz');
      expect(res.status).toBe(200);
      expect(res.body.results).toEqual([]);
      expect(res.body.query).toBe('zzz');
    });
  });

  describe('GET /search without a query', () => {
    it('returns 400', async () => {
      const res = await request(app).get('/search');
      expect(res.status).toBe(400);
      expect(res.body.error).toMatch(/query/i);
    });
  });

  describe('GET /layers', () => {
    it('lists the layers', async () => {
      const res = await request(app).get('/layers');
      expect(res.status).toBe(200);
      expect(res.body.layers).toBeInstanceOf(Array);
      expect(res.body.layers.length).toBeGreaterThan(0);
      expect(res.body.layers[0]).toHaveProperty('name');
      expect(res.body.layers[0]).toHaveProperty('icon');
    });
  });

  describe('GET /layers/:name/legend', () => {
    it('returns SVG for a known layer', async () => {
      const res = await request(app).get('/layers/parks/legend');
      expect(res.status).toBe(200);
      expect(res.headers['content-type']).toMatch(/image\/svg/);
    });

    it('returns 404 for an unknown layer', async () => {
      const res = await request(app).get('/layers/nonexistent/legend');
      expect(res.status).toBe(404);
    });
  });
});
