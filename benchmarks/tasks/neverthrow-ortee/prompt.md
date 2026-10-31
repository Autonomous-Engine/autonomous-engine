# Add `orTee` to `Result` and `ResultAsync`

Repository: supermacro/neverthrow (TypeScript).

## Request

`andTee` lets callers run a side effect (logging, metrics) on the `Ok` track without changing
the result. There is no equivalent for the error track, so logging an error mid-chain forces
an awkward `mapErr((e) => { log(e); return e })`. Add `orTee`, the error-track counterpart
of `andTee`.

## Acceptance criteria

- `Result<T, E>` gains `orTee(f: (e: E) => unknown): Result<T, E>`:
  - on an `Err`, `f` is called with the error and the same `Err` value is returned;
  - any exception thrown by `f` is ignored (the original `Err` is still returned);
  - on an `Ok`, `f` is not called and the same `Ok` value is returned.
- `ResultAsync<T, E>` gains `orTee(f: (e: E) => unknown): ResultAsync<T, E>` with the same
  semantics; if `f` returns a promise it is awaited before the chain continues, and a
  rejection or synchronous throw from `f` is ignored.
- The return value of `f` never affects the result.
- Declare the method on the `IResult` interface with a doc comment (like `andTee`), and add a
  short section to the README.

## Notes

- See `andTee` in `src/result.ts` and `src/result-async.ts` for the existing pattern.
- Run the tests with `npx vitest run`.
