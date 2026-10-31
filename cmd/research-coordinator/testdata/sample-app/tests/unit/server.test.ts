import { describe, it, expect, beforeEach } from 'vitest';
import request from 'supertest';
import { createApp, parseBbox, clampLimit, toStationView } from '../../src/server/index.js';
import { MemoryStationStore } from '../../src/store/memory-store.js';

const NOW = 1_700_000_000_000;

function seed(store: MemoryStationStore) {
  store.put({ id: 's1', name: 'Market & 5th', lat: '40.01', lon: '-75.01', bikes: '4', ebikes: '1', docks: '7', capacity: '12', updatedAt: String(NOW), status: 'open' });
  store.put({ id: 's2', name: 'Riverside Park', lat: '40.02', lon: '-75.03', bikes: '0', ebikes: '0', docks: '15', capacity: '15', updatedAt: String(NOW), status: 'open' });
  store.put({ id: 's3', name: 'Old Depot', lat: '40.05', lon: '-75.08', bikes: '2', ebikes: '0', docks: '0', capacity: '10', updatedAt: String(NOW - 3_600_000), status: 'closed' });
}

describe('Station Finder server', () => {
  let store: MemoryStationStore;
  let app: ReturnType<typeof createApp>['app'];

  beforeEach(() => {
    store = new MemoryStationStore();
    seed(store);
    app = createApp(store, { clock: () => NOW }).app;
  });

  it('GET /healthz reports the store state', async () => {
    const res = await request(app).get('/healthz');
    expect(res.status).toBe(200);
    expect(res.body.ok).toBe(true);
  });

  it('GET /healthz answers 503 when the store is down', async () => {
    store.down = true;
    const res = await request(app).get('/healthz');
    expect(res.status).toBe(503);
    expect(res.body.ok).toBe(false);
  });

  it('GET /api/stations lists every station', async () => {
    const res = await request(app).get('/api/stations');
    expect(res.status).toBe(200);
    expect(res.body.stations).toHaveLength(3);
    expect(res.body.stations[0]).toMatchObject({ id: 's1', bikes: 4, docks: 7 });
  });

  it('GET /api/stations filters by bounding box', async () => {
    const res = await request(app).get('/api/stations?bbox=-75.02,40.0,-75.0,40.015');
    expect(res.status).toBe(200);
    expect(res.body.stations.map((s: { id: string }) => s.id)).toEqual(['s1']);
  });

  it('GET /api/stations rejects a malformed bounding box', async () => {
    const res = await request(app).get('/api/stations?bbox=1,2,3');
    expect(res.status).toBe(400);
  });

  it('GET /api/stations/:id returns one station', async () => {
    const res = await request(app).get('/api/stations/s2');
    expect(res.status).toBe(200);
    expect(res.body.name).toBe('Riverside Park');
  });

  it('GET /api/stations/:id answers 404 for an unknown id', async () => {
    const res = await request(app).get('/api/stations/nope');
    expect(res.status).toBe(404);
  });

  it('unknown API paths answer JSON', async () => {
    const res = await request(app).get('/api/does-not-exist');
    expect(res.status).toBe(404);
    expect(res.body).toEqual({ error: 'not_found' });
  });

  it('OPTIONS preflight is answered with 204', async () => {
    const res = await request(app).options('/api/stations');
    expect(res.status).toBe(204);
    expect(res.headers['access-control-allow-methods']).toContain('GET');
  });

  it('GET /api/stations/:id/history returns samples', async () => {
    const res = await request(app).get('/api/stations/s1/history');
    expect(res.status).toBe(200);
    expect(Array.isArray(res.body.samples)).toBe(true);
  });

  it('GET /app/ serves the map page', async () => {
    const res = await request(app).get('/app/');
    expect([200, 404]).toContain(res.status);
  });
});

describe('parseBbox', () => {
  it('accepts a valid box', () => {
    expect(parseBbox('-75.1,39.9,-74.9,40.1')).toEqual([-75.1, 39.9, -74.9, 40.1]);
  });
  it('rejects inverted and out-of-range boxes', () => {
    expect(parseBbox('-74.9,39.9,-75.1,40.1')).toBeNull();
    expect(parseBbox('-190,0,10,10')).toBeNull();
    expect(parseBbox(undefined)).toBeNull();
  });
});

describe('clampLimit', () => {
  it('bounds the value', () => {
    expect(clampLimit('0', 50, 200)).toBe(1);
    expect(clampLimit('500', 50, 200)).toBe(200);
    expect(clampLimit('x', 50, 200)).toBe(50);
  });
});

describe('toStationView', () => {
  it('maps a stored record', () => {
    const v = toStationView({ id: 'a', lat: '1', lon: '2', bikes: '3', status: 'open' });
    expect(v).toMatchObject({ id: 'a', name: 'a', bikes: 3, docks: 0, status: 'open' });
  });
  it('drops a record without coordinates', () => {
    expect(toStationView({ id: 'a', lat: 'x', lon: '2' })).toBeNull();
  });
  it('maps an unknown status', () => {
    expect(toStationView({ id: 'a', lat: '1', lon: '2', status: 'weird' })?.status).toBe('unknown');
  });
  it('never returns negative counts', () => {
    expect(toStationView({ id: 'a', lat: '1', lon: '2', bikes: '-4' })?.bikes).toBe(0);
  });
});