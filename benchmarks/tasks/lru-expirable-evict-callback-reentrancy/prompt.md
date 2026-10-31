# expirable.LRU deadlocks when the eviction callback uses the cache

Repository: hashicorp/golang-lru (Go), module `github.com/hashicorp/golang-lru/v2`, package
`expirable`.

## Problem

`expirable.NewLRU(size, onEvict, ttl)` accepts an eviction callback. If that callback calls
back into the same cache (for example `Len`, `Get`, `Peek`, `Keys`, or even `Add`/`Remove`),
the program deadlocks. It happens for explicit removals (`Remove`, `RemoveOldest`, `Purge`,
`Resize`, and an `Add` that evicts because the cache is full) and for entries removed by
the background expiry goroutine. Callbacks that update metrics or refill the cache are a
common pattern, and the non-expirable `lru.Cache` in the same module already supports them.

```go
var c *expirable.LRU[string, int]
c = expirable.NewLRU[string, int](2, func(k string, v int) {
    log.Printf("evicted %s, %d left", k, c.Len()) // hangs forever
}, 0)
c.Add("a", 1)
c.Add("b", 2)
c.Add("c", 3) // evicts "a" -> deadlock
```

## Expected behaviour

- The eviction callback may call any method of the same cache, from every code path that
  evicts or expires entries, without deadlocking.
- For explicit operations (`Add` that evicts, `Remove`, `RemoveOldest`, `Purge`, `Resize`)
  the callback still runs synchronously: it has completed, once per evicted entry, before the
  method returns. For expired entries it runs on the background cleanup goroutine.
- When the callback runs, the evicted entry is already gone from the cache.
- Return values and the rest of the cache semantics (ordering, TTL handling, `Len`, `Keys`,
  `Values`) are unchanged.
- Document the callback's guarantees on `EvictCallback`/`NewLRU`.

## Notes

- `go test ./...` must pass. One upstream test in this package (`TestLoadingExpired`) is
  timing-sensitive on loaded machines; that flake is unrelated to this change.
