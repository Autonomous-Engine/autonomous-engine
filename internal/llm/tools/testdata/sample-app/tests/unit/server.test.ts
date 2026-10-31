import { describe, it, expect, vi, beforeAll } from 'vitest';
import request from 'supertest';

// Mock the book store before importing the server
vi.mock('../../src/store/books.js', () => {
  const mockStore = {
    getBook: vi.fn(),
    listBooks: vi.fn(),
    listShelves: vi.fn(),
    saveBook: vi.fn(),
    close: vi.fn(),
  };
  return {
    BookStore: vi.fn(() => mockStore),
  };
});

import { createApp } from '../../src/server/index.js';
import { BookStore } from '../../src/store/books.js';

describe('Bookshelf API', () => {
  let app: ReturnType<typeof createApp>['app'];
  let mockStore: any;

  beforeAll(() => {
    mockStore = new BookStore() as any;
    const result = createApp(mockStore);
    app = result.app;
  });

  describe('GET /books/:id', () => {
    it('returns the book', async () => {
      mockStore.getBook.mockResolvedValue({
        id: 'b-101',
        title: 'A Sample Novel',
        shelf: 'fiction',
      });

      const res = await request(app).get('/books/b-101');
      expect(res.status).toBe(200);
      expect(res.body.title).toBe('A Sample Novel');
      expect(res.body.shelf).toBe('fiction');
    });

    it('returns 404 for an unknown book', async () => {
      mockStore.getBook.mockResolvedValue(null);
      const res = await request(app).get('/books/missing');
      expect(res.status).toBe(404);
    });
  });

  describe('GET /books', () => {
    it('returns the book list', async () => {
      mockStore.listBooks.mockResolvedValue([
        { id: 'b-101', title: 'A Sample Novel', shelf: 'fiction' },
        { id: 'b-102', title: 'Another Sample', shelf: 'fiction' },
      ]);

      const res = await request(app).get('/books?shelf=fiction');
      expect(res.status).toBe(200);
      expect(res.body.books).toHaveLength(2);
      expect(res.body.count).toBe(2);
    });
  });

  describe('GET /health', () => {
    it('returns health status', async () => {
      mockStore.listShelves.mockResolvedValue([{ id: 'fiction' }]);

      const res = await request(app).get('/health');
      expect(res.status).toBe(200);
      expect(res.body.status).toBe('ok');
    });
  });

  describe('POST /books', () => {
    it('returns 400 without a title', async () => {
      const res = await request(app).post('/books').send({ author: 'Nobody' });
      expect(res.status).toBe(400);
      expect(res.body.error).toMatch(/title/i);
    });
  });

  describe('GET /search', () => {
    it('returns 400 for an empty query', async () => {
      const res = await request(app).get('/search?q=');
      expect(res.status).toBe(400);
      expect(res.body.error).toMatch(/q is required/);
    });
  });

  describe('GET /shelves', () => {
    it('returns the shelf list', async () => {
      mockStore.listShelves.mockResolvedValue([{ id: 'fiction', name: 'Fiction' }]);
      const res = await request(app).get('/shelves');
      expect(res.status).toBe(200);
      expect(res.body.shelves).toBeInstanceOf(Array);
      expect(res.body.shelves.length).toBeGreaterThan(0);
      expect(res.body.shelves[0]).toHaveProperty('id');
      expect(res.body.shelves[0]).toHaveProperty('name');
    });
  });

  describe('GET /shelves/:id/cover', () => {
    it('returns PNG for a known shelf', async () => {
      const res = await request(app).get('/shelves/fiction/cover');
      expect(res.status).toBe(200);
      expect(res.headers['content-type']).toMatch(/image\/png/);
    });

    it('returns 404 for an unknown shelf', async () => {
      mockStore.listShelves.mockResolvedValue([]);
      const res = await request(app).get('/shelves/nonexistent/cover');
      expect(res.status).toBe(404);
    });
  });

  describe('GET /sections/:name', () => {
    it('returns the section', async () => {
      mockStore.listBooks.mockResolvedValue([]);
      const res = await request(app).get('/sections/fiction');
      expect(res.status).toBe(200);
      expect(res.body.section).toBe('fiction');
    });
  });
});
