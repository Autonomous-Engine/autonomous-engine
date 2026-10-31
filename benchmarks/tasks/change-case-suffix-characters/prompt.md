# Add a `suffixCharacters` option to change-case

Repository: blakeembrey/change-case (TypeScript monorepo; the package is
`packages/change-case`).

## Request

`prefixCharacters` lets callers keep leading characters that would otherwise be stripped
(e.g. `"_"` keeps the underscores in `__typename`). There is no way to do the same for
trailing characters, so identifiers such as `type_` or `class$` lose their suffix. Add a
`suffixCharacters` option that mirrors `prefixCharacters` at the end of the input.

## Acceptance criteria

- `Options` gains `suffixCharacters?: string` (default `""`): the longest run of characters
  at the end of the input that all belong to `suffixCharacters` is kept verbatim after the
  transformed words. Characters from the set that appear elsewhere in the input are treated
  as before (split/stripped).
  - `camelCase("user_id__", { suffixCharacters: "_" })` → `"userId__"`
  - `constantCase("user_id__", { suffixCharacters: "_" })` → `"USER_ID__"`
  - `camelCase("my value$_", { suffixCharacters: "_$" })` → `"myValue$_"`
- Every case function that accepts `Options` honours it: `noCase`, `camelCase`,
  `pascalCase`, `pascalSnakeCase`, `capitalCase`, `constantCase`, `dotCase`, `kebabCase`,
  `pathCase`, `sentenceCase`, `snakeCase`, `trainCase`.
- It combines with `prefixCharacters`: `camelCase("__private_field_", { prefixCharacters: "_", suffixCharacters: "_" })`
  → `"__privateField_"`. A character is never kept twice: if the whole input consists of
  prefix/suffix characters it is returned unchanged (e.g. `"___"`).
- Without the option, behaviour is unchanged (`camelCase("type_")` is still `"type"`).
- Document the option in `packages/change-case/README.md` next to `prefixCharacters`.

## Notes

- The implementation is in `packages/change-case/src/index.ts`.
- Run the tests with `npx vitest run packages/change-case`.
