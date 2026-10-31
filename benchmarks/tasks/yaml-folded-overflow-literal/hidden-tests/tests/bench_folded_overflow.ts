import { parse, stringify } from 'yaml'

// Held-out acceptance tests: block scalars fall back to literal style when
// folding cannot keep the lines within lineWidth.

const unbreakable = ('a'.repeat(30) + '\n').repeat(3)

describe('bench: folded block scalar overflow', () => {
  test('unbreakable lines longer than lineWidth are written as a literal block', () => {
    const out = stringify(unbreakable, { lineWidth: 20 })
    expect(out.startsWith('|')).toBe(true)
    expect(out).not.toMatch(/^>/)
    expect(parse(out)).toBe(unbreakable)
  })

  test('literal fallback keeps every content line intact', () => {
    const out = stringify(unbreakable, { lineWidth: 20 })
    const lines = out.split('\n').filter(line => line.trim() !== '' && line !== '|')
    expect(lines).toEqual(['a'.repeat(30), 'a'.repeat(30), 'a'.repeat(30)])
  })

  test('a single unbreakable line among foldable ones still selects literal', () => {
    const value = 'one two three four five six seven\n' + 'x'.repeat(40) + '\nshort\n'
    const out = stringify({ key: value }, { lineWidth: 20 })
    expect(out.startsWith('key: |')).toBe(true)
    expect(parse(out)).toEqual({ key: value })
  })

  test('text that can be folded within lineWidth keeps the folded style', () => {
    const value = 'alpha beta gamma delta epsilon zeta eta theta iota\nkappa lambda\n'
    const out = stringify({ key: value }, { lineWidth: 20 })
    expect(out.startsWith('key: >')).toBe(true)
    for (const line of out.split('\n')) expect(line.length).toBeLessThanOrEqual(20)
  })

  test("blockQuote: 'folded' still forces the folded style", () => {
    const out = stringify(unbreakable, { lineWidth: 20, blockQuote: 'folded' })
    expect(out.startsWith('>')).toBe(true)
    expect(parse(out)).toBe(unbreakable)
  })

  test("blockQuote: 'literal' is unaffected", () => {
    const out = stringify(unbreakable, { lineWidth: 20, blockQuote: 'literal' })
    expect(out.startsWith('|')).toBe(true)
    expect(parse(out)).toBe(unbreakable)
  })

  test('nested values round-trip with the fallback', () => {
    const doc = { list: [unbreakable, 'plain'], nested: { deep: unbreakable } }
    const out = stringify(doc, { lineWidth: 20 })
    expect(out).not.toMatch(/: >|- >/)
    expect(parse(out)).toEqual(doc)
  })
})
