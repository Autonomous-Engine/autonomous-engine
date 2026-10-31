# Rename the unreleased do-notation helper `bindMap` to `let`

Repository: gcanti/fp-ts (TypeScript).

## Request

The upcoming 2.13 release adds a do-notation helper that stores a computed value in the
do context. It is currently called `bindMap` (in `Functor` and as an instance-specialised
constant in every data-type module), but the name reads like a variant of `bind`, which it is
not. Before the release, rename it to `let` everywhere, matching the `let` of other
languages' do/for-comprehensions. `bindMap` has never been released, so no deprecated alias
is needed.

## Acceptance criteria

- `src/Functor.ts` exports the generic helper as `let` (same overloads and behaviour as
  today's `bindMap`): `let(F)(name, f)` maps `fa` to `{ ...a, [name]: f(a) }`.
- Every module that currently exports `bindMap` exports `let` instead, derived from its
  `Functor` instance: `Array`, `Either`, `Identity`, `IO`, `IOEither`, `IOOption`,
  `NonEmptyArray`, `Option`, `Reader`, `ReaderEither`, `ReaderTask`, `ReaderTaskEither`,
  `ReadonlyArray`, `ReadonlyNonEmptyArray`, `State`, `StateReaderTaskEither`, `Task`,
  `TaskEither`, `TaskOption`, `Tree`.
- No module exports `bindMap` any more.
- `let` is a reserved word, so it cannot be declared directly with `export const let = ...`;
  find a way to export it under that name while keeping the `@since 2.13.0` doc tag.
- Usage in pipelines is unchanged apart from the name, e.g.
  `pipe(O.some(3), O.bindTo('x'), O.bind('y', ...), O.let('sum', ({ x, y }) => x + y))`.
- Update the existing tests (and the generated module docs under `docs/modules`, if you
  regenerate them) to the new name.

## Notes

- Run the tests with `npx jest --coverage=false test/<Module>` (the repository's Jest config
  enforces 100 % coverage, which only makes sense for a full run).
