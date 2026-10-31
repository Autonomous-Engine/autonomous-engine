# Encoder writes the wrong keys for structs embedded several levels deep

Repository: BurntSushi/toml (Go).

## Problem

The encoder flattens anonymous (embedded) struct fields into the parent table, the way
`encoding/json` does. With one or two levels of embedding this works, but once a struct is
embedded three or more levels deep the output is corrupted: promoted fields are written under
the wrong names or values, typically the last field repeated several times.

```go
type Conn struct {
    Host string
    Port int
    User string
}
type L3 struct{ Conn }
type L2 struct{ L3 }
type L1 struct{ L2 }

toml.NewEncoder(os.Stdout).Encode(L1{L2{L3{Conn{"db.internal", 5432, "svc"}}}})
```

Expected:

```toml
Host = "db.internal"
Port = 5432
User = "svc"
```

Actual output repeats one key (`User = "svc"` three times), which is also invalid TOML that
the package's own decoder rejects with a duplicate-key error. The same corruption happens when
the deeply embedded struct is itself the value of a table field.

## Expected behaviour

- Embedded structs are flattened correctly at any nesting depth: every promoted field is
  written exactly once, under its own key, with its own value, in declaration order.
- Output for structs with zero, one or two levels of embedding is unchanged.
- Encoding must behave the same on 32-bit and 64-bit platforms.

## Notes

- The encoder lives in `encode.go`.
- `go test ./...` must pass.
