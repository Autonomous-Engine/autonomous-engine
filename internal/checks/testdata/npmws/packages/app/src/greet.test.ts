import { describe, expect, it } from 'vitest';
import { greet } from './greet';

describe('greet', () => {
  it('shouts through the workspace library', () => {
    expect(greet('ws')).toBe('HELLO WS! 2');
  });
});
