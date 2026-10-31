Fixture for internal/checks JSImpact: an npm workspace whose root test
script delegates to three packages — `@ws/lib` (jest, CommonJS), `@ws/app`
(vitest, TypeScript, depends on `@ws/lib`, has a lint script) and
`@ws/clock` (node:test, ESM).
