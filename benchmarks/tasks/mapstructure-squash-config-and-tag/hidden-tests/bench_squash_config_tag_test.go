package mapstructure

import (
	"reflect"
	"testing"
)

// Held-out acceptance tests: struct -> map decoding must squash an embedded struct when
// DecoderConfig.Squash is enabled, whether or not the field also carries a ",squash" tag.

type BenchAddress struct {
	Street string
	City   string
}

type benchTaggedEmbed struct {
	BenchAddress `mapstructure:",squash"`
	Name         string
}

type benchTaggedPtrEmbed struct {
	*BenchAddress `mapstructure:",squash"`
	Name          string
}

type benchUntaggedEmbed struct {
	BenchAddress
	Name string
}

func benchStructToMap(t *testing.T, squash bool, input interface{}) map[string]interface{} {
	t.Helper()
	out := map[string]interface{}{}
	dec, err := NewDecoder(&DecoderConfig{Squash: squash, Result: &out})
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if err := dec.Decode(input); err != nil {
		t.Fatalf("Decode(%T): %v", input, err)
	}
	return out
}

func benchWantFlat(t *testing.T, label string, got map[string]interface{}) {
	t.Helper()
	want := map[string]interface{}{"Street": "1 Main St", "City": "Springfield", "Name": "home"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: decoded map = %#v, want %#v", label, got, want)
	}
}

func TestBenchSquashConfigWithSquashTag(t *testing.T) {
	in := benchTaggedEmbed{BenchAddress: BenchAddress{Street: "1 Main St", City: "Springfield"}, Name: "home"}
	benchWantFlat(t, "Squash=true, tagged embed", benchStructToMap(t, true, in))
}

func TestBenchSquashConfigWithSquashTagPointer(t *testing.T) {
	in := benchTaggedPtrEmbed{BenchAddress: &BenchAddress{Street: "1 Main St", City: "Springfield"}, Name: "home"}
	benchWantFlat(t, "Squash=true, tagged pointer embed", benchStructToMap(t, true, in))
}

func TestBenchSquashTagOrConfigAloneStillSquash(t *testing.T) {
	tagged := benchTaggedEmbed{BenchAddress: BenchAddress{Street: "1 Main St", City: "Springfield"}, Name: "home"}
	benchWantFlat(t, "Squash=false, tagged embed", benchStructToMap(t, false, tagged))

	untagged := benchUntaggedEmbed{BenchAddress: BenchAddress{Street: "1 Main St", City: "Springfield"}, Name: "home"}
	benchWantFlat(t, "Squash=true, untagged embed", benchStructToMap(t, true, untagged))
}

func TestBenchNoSquashKeepsNestedStruct(t *testing.T) {
	untagged := benchUntaggedEmbed{BenchAddress: BenchAddress{Street: "1 Main St", City: "Springfield"}, Name: "home"}
	got := benchStructToMap(t, false, untagged)
	if _, ok := got["Street"]; ok {
		t.Errorf("Squash=false, untagged embed: field was squashed: %#v", got)
	}
	if _, ok := got["BenchAddress"]; !ok {
		t.Errorf("Squash=false, untagged embed: nested key missing: %#v", got)
	}
}
