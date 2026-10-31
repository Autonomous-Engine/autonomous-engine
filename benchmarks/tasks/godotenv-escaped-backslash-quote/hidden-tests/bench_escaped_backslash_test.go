package godotenv

import (
	"reflect"
	"strings"
	"testing"
)

// Held-out acceptance tests: a closing quote is escaped only by an odd run of backslashes.

func TestBenchDoubleQuotedValueEndingInEscapedBackslash(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  map[string]string
	}{
		{"escaped backslash before closing quote", `FOO="bar\\"`, map[string]string{"FOO": `bar\`}},
		{"following line is still parsed", "FOO=\"bar\\\\\"\nBAR=baz", map[string]string{"FOO": `bar\`, "BAR": "baz"}},
		{"escaped quote then escaped backslash", `FOO="a\"b\\"`, map[string]string{"FOO": `a"b\`}},
		{"odd backslash run still escapes the quote", `FOO="q\\\" tail"`, map[string]string{"FOO": `q\" tail`}},
		{"two escaped backslashes", `FOO="x\\\\"`, map[string]string{"FOO": `x\\`}},
		{"plain escaped quote unchanged", `FOO="escaped\"bar"`, map[string]string{"FOO": `escaped"bar`}},
		{"newline escape unchanged", `FOO="line1\nline2"`, map[string]string{"FOO": "line1\nline2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Unmarshal(tc.input)
			if err != nil {
				t.Fatalf("Unmarshal(%q) returned error: %v", tc.input, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Unmarshal(%q) = %#v, want %#v", tc.input, got, tc.want)
			}
		})
	}
}

func TestBenchParseReaderWithEscapedBackslashBeforeQuote(t *testing.T) {
	src := "A=\"one\\\\\"\nB=\"two\"\nC=three\n"
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	want := map[string]string{"A": `one\`, "B": "two", "C": "three"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse = %#v, want %#v", got, want)
	}
}
