# Feature: accept compare functions in cmpopts.SortSlices and cmpopts.SortMaps

Repository: google/go-cmp (Go), package `cmp/cmpopts`.

## Request

`cmpopts.SortSlices` and `cmpopts.SortMaps` take an `interface{}` that must be a less
function of the form `func(T, T) bool`; anything else panics with "invalid less function".
The Go ecosystem has moved to three-way compare functions of the form `func(T, T) int`
(`slices.SortFunc`, `strings.Compare`, `cmp.Compare`), so callers currently have to wrap
every compare function in a less adapter. Accept compare functions directly.

## Acceptance criteria

1. `SortSlices` and `SortMaps` accept either a less function `func(T, T) bool` or a compare
   function `func(T, T) int`. A compare function orders `x` before `y` when
   `compare(x, y) < 0`. For example `cmpopts.SortSlices(strings.Compare)` and
   `cmpopts.SortMaps(strings.Compare)` work.
2. Sorting with a compare function behaves exactly like sorting with the equivalent less
   function, including stability: elements for which the compare function returns `0` keep
   their relative order (as with a less function that is not total).
3. Less functions keep working unchanged.
4. Any other function shape (wrong result type, mismatched parameter types, wrong arity) or a
   nil function still panics when the option is constructed; the panic message may mention
   both accepted forms.
5. Update the doc comments of both options to describe the two accepted forms and the
   requirements on a compare function (deterministic, `compare(x, x) == 0`, transitive; and
   total for `SortMaps`).

## Notes

- Existing tests that expect a compare function such as `strings.Compare` to panic pin the
  old behaviour; update them.
- The function-shape detection lives in `cmp/internal/function`.
- `go test ./...` must pass.
