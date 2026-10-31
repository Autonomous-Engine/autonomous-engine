# Embedded struct is not squashed when both the Squash option and a squash tag are set

Repository: mitchellh/mapstructure (Go).

## Problem

When decoding a struct into a `map[string]interface{}`, an embedded struct can be flattened
("squashed") into the parent map in two ways: by setting `DecoderConfig.Squash = true`, or by
tagging the embedded field with `mapstructure:",squash"`. Each mechanism works on its own,
but when **both** are present the embedded struct is not flattened: its fields end up in a
nested map entry instead of at the top level of the result.

```go
type Address struct{ Street, City string }
type Contact struct {
    Address `mapstructure:",squash"`
    Name    string
}

out := map[string]interface{}{}
dec, _ := mapstructure.NewDecoder(&mapstructure.DecoderConfig{Squash: true, Result: &out})
_ = dec.Decode(Contact{Address{"1 Main St", "Springfield"}, "home"})
// want: map[City:Springfield Name:home Street:1 Main St]
// got:  Street and City are not top-level keys; they end up in a nested map
```

## Expected behaviour

- Struct-to-map decoding squashes an embedded struct (or pointer to struct) whenever the
  `Squash` option is enabled **or** the field carries the `squash` tag option, including when
  both apply.
- Behaviour when only one of the two mechanisms applies, and when neither applies (the
  embedded struct stays nested under its field name), is unchanged.
- Map-to-struct decoding is out of scope.

## Notes

- `go test ./...` must pass.
