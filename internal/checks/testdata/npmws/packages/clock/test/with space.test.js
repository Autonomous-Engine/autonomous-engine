import { test } from 'node:test';
import assert from 'node:assert/strict';
import { tick } from '../src/tick.js';

test('tick', () => {
  assert.equal(tick(1), 2);
});
