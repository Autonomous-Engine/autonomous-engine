# Feature: nil-safe Version.Equal

Repository: Masterminds/semver (Go), module `github.com/Masterminds/semver/v3`.

## Request

`(*Version).Equal` dereferences both the receiver and the argument, so comparing optional
versions (`*Version` fields that may be nil) panics with a nil pointer dereference and callers
have to guard every comparison by hand. Make `Equal` nil-safe with the semantics below.

## Acceptance criteria

1. `var a, b *Version; a.Equal(b)` returns `true` (two nil versions are equal).
2. A nil `*Version` compared with a non-nil one returns `false` in both directions
   (`nilVersion.Equal(v)` and `v.Equal(nilVersion)`) and never panics.
3. Non-nil comparisons are unchanged: the same version is equal; versions that differ only in
   build metadata are equal; different versions are not.
4. `Compare`, `LessThan` and `GreaterThan` are out of scope; do not change their behaviour.

## Notes

- Extend the existing `TestEqual` table (or add a test) to cover the nil cases.
- `go test ./...` must pass.
