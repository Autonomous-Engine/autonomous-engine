# Double-quoted value ending in an escaped backslash breaks parsing

Repository: joho/godotenv (Go).

## Problem

Inside a double-quoted value, `\\` is an escaped backslash and `\"` is an escaped quote. The
parser decides whether a `"` closes the value by looking only at the single preceding byte, so
a value whose content ends in an escaped backslash, for example

    FOO="bar\\"

is treated as if the closing quote were escaped. The parser keeps scanning past the end of the
line and either swallows the following lines into `FOO` or fails with
`unterminated quoted value`.

## Expected behaviour

- A `"` inside a double-quoted value is escaped only when it is preceded by an odd number of
  consecutive backslashes. An even number of backslashes (including zero) means the quote
  terminates the value.
- `FOO="bar\\"` parses as `FOO` = `bar\`, and a variable on the following line is parsed
  normally.
- Existing escapes keep working: `FOO="escaped\"bar"` is `escaped"bar`; a run of three
  backslashes before a quote still escapes it (`"q\\\" tail"` is `q\" tail`); `\n`/`\r`
  expansion inside double quotes is unchanged.
- Both `Unmarshal` and `Parse` (they share the parser) must behave this way.

## Notes

- The relevant code is the quoted-value terminator scan in `parser.go`.
- Add regression tests in the existing test style.
- `go test ./...` must pass.
