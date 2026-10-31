import { describe, expect, test } from 'vitest'
import { EitherAsync } from './EitherAsync'
import { Left, Right } from './Either'

// Held-out acceptance tests: EitherAsync#mapLeft awaits an asynchronous mapper.

const delay = <T>(ms: number, value: T) =>
  new Promise<T>((resolve) => setTimeout(() => resolve(value), ms))

describe('bench: EitherAsync.mapLeft with async mappers', () => {
  test('an async mapper result is awaited', async () => {
    const result = await EitherAsync.liftEither(Left(20))
      .mapLeft(async (n) => n * 2)
      .run()
    expect(result).toEqual(Left(40))
  })

  test('a mapper returning a delayed promise is awaited', async () => {
    const result = await EitherAsync<string, number>(({ throwE }) => throwE('boom'))
      .mapLeft((e) => delay(5, `wrapped: ${e}`))
      .run()
    expect(result).toEqual(Left('wrapped: boom'))
  })

  test('the awaited left value flows into later operations', async () => {
    const result = await EitherAsync.liftEither(Left('a'))
      .mapLeft(async (s) => s + 'b')
      .mapLeft((s) => s.toUpperCase())
      .run()
    expect(result).toEqual(Left('AB'))
  })

  test('orDefault / caseOf see the awaited value', async () => {
    const value = await EitherAsync.liftEither(Left(1))
      .mapLeft(async (n) => n + 1)
      .caseOf({ Left: (l) => `left ${l}`, Right: (r) => `right ${r}` })
    expect(value).toBe('left 2')
  })

  test('a Right is left untouched and the mapper is not called', async () => {
    let calls = 0
    const result = await EitherAsync.liftEither(Right<number, number>(7))
      .mapLeft(async (n) => {
        calls++
        return n
      })
      .run()
    expect(result).toEqual(Right(7))
    expect(calls).toBe(0)
  })

  test('synchronous mappers behave as before', async () => {
    const result = await EitherAsync.liftEither(Left(3))
      .mapLeft((n) => n + 1)
      .run()
    expect(result).toEqual(Left(4))
  })
})
