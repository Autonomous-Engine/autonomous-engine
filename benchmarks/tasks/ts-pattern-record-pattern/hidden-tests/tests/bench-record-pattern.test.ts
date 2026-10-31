import { isMatching, match, P } from '../src';

// Held-out acceptance tests for the runtime behaviour of P.record. The pattern is reached
// through a loosely typed handle so this file also compiles before P.record exists.
const record: (...args: unknown[]) => any = (...args) => (P as any).record(...args);

const classify = (value: unknown) =>
  match(value)
    .with(record(P.string, P.number), () => 'numbers')
    .with(record(P.string, P.string), () => 'strings')
    .otherwise(() => 'other');

describe('bench: P.record', () => {
  it('matches objects whose every entry matches the key and value patterns', () => {
    expect(classify({ a: 1, b: 2 })).toBe('numbers');
    expect(classify({ a: 'x', b: 'y' })).toBe('strings');
    expect(classify({ a: 1, b: 'y' })).toBe('other');
  });

  it('matches the empty object', () => {
    expect(isMatching(record(P.string, P.number), {})).toBe(true);
  });

  it('does not match arrays, null or primitives', () => {
    for (const value of [[1, 2], null, undefined, 42, 'str', true]) {
      expect(isMatching(record(P.string, P.any), value)).toBe(false);
    }
    expect(classify([1, 2, 3])).toBe('other');
  });

  it('defaults the key pattern to strings when given a single value pattern', () => {
    expect(isMatching(record(P.boolean), { on: true, off: false })).toBe(true);
    expect(isMatching(record(P.boolean), { on: true, off: 0 })).toBe(false);
  });

  it('matches numeric keys against number patterns', () => {
    expect(isMatching(record(P.number, P.string), { 1: 'one', 2: 'two' })).toBe(true);
    expect(isMatching(record(P.number, P.string), { one: 'one' })).toBe(false);
    expect(isMatching(record(P.string, P.string), { 1: 'one' })).toBe(true);
  });

  it('supports guarded key and value sub-patterns', () => {
    const lowerKeysPositive = record(P.string.regex(/^[a-z]+$/), P.number.gte(0));
    expect(isMatching(lowerKeysPositive, { abc: 1, def: 0 })).toBe(true);
    expect(isMatching(lowerKeysPositive, { abc: 1, Def: 0 })).toBe(false);
    expect(isMatching(lowerKeysPositive, { abc: -1 })).toBe(false);
  });

  it('works nested inside object patterns', () => {
    const result = match({ kind: 'scores', scores: { ann: 3, bob: 5 } } as unknown)
      .with({ kind: 'scores', scores: record(P.string, P.number) }, (v: any) => Object.keys(v.scores).length)
      .otherwise(() => -1);
    expect(result).toBe(2);
  });

  it('collects selections from every entry', () => {
    const values = match({ x: 1, y: 2 } as unknown)
      .with(record(P.string, P.select()), (selected: unknown) => selected)
      .otherwise(() => null);
    expect(values).toEqual([1, 2]);

    const named = match({ x: 1, y: 2 } as unknown)
      .with(record(P.select('keys'), P.select('values')), (selected: unknown) => selected)
      .otherwise(() => null);
    expect(named).toEqual({ keys: ['x', 'y'], values: [1, 2] });
  });
});
