# Convert the repository to a Go module

Repository: joho/godotenv (Go).

## Problem

The repository has no `go.mod`. With current Go toolchains (module mode is the default)
`go build ./...` and `go test ./...` fail in a fresh checkout with
`go: go.mod file not found`, and consumers cannot resolve a tagged version through the module
proxy.

## Required change

- Add a `go.mod` declaring the module path `github.com/joho/godotenv` (the import path used by
  the README and by `cmd/godotenv`).
- Use a minimal `go` directive. The package has no third-party dependencies, so no `require`
  block and no `go.sum` are expected.
- The root package and `./cmd/godotenv` must build and test in module mode from a clean clone.

## Notes

- Do not vendor anything, and do not change source files unless the module conversion
  requires it.
- `go test ./...` must pass.
