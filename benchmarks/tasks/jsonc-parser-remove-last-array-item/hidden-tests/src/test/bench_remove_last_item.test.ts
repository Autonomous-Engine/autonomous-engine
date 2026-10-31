import * as assert from 'node:assert';
import { suite, test } from 'node:test';
import { applyEdits, modify, parse, JSONPath } from '../main.js';

// Held-out acceptance tests: removing the last element of an array keeps the closing bracket
// and every remaining element intact.

function remove(text: string, path: JSONPath, formatted: boolean): string {
	const options = formatted ? { formattingOptions: { insertSpaces: true, tabSize: 2, eol: '\n' } } : {};
	return applyEdits(text, modify(text, path, undefined, options));
}

suite('bench: remove the last array element', () => {
	test('compact number array, no formatting', () => {
		assert.strictEqual(remove('[7,8,9]', [2], false), '[7,8]');
	});

	test('compact string array nested in an object, no formatting', () => {
		assert.strictEqual(remove('{"tags":["a","b"],"n":1}', ['tags', 1], false), '{"tags":["a"],"n":1}');
	});

	test('compact array of objects, no formatting', () => {
		assert.strictEqual(remove('[{"x":1},{"y":2}]', [1], false), '[{"x":1}]');
	});

	test('two-element array down to one, with spaces after commas', () => {
		assert.strictEqual(remove('[true, false]', [1], false), '[true]');
	});

	test('formatted removal yields the expected value', () => {
		for (const [text, path, expected] of [
			['[1,2]', [1], [1]],
			['{"a":[10,20,30]}', ['a', 2], { a: [10, 20] }],
			['{"deep":{"list":["p","q"]}}', ['deep', 'list', 1], { deep: { list: ['p'] } }],
			['[[1,2],[3,4]]', [1], [[1, 2]]],
		] as [string, JSONPath, unknown][]) {
			const result = remove(text, path, true);
			const errors: any[] = [];
			assert.deepStrictEqual(parse(result, errors), expected, `removing ${JSON.stringify(path)} from ${text} gave ${JSON.stringify(result)}`);
			assert.strictEqual(errors.length, 0);
		}
	});

	test('whitespace before the closing bracket is handled', () => {
		const result = remove('[ "x", "y" ]', [1], false);
		assert.deepStrictEqual(parse(result), ['x']);
		assert.ok(result.trim().endsWith(']'));
	});

	test('removing a non-last element is unchanged', () => {
		assert.strictEqual(remove('[1,2,3]', [0], false), '[2,3]');
		assert.strictEqual(remove('[1,2,3]', [1], false), '[1,3]');
	});

	test('removing the only element leaves an empty array', () => {
		assert.deepStrictEqual(parse(remove('{"a":[5]}', ['a', 0], false)), { a: [] });
	});
});
