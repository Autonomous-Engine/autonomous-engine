# Int() and Uint() wrap around on out-of-range numbers

Repository: tidwall/gjson (Go).

## Problem

`Result.Int()` and `Result.Uint()` accept JSON numbers, and numeric JSON strings, whose raw
text lies outside the range of `int64`/`uint64`. The manual digit parser used for those values
silently overflows, so callers get wrapped garbage (for example `0`, or a large negative
number) for inputs such as `18446744073709551616`.

## Expected behaviour

Out-of-range integer literals must saturate (clamp) to the nearest representable bound
instead of wrapping:

| JSON value               | `.Uint()`              | `.Int()`               |
|--------------------------|------------------------|------------------------|
| `18446744073709551616`   | `18446744073709551615` | `9223372036854775807`  |
| `99999999999999999999`   | `18446744073709551615` | `9223372036854775807`  |
| `9223372036854775808`    | `9223372036854775808`  | `9223372036854775807`  |
| `-9223372036854775809`   | (unspecified)          | `-9223372036854775808` |

- In-range values, including the exact bounds `18446744073709551615`, `9223372036854775807`
  and `-9223372036854775808`, are still returned exactly (no float rounding).
- Numeric JSON strings holding an integer literal (for example `"18446744073709551616"`)
  follow the same clamping rules as numbers.
- Out-of-range values with a fractional part (for example `18446744073709551617.1`) are out
  of scope: they continue to go through Go's float conversion, whose result is
  implementation-defined.
- The repository's existing tests pin the old wrap-around results for the `overflow_uint64`
  and `overflow_int64` inputs; update those expectations to the clamped values.

## Notes

- The relevant code is `Result.Int`, `Result.Uint`, and the internal `parseInt`/`parseUint`
  helpers in `gjson.go`.
- `go test ./...` must pass.
