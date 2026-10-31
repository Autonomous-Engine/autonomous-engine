import { expect, test } from 'vitest';
import { title } from './page';

test('title', () => {
  expect(title('x')).toBe('# x');
});
