# Folded block scalars overflow `lineWidth` when lines cannot be folded

Repository: eemeli/yaml (TypeScript).

## Problem

When `stringify` writes a multi-line string as a block scalar and some line is longer than
`lineWidth`, it picks the *folded* style (`>`) so that the long line can be wrapped at spaces.
If a long line contains no space to break at (for example a long token, hash or URL), folding
cannot shorten it, and the output is a folded block scalar whose lines still exceed
`lineWidth`. For example, with `{ lineWidth: 20 }` the string made of three lines of 30 `a`
characters (each followed by a newline) is written as a `>` block with 30-character lines.

## Expected behaviour

- When the block style is chosen automatically (the default `blockQuote: true`, and the
  scalar is not explicitly typed as a folded block), and folding the value would still leave
  at least one line longer than `lineWidth`, write a literal block scalar (`|`) instead.
- When the value *can* be folded so that every line fits, keep the folded style as today.
- An explicit `blockQuote: 'folded'` (or a scalar whose type is `BLOCK_FOLDED`) still
  produces a folded block, even if it overflows. `blockQuote: 'literal'` is unchanged.
- The output must still parse back to the original string, including for values nested in
  maps and sequences.

## Notes

- The relevant code is the block-scalar branch of `src/stringify/stringifyString.ts` and the
  folding helper in `src/stringify/foldFlowLines.ts`.
- Run the tests with `npx jest --config config/jest.config.js`; the existing suites under
  `tests/doc/` (notably `foldFlowLines.ts` and `stringify.ts`) must keep passing.
