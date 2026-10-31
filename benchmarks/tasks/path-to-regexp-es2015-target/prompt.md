# Build path-to-regexp for ES2015 runtimes

Repository: pillarjs/path-to-regexp (TypeScript).

## Request

path-to-regexp is used by routers that still run on old JavaScript engines (embedded
runtimes, older browsers via bundlers that do not transpile `node_modules`). The package is
currently compiled with `target`/`lib` `ES2020`, and the regular expressions it generates use
the ES2018 `s` (dotAll) flag, which ES2015 engines reject with a `SyntaxError`. Make the
package ES2015-compatible.

## Acceptance criteria

- The project's TypeScript configuration targets ES2015 and type-checks against the ES2015
  standard library only (`target` and `lib` are both ES2015), and `tsc -p tsconfig.json`
  succeeds with that configuration.
- Regular expressions created by `pathToRegexp` / `match` no longer use the `s` flag: the
  flags are `"i"` by default and `""` with `{ sensitive: true }`.
- Matching behaviour is unchanged. In particular wildcard parameters (`*name`) must still
  match any characters within a segment, including line terminators such as `\n` and `\r`.
- Prefer source changes that keep the emitted code small and idiomatic for ES2015 (for
  example, avoid constructs that TypeScript has to down-level with helper code).

## Notes

- The configuration is in `tsconfig.json`; the implementation is `src/index.ts`.
- Run the tests with `npx vitest run src/index.spec` (`src/cases.spec.ts` is a data module,
  not a test file).
