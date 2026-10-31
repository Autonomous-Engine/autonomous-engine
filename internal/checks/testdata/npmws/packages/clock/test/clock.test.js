import { test } from 'node:test';
import assert from 'node:assert/strict';
import { hhmm } from '../src/clock.js';

test('hhmm', () => {
  assert.equal(hhmm(125), '02:05');
});
