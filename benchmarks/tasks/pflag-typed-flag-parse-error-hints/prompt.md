# Replace raw stdlib parse errors in the typed flag values

Repository: spf13/pflag (Go).

## Motivation

Every scalar typed flag (`bool`, `int`, `int8`, `int16`, `int32`, `int64`, `uint`, `uint8`,
`uint16`, `uint32`, `uint64`, `float32`, `float64`, `duration`) implements `Value.Set` in its
own file by calling `strconv.Parse*` / `time.ParseDuration` and returning that error verbatim.
End users of CLIs built on pflag therefore see implementation details:

```
invalid argument "x" for "--verbose" flag: strconv.ParseBool: parsing "x": invalid syntax
```

Two further problems come from the same pattern: the errors are inconsistent across types,
and `Set` assigns the parsed value even when parsing failed, so a rejected argument silently
overwrites the flag's default (with `0`, or with the clamped bound for out-of-range input).

## Required change

Across all fourteen scalar flag types listed above:

- When parsing fails (bad syntax or out of range), `Set` returns an error whose message is
  exactly one of these hints, so the user-facing error reads
  `invalid argument "<value>" for "<flag>" flag: <hint>`:

  | flag types                                   | hint                                  |
  |----------------------------------------------|---------------------------------------|
  | bool                                         | `must be true or false`               |
  | int, int8, int16, int32, int64               | `must be an integer`                  |
  | uint, uint8, uint16, uint32, uint64          | `must be a non-negative integer`      |
  | float32, float64                             | `must be a number`                    |
  | duration                                     | `must be a duration like "30s" or "5m"` |

- A failed `Set` leaves the flag's current value unchanged.
- Valid input parses exactly as before (including base prefixes such as `0x10` for integers).
- Slice, map and other composite flag types are out of scope and keep their current errors.
- Update existing tests that pin the old raw error text.

## Notes

- `go test ./...` must pass.
