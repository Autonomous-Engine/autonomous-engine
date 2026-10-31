import { expect, test } from 'vitest';
import { pad } from './format.js';

test('pad', () => {
  expect(pad('x', 3)).toBe('  x');
});
