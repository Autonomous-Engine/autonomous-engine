# Len() panics on a nil *Error

Repository: hashicorp/go-multierror (Go).

## Problem

`*Error` is routinely nil: `Append` returns a typed nil in some flows, `ErrorOrNil()` exists
precisely so callers can hold a `*Error` that may be nil, and most methods on the type
(`Error()`, `ErrorOrNil()`, `WrappedErrors()`, `Unwrap()`) already tolerate a nil receiver.
`Len()` does not. Calling `Len()` on a nil `*Error` dereferences the receiver and panics with
`invalid memory address or nil pointer dereference`, so code such as

    var merr *multierror.Error
    ...
    if merr.Len() > 0 { ... }

crashes instead of reporting zero errors.

## Expected behaviour

- `Len()` on a nil `*Error` returns `0` and never panics, consistent with `ErrorOrNil()`.
- `Len()` on a non-nil `*Error` still returns the number of collected errors (including `0`
  for an `Error` with no entries).
- `*Error` must keep satisfying `sort.Interface` (`sort.Sort(merr)` keeps working), and
  `Append` followed by `Len()` keeps reporting the appended count.
- Changing the receiver kind of `Len` is acceptable; do not change `Swap`/`Less` semantics.

## Notes

- The relevant code is in `sort.go`.
- Add a regression test.
- `go test ./...` must pass.
