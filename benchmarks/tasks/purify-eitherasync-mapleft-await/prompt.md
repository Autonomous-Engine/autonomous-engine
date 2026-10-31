# `EitherAsync#mapLeft` does not await an asynchronous mapper

Repository: gigobyte/purify (TypeScript).

## Problem

`EitherAsync#mapLeft` is typed as `mapLeft<L2>(f: (value: L) => L2): EitherAsync<Awaited<L2>, R>`,
i.e. it promises that a mapper returning a `Promise` is awaited. The implementation does not
await it, so with an `async` mapper the resulting `Left` holds a pending `Promise` instead of
the mapped value:

```ts
await EitherAsync.liftEither(Left(20)).mapLeft(async (n) => n * 2).run();
// expected Left(40), actual Left(Promise { 40 })
```

## Expected behaviour

- When the mapper returns a promise, `mapLeft` awaits it and the `Left` contains the resolved
  value; later operations (`mapLeft`, `caseOf`, `run`, ...) see that value.
- Synchronous mappers behave exactly as before.
- A `Right` passes through unchanged and the mapper is not called.

## Notes

- The implementation is in `src/EitherAsync.ts`.
- Run the tests with `npx vitest run src/EitherAsync.test.ts` (or the whole suite with
  `npx vitest run`).
