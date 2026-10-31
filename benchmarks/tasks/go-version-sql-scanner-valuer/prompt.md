# Feature: store Version in SQL columns through database/sql

Repository: hashicorp/go-version (Go).

## Request

Applications persist versions in text columns and want to scan rows directly into
`*version.Version` and pass `*version.Version` as query arguments, without wrapper types.
Implement the standard `database/sql` integration on `Version`.

## Acceptance criteria

1. `*Version` implements `database/sql.Scanner`:
   - `Scan(src interface{})` with a `string` parses the value exactly like `NewVersion`
     (so `"v1.2.3-beta+build"` is accepted) and replaces the receiver's value.
   - `Scan(nil)` returns `nil` and leaves the receiver unchanged (NULL column).
   - Any other source type (for example `int64` or `float64`) returns a non-nil error and
     leaves the receiver unchanged.
   - A string that does not parse returns the parse error.
2. `*Version` implements `database/sql/driver.Valuer`:
   - `Value()` returns the canonical string form produced by `String()` as a `string`
     (for example `Must(NewVersion("v2.0.1")).Value()` yields `"2.0.1"`) with a nil error.
3. A value produced by `Value()` scans back into a version that is `Equal` to the original
   (round-trip).
4. Existing behaviour and the existing test suite are unchanged.

## Notes

- Put the methods in `version.go` next to the existing `encoding.TextMarshaler` support.
- `go test ./...` must pass.
