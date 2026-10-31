package toml_test

import (
	"bytes"
	"testing"

	"github.com/BurntSushi/toml"
)

// Held-out acceptance tests: anonymous (embedded) structs are flattened into the parent
// table at any depth, and every promoted field keeps its own value.

type BenchLevel4 struct {
	Host string
	Port int
	User string
}

type BenchLevel3 struct{ BenchLevel4 }
type BenchLevel2 struct{ BenchLevel3 }
type BenchLevel1 struct{ BenchLevel2 }

type BenchMixedInner struct {
	Alpha string
	Beta  string
	Gamma string
}

type BenchMixedMiddle struct {
	BenchMixedInner
	Delta string
}

type BenchMixedOuter struct {
	BenchMixedMiddle
	Epsilon string
	Zeta    string
}

func benchEncode(t *testing.T, v interface{}) string {
	t.Helper()
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(v); err != nil {
		t.Fatalf("Encode(%T): %v", v, err)
	}
	return buf.String()
}

func TestBenchEncodeFourLevelEmbedding(t *testing.T) {
	in := BenchLevel1{BenchLevel2{BenchLevel3{BenchLevel4{Host: "db.internal", Port: 5432, User: "svc"}}}}
	want := "Host = \"db.internal\"\nPort = 5432\nUser = \"svc\"\n"
	if got := benchEncode(t, in); got != want {
		t.Fatalf("encoded\n%s\nwant\n%s", got, want)
	}
}

func TestBenchEncodeEmbeddingWithSiblingFields(t *testing.T) {
	in := BenchMixedOuter{
		BenchMixedMiddle: BenchMixedMiddle{
			BenchMixedInner: BenchMixedInner{Alpha: "a", Beta: "b", Gamma: "c"},
			Delta:           "d",
		},
		Epsilon: "e",
		Zeta:    "z",
	}
	want := "Alpha = \"a\"\nBeta = \"b\"\nGamma = \"c\"\nDelta = \"d\"\nEpsilon = \"e\"\nZeta = \"z\"\n"
	if got := benchEncode(t, in); got != want {
		t.Fatalf("encoded\n%s\nwant\n%s", got, want)
	}
}

func TestBenchEncodeDeepEmbeddingRoundTrip(t *testing.T) {
	in := BenchLevel1{BenchLevel2{BenchLevel3{BenchLevel4{Host: "h1", Port: 1, User: "u1"}}}}
	var out BenchLevel1
	if _, err := toml.Decode(benchEncode(t, in), &out); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out != in {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}

func TestBenchEncodeDeepEmbeddingInsideTable(t *testing.T) {
	in := struct {
		Server BenchLevel1 `toml:"server"`
	}{BenchLevel1{BenchLevel2{BenchLevel3{BenchLevel4{Host: "h2", Port: 2, User: "u2"}}}}}
	want := "[server]\n  Host = \"h2\"\n  Port = 2\n  User = \"u2\"\n"
	if got := benchEncode(t, in); got != want {
		t.Fatalf("encoded\n%s\nwant\n%s", got, want)
	}
}
