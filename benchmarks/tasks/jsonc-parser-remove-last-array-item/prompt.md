# `modify()` corrupts the array when removing its last element

Repository: microsoft/node-jsonc-parser (TypeScript).

## Problem

Removing the last element of an array with `modify(text, path, undefined, options)` produces
edits that delete one character too few. When the closing bracket directly follows the last
element, the result merges the previous element with the removed one:

```ts
applyEdits('{"items":[1,2]}', modify('{"items":[1,2]}', ['items', 1], undefined, {}));
// expected '{"items":[1]}', actual '{"items":[12]}'
```

The same happens with string elements (`["1","2"]` becomes `["1""2"]`-like garbage) and with
formatting options enabled.

## Expected behaviour

- Removing the last element of an array removes that element and the separator in front of
  it, and nothing else; the closing bracket and the remaining elements are untouched
  (`[7,8,9]` minus index 2 is `[7,8]`; `[true, false]` minus index 1 is `[true]`).
- This holds with and without `formattingOptions`, for numbers, strings, objects and nested
  arrays, and when there is whitespace before the closing bracket.
- Removing the first or a middle element, or the only element, behaves as before.

## Notes

- The removal logic is in `setProperty` in `src/impl/edit.ts`.
- Build and test with `npx tsc -p ./src && node --test ./lib/esm/test/*.test.js`.
