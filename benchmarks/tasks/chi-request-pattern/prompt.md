# Feature: populate http.Request.Pattern with the matched chi route

Repository: go-chi/chi (Go), module `github.com/go-chi/chi/v5`.

## Request

Go 1.23 added the `Pattern` field to `net/http.Request`; `http.ServeMux` fills it with the
pattern that matched the request, and a growing number of libraries (logging, metrics,
tracing middleware) read it instead of router-specific APIs. chi already records the matched
pattern in its routing context (`chi.RouteContext(r.Context()).RoutePattern()`), but
`r.Pattern` stays empty for requests routed by chi. chi already does the equivalent for
`r.PathValue` (Go 1.22) via `SetPathValue`; do the same for `Pattern`.

## Acceptance criteria

1. When a chi `Mux` routes a request to a handler registered directly on it, the handler sees
   `r.Pattern` equal to the chi route pattern that matched, exactly as registered: for
   example `/orders/{orderID}` for a request to `/orders/77`, `/static/*` for a wildcard
   route, and regexp parameters such as `/v/{id:[0-9]+}` verbatim.
2. For such routes `r.Pattern` equals `chi.RouteContext(r.Context()).RoutePattern()` inside
   the handler, also when the router has middleware.
3. Unmatched requests behave as before (404/405 handling is unchanged).
4. The module must keep building on Go versions older than 1.23, where the field does not
   exist (the module's `go` directive is 1.20), and with TinyGo, following the approach
   chi already uses for the `PathValue` support.
5. Routes inside mounted subrouters (`Mount`, `Route`) are out of scope for this ticket.

## Notes

- `go test ./...` must pass.
