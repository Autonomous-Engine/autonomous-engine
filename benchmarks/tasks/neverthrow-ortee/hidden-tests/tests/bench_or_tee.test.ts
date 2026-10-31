import { describe, expect, it } from 'vitest'

import { err, errAsync, ok, okAsync, Result, ResultAsync } from '../src'

// Held-out acceptance tests for orTee on Result and ResultAsync.

describe('bench: Result.orTee', () => {
  it('passes the error to the side effect and returns the same Err', () => {
    const seen: string[] = []
    const result = err<number, string>('nope').orTee((e) => {
      seen.push(e)
      return 'ignored'
    })
    expect(seen).toEqual(['nope'])
    expect(result.isErr()).toBe(true)
    expect(result._unsafeUnwrapErr()).toBe('nope')
  })

  it('swallows exceptions thrown by the side effect', () => {
    const result = err<number, Error>(new Error('original')).orTee(() => {
      throw new Error('from tee')
    })
    expect(result.isErr()).toBe(true)
    expect((result._unsafeUnwrapErr() as Error).message).toBe('original')
  })

  it('does not call the side effect for Ok', () => {
    let calls = 0
    const result = ok<number, string>(5).orTee(() => {
      calls++
    })
    expect(calls).toBe(0)
    expect(result._unsafeUnwrap()).toBe(5)
  })

  it('composes with the rest of the chain', () => {
    const log: string[] = []
    const result: Result<number, string> = err<number, string>('bad')
      .orTee((e) => log.push(`first ${e}`))
      .mapErr((e) => e.toUpperCase())
      .orTee((e) => log.push(`second ${e}`))
    expect(log).toEqual(['first bad', 'second BAD'])
    expect(result._unsafeUnwrapErr()).toBe('BAD')
  })
})

describe('bench: ResultAsync.orTee', () => {
  it('runs the side effect with the error and resolves to the same Err', async () => {
    const seen: string[] = []
    const result = await errAsync<number, string>('async nope').orTee((e) => {
      seen.push(e)
    })
    expect(seen).toEqual(['async nope'])
    expect(result.isErr()).toBe(true)
    expect(result._unsafeUnwrapErr()).toBe('async nope')
  })

  it('waits for an asynchronous side effect before continuing', async () => {
    const order: string[] = []
    const result = await errAsync<number, string>('e')
      .orTee(async () => {
        await new Promise((r) => setTimeout(r, 10))
        order.push('tee done')
      })
      .mapErr((e) => {
        order.push('mapErr')
        return e
      })
    expect(order).toEqual(['tee done', 'mapErr'])
    expect(result._unsafeUnwrapErr()).toBe('e')
  })

  it('swallows synchronous and asynchronous failures of the side effect', async () => {
    const syncThrow = await errAsync<number, string>('x').orTee(() => {
      throw new Error('boom')
    })
    expect(syncThrow._unsafeUnwrapErr()).toBe('x')

    const asyncReject = await errAsync<number, string>('y').orTee(() => Promise.reject(new Error('boom')))
    expect(asyncReject._unsafeUnwrapErr()).toBe('y')
  })

  it('does not call the side effect for Ok and returns a ResultAsync', async () => {
    let calls = 0
    const chained = okAsync<number, string>(9).orTee(() => {
      calls++
    })
    expect(chained).toBeInstanceOf(ResultAsync)
    const result = await chained
    expect(calls).toBe(0)
    expect(result._unsafeUnwrap()).toBe(9)
  })
})
