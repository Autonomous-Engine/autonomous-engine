import * as A from '../src/Array'
import * as E from '../src/Either'
import { pipe } from '../src/function'
import * as Fu from '../src/Functor'
import * as I from '../src/Identity'
import * as IO from '../src/IO'
import * as IOE from '../src/IOEither'
import * as IOO from '../src/IOOption'
import * as NEA from '../src/NonEmptyArray'
import * as O from '../src/Option'
import * as R from '../src/Reader'
import * as RE from '../src/ReaderEither'
import * as RT from '../src/ReaderTask'
import * as RTE from '../src/ReaderTaskEither'
import * as RA from '../src/ReadonlyArray'
import * as RNEA from '../src/ReadonlyNonEmptyArray'
import * as S from '../src/State'
import * as SRTE from '../src/StateReaderTaskEither'
import * as T from '../src/Task'
import * as TE from '../src/TaskEither'
import * as TO from '../src/TaskOption'
import * as Tr from '../src/Tree'

// Held-out acceptance tests: the do-notation helper is exported as `let` (and no longer as
// `bindMap`) from Functor and every data-type module. Modules are reached through loosely
// typed handles so this file compiles whichever name exists.

const ENV = { env: true }
const STATE = 0

type Runner = (fa: any) => unknown
const plain: Runner = (fa) => fa
const thunk: Runner = (fa) => fa()
const reader: Runner = (fa) => fa(ENV)
const readerThunk: Runner = (fa) => fa(ENV)()
const state: Runner = (fa) => fa(STATE)
const stateReaderThunk: Runner = (fa) => fa(STATE)(ENV)()

const modules: Array<[string, any, Runner]> = [
  ['Array', A, plain],
  ['Either', E, plain],
  ['Identity', I, plain],
  ['IO', IO, thunk],
  ['IOEither', IOE, thunk],
  ['IOOption', IOO, thunk],
  ['NonEmptyArray', NEA, plain],
  ['Option', O, plain],
  ['Reader', R, reader],
  ['ReaderEither', RE, reader],
  ['ReaderTask', RT, readerThunk],
  ['ReaderTaskEither', RTE, readerThunk],
  ['ReadonlyArray', RA, plain],
  ['ReadonlyNonEmptyArray', RNEA, plain],
  ['State', S, state],
  ['StateReaderTaskEither', SRTE, stateReaderThunk],
  ['Task', T, thunk],
  ['TaskEither', TE, thunk],
  ['TaskOption', TO, thunk],
  ['Tree', Tr, plain]
]

describe('bench: do-notation `let`', () => {
  for (const [name, M, run] of modules) {
    it(`${name}.let adds a computed field to the do context`, async () => {
      expect(typeof M.let).toBe('function')
      const program = pipe(
        M.Do ?? M.of({}),
        M.let('a', () => 1),
        M.let('b', ({ a }: { a: number }) => a + 1)
      )
      const actual = await run(program)
      const expected = await run(M.of({ a: 1, b: 2 }))
      expect(actual).toEqual(expected)
    })

    it(`${name} no longer exports bindMap`, () => {
      expect(M.bindMap).toBeUndefined()
    })
  }

  it('Functor.let derives the helper from any Functor instance', () => {
    const F: any = Fu
    expect(typeof F.let).toBe('function')
    expect(F.bindMap).toBeUndefined()
    const letO = F.let(O.Functor)
    expect(pipe(O.some({ a: 2 }), letO('double', ({ a }: { a: number }) => a * 2))).toEqual(O.some({ a: 2, double: 4 }))
    expect(pipe(O.none, letO('double', () => 0))).toEqual(O.none)
  })

  it('let composes with bind and bindTo', () => {
    const OO: any = O
    expect(
      pipe(
        O.some(3),
        O.bindTo('x'),
        O.bind('y', ({ x }) => O.some(x * 10)),
        OO.let('sum', ({ x, y }: { x: number; y: number }) => x + y)
      )
    ).toEqual(O.some({ x: 3, y: 30, sum: 33 }))
  })
})
