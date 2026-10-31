package modeljson

import (
	"reflect"
	"testing"
)

func TestFirstObject(t *testing.T) {
	obj := `{"a":"x } y \" {","b":[1,{"c":2}]}`
	cases := map[string]struct{ in, want string }{
		"bare":            {obj, obj},
		"trailing brace":  {obj + "}", obj},
		"trailing prose":  {obj + " and that is all {really}.", obj},
		"two objects":     {obj + `{"z":1}`, obj},
		"fenced":          {"```json\n" + obj + "\n```", obj},
		"think stripped":  {StripThinking(`<think>{"draft":true}</think>`) + obj, obj},
		"prose brace":     {"see {the PRD}: " + obj, obj},
		"none":            {"no json here", ""},
		"invalid only":    {`{"a":1,}`, `{"a":1,}`},
		"unterminated":    {`{"a":`, `{"a":`},
		"escaped slash":   {`{"p":"C:\\dir\\"}` + "}", `{"p":"C:\\dir\\"}`},
		"array not taken": {`[1] ` + obj, obj},
		// R6 M4: a brace that never closes no longer hides a later value.
		"stray open brace": {`stray { then {"a":1}`, `{"a":1}`},
		// R6 review: a truncated value is cut-off JSON, not a stray brace:
		// its inner objects must not be returned in its place.
		"truncated object": {`{"criteria":[{"id":"AC-1","text":"x"},{"id":"AC-`, `{"criteria":[{"id":"AC-1","text":"x"},{"id":"AC-`},
	}
	for name, tc := range cases {
		if got := FirstObject(tc.in); got != tc.want {
			t.Errorf("%s: FirstObject = %q, want %q", name, got, tc.want)
		}
	}
}

func TestArrays(t *testing.T) {
	got := Arrays(`Files [1] then [{"t":"a ] b"}]] and ["x"]`)
	want := []string{`[1]`, `[{"t":"a ] b"}]`, `["x"]`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Arrays = %q, want %q", got, want)
	}
}

// A truncated array yields nothing (the caller reports the extraction
// failure and nudges), not its inner array.
func TestArraysTruncated(t *testing.T) {
	if got := Arrays(`Here: [{"title":"a","files":["x.ts"]},{"title":"b","fi`); len(got) != 0 {
		t.Fatalf("Arrays of a truncated array = %q, want none", got)
	}
	if got := Arrays(`stray [ then [1,2]`); len(got) != 1 || got[0] != `[1,2]` {
		t.Fatalf("Arrays after a stray bracket = %q", got)
	}
}

func TestStripThinking(t *testing.T) {
	if got := StripThinking("a<think>b</think>c<think>d"); got != "ac" {
		t.Fatalf("StripThinking = %q", got)
	}
}
