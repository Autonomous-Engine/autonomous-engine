# Rename the `class*` transformation API to `instance*`

Repository: typestack/class-transformer (TypeScript).

## Request

The library's functions are named after "class" when they actually operate on class
*instances* (`classToPlain` takes an instance, not a class), which keeps confusing users.
Rename the API consistently from `class` to `instance` throughout the code base.

## Acceptance criteria

- Top-level functions exported from the package entry point (`src/index.ts`):
  - `classToPlain` → `instanceToPlain`
  - `plainToClass` → `plainToInstance`
  - `classToClass` → `instanceToInstance`
  Behaviour, overloads (single object / arrays) and options are unchanged.
- `ClassTransformer` methods are renamed the same way (`instanceToPlain`, `plainToInstance`,
  `instanceToInstance`), and every internal caller (`serialize`, `deserialize`,
  `deserializeArray`, the method decorators, ...) uses the new names.
- Method decorators are renamed, with their files renamed to match:
  `TransformClassToPlain` → `TransformInstanceToPlain`,
  `TransformPlainToClass` → `TransformPlainToInstance`,
  `TransformClassToClass` → `TransformInstanceToInstance`
  (`src/decorators/transform-*.decorator.ts`, re-exported from `src/decorators/index.ts`).
- `classToPlain` and `plainToClass` remain exported from the entry point as thin
  deprecated aliases (JSDoc `@deprecated`, pointing at the new names) so existing callers
  keep working. The other old names do not need aliases.
- Doc comments that show usage examples use the new names, and the existing test suite is
  updated to the new API and passes.

## Notes

- Run the tests with `npx jest test/functional`.
