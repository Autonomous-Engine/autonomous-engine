# Allow header advertises OPTIONS even when automatic OPTIONS handling is disabled

Repository: julienschmidt/httprouter (Go).

## Problem

When a request uses a method that has no handler for a path, the router answers
`405 Method Not Allowed` with an `Allow` header listing the methods that are registered for
that path. The list always includes `OPTIONS`, even when `Router.HandleOPTIONS` is `false`
and the router will therefore not answer OPTIONS requests itself. A client that trusts the
header sends an OPTIONS request and receives another 405 whose `Allow` header again lists
`OPTIONS`.

The same list is cached for server-wide handling (`OPTIONS *`).

## Expected behaviour

- With `HandleOPTIONS == false`, the `Allow` header on a 405 response contains only the
  methods that have handlers registered for that path (sorted, comma-separated, as today) and
  never `OPTIONS`.
- With `HandleOPTIONS == true` (the default) behaviour is unchanged: `OPTIONS` is appended to
  the list and automatic OPTIONS responses keep working.
- The server-wide list (the `Allow` value used for `*`) follows the same rule.

## Notes

- The relevant code is `Router.allowed` in `router.go`.
- Add a regression test.
- `go test ./...` must pass.
