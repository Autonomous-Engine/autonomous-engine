# Implement the "extended quoting" mode for shlex.Split

Repository: google/shlex (Go).

## Request

Product needs `shlex.Split` to support the *extended quoting* mode agreed in design note
DN-0417 ("Shell quoting v2"). In that mode the tokenizer must:

- treat `$'...'` sequences according to the escape table in section 3 of the design note (the
  table deliberately differs from bash for the `\x` and `\u` sequences);
- handle nested backtick groups the way section 4 specifies, including the exception for a
  backtick that immediately follows a non-escaping backslash;
- keep the current behaviour when the mode is off.

Expose the mode through whatever option style you think fits the package, and cover every row
of the section 3 escape table with tests.

## Notes

- DN-0417 is an internal document and is not part of this repository; the sections referenced
  above define the required semantics.
- `go test ./...` must pass.
