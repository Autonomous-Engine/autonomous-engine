# Resolve user-defined basic types in every conversion family

Repository: spf13/cast (Go).

## Motivation

The conversion functions switch on concrete types (`int`, `string`, `bool`, `float64`, ...).
A value whose type is *defined* over one of those basic types, such as

```go
type Port int
type Mode string
type Enabled bool
```

falls through to the default case and fails: `cast.ToIntE(Port(8080))` returns
`unable to cast 8080 of type main.Port to int`, and `cast.ToStringE(Mode("fast"))`,
`cast.ToBoolE(Enabled(true))` and `cast.ToDurationE(Mode("90s"))` fail the same way. This
is a common source of bug reports from configuration libraries that use named types.

The conversion families live in separate files (`basic.go` for bool/string, `number.go` for
the generic signed/unsigned/float helpers, `time.go` for durations). Fix this once, in a
shared helper, and route each family's fallback through it, rather than adding a case per
named type.

## Required behaviour

1. A value whose type is a defined type with an underlying kind of `bool`, `string`, any
   signed or unsigned integer, `float32` or `float64` is converted exactly as its underlying
   value would be by: `ToBool[E]`, `ToString[E]`, every integer and float conversion
   (`ToInt[E]` ... `ToUint64[E]`, `ToFloat32[E]`, `ToFloat64[E]`, and the generic helpers built
   on them), and `ToDuration[E]`. This includes the existing rules for that underlying value
   (for example a negative value still fails for unsigned targets, and an integer converts to
   a duration in nanoseconds).
2. Pointers to such values are indirected first, as for other pointers.
3. Types that already have explicit handling keep it: a type implementing `fmt.Stringer` or
   `error` still converts to its `String()`/`Error()` text in `ToString`, and `time.Duration`,
   `json.Number` and similar named types behave as before.
4. Defined types over non-basic kinds (structs, slices, maps, ...) still return an error.
5. Add tests for the new behaviour; the existing tests must keep passing.

## Notes

- `go test ./...` must pass.
