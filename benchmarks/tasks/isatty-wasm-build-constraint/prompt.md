# Build configuration: support every GOARCH=wasm target

Repository: mattn/go-isatty (Go).

## Problem

The package selects a platform implementation with build constraints. WebAssembly is only
covered when `GOOS=js`; for other WebAssembly targets such as `GOOS=wasip1 GOARCH=wasm` no
implementation file is selected, so `IsTerminal` and `IsCygwinTerminal` are undefined and
every program that imports `go-isatty` (directly or through a logging/colour library) fails
to compile:

```
$ GOOS=wasip1 GOARCH=wasm go vet .
./isatty_others_test.go:12:2: undefined: IsTerminal
```

## Required change

- Adjust the build configuration so that the existing stub implementation (which reports
  "not a terminal") is used for every `GOARCH=wasm` target, whatever the `GOOS`.
- `GOOS=js GOARCH=wasm` and all native targets (Linux, macOS/BSD, Windows, Solaris, Plan 9,
  App Engine) keep their current implementation.
- The package, its tests and its example must type-check for `GOOS=wasip1 GOARCH=wasm` and
  `GOOS=js GOARCH=wasm`.
- No dependency changes are needed; do not change the module's `go.mod`.

## Notes

- `go test ./...` must pass on the host.
