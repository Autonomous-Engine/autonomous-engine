# Add a `P.record` pattern

Repository: gvergnaud/ts-pattern (TypeScript).

## Request

ts-pattern has `P.array`, `P.set` and `P.map` for matching every element of a collection, but
nothing for plain objects used as dictionaries (`Record<string, number>` and friends). Add
`P.record`.

## Acceptance criteria

- `P.record(keyPattern, valuePattern)` returns a chainable pattern that matches a value when
  it is a non-null, non-array object and **every** own entry matches: the key against
  `keyPattern` and the value against `valuePattern`. The empty object matches.
- `P.record(valuePattern)` (one argument) is shorthand for `P.record(P.string, valuePattern)`.
- Object keys are strings at runtime, so a numeric-looking key (`"1"`) also matches a
  `P.number` key pattern (by its numeric value); non-numeric keys do not.
- Arrays, `null`, `undefined` and primitives never match.
- Sub-patterns can be any pattern, including guarded ones (`P.string.regex(...)`,
  `P.number.gte(0)`), and `P.record` works nested inside object patterns and with
  `isMatching`.
- Selections inside a record collect one value per entry: `P.record(P.string, P.select())`
  on `{ x: 1, y: 2 }` selects `[1, 2]`; named selections on keys and values give arrays
  (`{ keys: ['x', 'y'], values: [1, 2] }`).
- The handler's input type should narrow to the corresponding `Record<Key, Value>` type,
  consistent with the other collection patterns, and `P.record` needs to be exported from
  `P` and documented in the README next to `P.map`.

## Notes

- The other collection patterns (`array`, `set`, `map`) live in `src/patterns.ts`, with their
  types under `src/types/` (`Pattern.ts`, `InvertPattern.ts`, `FindSelected.ts`).
- The existing `tests/records.test.ts` is about plain object-literal patterns, not this
  feature; you may move or keep it.
- Run the tests with `npx jest` (ts-jest type-checks every test file).
