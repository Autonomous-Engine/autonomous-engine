import assert from 'assert';
import { satisfies } from '../src/index';

// Held-out acceptance tests for tilde, caret and pre-release ranges.

type Case = [string, string, boolean];

const check = (cases: Case[]) => {
  cases.forEach(([version, range, expected]) => {
    it(`satisfies(${version}, ${range}) === ${expected}`, () => {
      assert.strictEqual(satisfies(version, range), expected);
    });
  });
};

describe('bench: tilde ranges', () => {
  check([
    ['2.4.7', '~2.4.1', true],
    ['2.4.1', '~2.4.1', true],
    ['2.4.0', '~2.4.1', false],
    ['2.5.0', '~2.4.1', false],
    ['2.4.9', '~2.4', true],
    ['2.5.0', '~2.4', false],
    ['2.9.9', '~2', true],
    ['2.0.0', '~2', true],
    ['3.0.0', '~2', false],
    ['0.4.2', '~0.4.1', true],
    ['0.5.0', '~0.4.1', false],
    ['0.7.3', '~0', true],
    ['1.0.0', '~0', false],
  ]);
});

describe('bench: caret ranges', () => {
  check([
    ['2.9.0', '^2.3.4', true],
    ['2.3.4', '^2.3.4', true],
    ['2.3.3', '^2.3.4', false],
    ['3.0.0', '^2.3.4', false],
    ['0.4.9', '^0.4.2', true],
    ['0.4.1', '^0.4.2', false],
    ['0.5.0', '^0.4.2', false],
    ['1.4.2', '^0.4.2', false],
    ['0.0.7', '^0.0.7', true],
    ['0.0.8', '^0.0.7', false],
    ['0.1.7', '^0.0.7', false],
    ['2.7.1', '^2.0', true],
    ['3.1.0', '^2.0', false],
  ]);
});

describe('bench: ranges with a pre-release', () => {
  check([
    ['3.1.4-rc.3', '~3.1.4-rc.1', true],
    ['3.1.4-rc.1', '~3.1.4-rc.1', true],
    ['3.1.4-rc.0', '~3.1.4-rc.1', false],
    ['3.1.4-rc.10', '~3.1.4-rc.9', true],
    ['3.1.5-rc.1', '~3.1.4-rc.1', false],
    ['3.1.4-rc.2', '^3.1.4-rc.1', true],
    ['3.2.0-rc.1', '^3.1.4-rc.1', false],
    ['4.0.0', '^3.1.4-rc.1', false],
    ['0.0.5-alpha.3', '^0.0.5-alpha', true],
    ['0.0.6', '^0.0.5-alpha', false],
  ]);
});

describe('bench: other range forms are unchanged', () => {
  check([
    ['1.5.0', '>=1.2.0', true],
    ['1.2.0', '<1.2.0', false],
    ['4.2.1', '4.x.x', true],
    ['4.2.1', '4.3.x', false],
    ['4.2.1', '4.2.1', true],
    ['v2.4.7', '~2.4.1', true],
  ]);
});
