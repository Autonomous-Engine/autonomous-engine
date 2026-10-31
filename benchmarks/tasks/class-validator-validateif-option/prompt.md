# Add a per-decorator `validateIf` validation option

Repository: typestack/class-validator (TypeScript).

## Request

`@ValidateIf(condition)` switches off *all* validators on a property. Users need finer
control: a single decorator should be able to apply conditionally while the others on the
same property keep running. Add a `validateIf` option to the common validation options that
every decorator accepts.

## Acceptance criteria

- `ValidationOptions` gains `validateIf?: (object: any, value: any) => boolean`, documented
  like the other options.
- When a decorator carries `validateIf`, the condition is evaluated with the object being
  validated and the property's value. If it returns `false`, that decorator's constraint is
  skipped (no error); if it returns `true`, the constraint is validated as usual.
- It only affects the decorator it is attached to; other decorators on the same property,
  and the property-level `@ValidateIf`, behave as before.
- It works for built-in decorators (`@Min`, `@Max`, `@Length`, `@IsString`, ...), for
  `each: true`, for custom decorators created with `registerDecorator` (their `options`), and
  for both `validate()` and `validateSync()`.
- `isValidationOptions()` recognises an object that only sets `validateIf` as validation
  options.
- Add a README section explaining the option and how it differs from `@ValidateIf`.

Example: with `@Min(10, { validateIf: o => o.mode !== 'max-only' })` and
`@Max(2, { validateIf: o => o.mode !== 'min-only' })` on `amount = 5`, mode `'min-only'`
reports only `min`, mode `'max-only'` only `max`, and any other mode both.

## Notes

- Relevant code: `src/decorator/ValidationOptions.ts`, `src/metadata/ValidationMetadata.ts`,
  `src/validation/ValidationExecutor.ts`.
- Run the tests with `npx jest test/functional` (ts-jest type-checks the specs).
