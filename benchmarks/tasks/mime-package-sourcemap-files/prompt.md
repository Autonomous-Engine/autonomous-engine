# Published package ships source maps that point at files it does not include

Repository: broofa/mime (TypeScript).

## Problem

The build (`tsc`) emits `.js.map` source maps next to every file in `dist/src` and
`dist/types`, and they are published. Each map refers back to the original TypeScript
(`../../src/index.ts`, `../../types/standard.ts`, ...), but `package.json#files` only
publishes `dist/src/*`, `dist/types/*` and `bin/*`. Consumers whose bundlers or debuggers
load source maps get "failed to load source map / missing source" warnings for every module
of the package.

## Expected behaviour

- Every source referenced by a published source map is itself part of the published
  package, i.e. the TypeScript sources under `src/` and the type tables under `types/` are
  published alongside `dist/`.
- Everything that `package.json` references (`main`, every `exports` target, `bin`) is still
  published.
- Tests, build scripts (`scripts/`, `dist/scripts/`), `tsconfig.json` and other development
  files are still not published.
- No change to the runtime code or to the build output.

## Notes

- "Published" means what `npm pack` would put in the tarball; you can inspect it with
  `npm pack --dry-run --json` after building with `npx tsc`.
- Run the tests with `npx tsc && node --test --test-skip-pattern=CLI` (the CLI tests spawn
  `bin/cli.js` through a POSIX shell and are not part of this task).
