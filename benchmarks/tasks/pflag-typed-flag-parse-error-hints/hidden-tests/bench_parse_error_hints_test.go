package pflag

import (
	"io/ioutil"
	"strings"
	"testing"
	"time"
)

// Held-out acceptance tests: typed scalar flags report a human-readable hint instead of the
// raw strconv/time parse error, and a failed parse leaves the destination unchanged.

type benchHintCase struct {
	name     string
	register func(f *FlagSet)
	arg      string
	hint     string
}

func benchHintCases() []benchHintCase {
	return []benchHintCase{
		{"b", func(f *FlagSet) { f.Bool("b", true, "") }, "maybe", "must be true or false"},
		{"i", func(f *FlagSet) { f.Int("i", 5, "") }, "five", "must be an integer"},
		{"i8", func(f *FlagSet) { f.Int8("i8", 5, "") }, "300", "must be an integer"},
		{"i16", func(f *FlagSet) { f.Int16("i16", 5, "") }, "1.5", "must be an integer"},
		{"i32", func(f *FlagSet) { f.Int32("i32", 5, "") }, "x", "must be an integer"},
		{"i64", func(f *FlagSet) { f.Int64("i64", 5, "") }, "99999999999999999999", "must be an integer"},
		{"u", func(f *FlagSet) { f.Uint("u", 5, "") }, "-3", "must be a non-negative integer"},
		{"u8", func(f *FlagSet) { f.Uint8("u8", 5, "") }, "256", "must be a non-negative integer"},
		{"u16", func(f *FlagSet) { f.Uint16("u16", 5, "") }, "x", "must be a non-negative integer"},
		{"u32", func(f *FlagSet) { f.Uint32("u32", 5, "") }, "-1", "must be a non-negative integer"},
		{"u64", func(f *FlagSet) { f.Uint64("u64", 5, "") }, "abc", "must be a non-negative integer"},
		{"f32", func(f *FlagSet) { f.Float32("f32", 2.5, "") }, "two", "must be a number"},
		{"f64", func(f *FlagSet) { f.Float64("f64", 2.5, "") }, "1e999", "must be a number"},
		{"d", func(f *FlagSet) { f.Duration("d", time.Minute, "") }, "soon", `must be a duration like "30s" or "5m"`},
	}
}

func TestBenchTypedFlagParseErrorHints(t *testing.T) {
	for _, tc := range benchHintCases() {
		f := NewFlagSet("bench", ContinueOnError)
		f.SetOutput(ioutil.Discard)
		tc.register(f)
		initial := f.Lookup(tc.name).Value.String()
		err := f.Parse([]string{"--" + tc.name + "=" + tc.arg})
		if err == nil {
			t.Errorf("--%s=%s: Parse returned nil error", tc.name, tc.arg)
			continue
		}
		want := `invalid argument "` + tc.arg + `" for "--` + tc.name + `" flag: ` + tc.hint
		if err.Error() != want {
			t.Errorf("--%s=%s:\n got: %s\nwant: %s", tc.name, tc.arg, err.Error(), want)
		}
		if strings.Contains(err.Error(), "strconv.") || strings.Contains(err.Error(), "time:") {
			t.Errorf("--%s=%s: error still leaks the stdlib parse error: %s", tc.name, tc.arg, err.Error())
		}
		if got := f.Lookup(tc.name).Value.String(); got != initial {
			t.Errorf("--%s=%s: value changed to %s after a failed parse, want %s", tc.name, tc.arg, got, initial)
		}
	}
}

func TestBenchParseErrorHintWithShorthandAndSet(t *testing.T) {
	f := NewFlagSet("bench", ContinueOnError)
	f.SetOutput(ioutil.Discard)
	port := f.IntP("port", "p", 8080, "")
	err := f.Parse([]string{"-p", "http"})
	if err == nil {
		t.Fatal("-p http: Parse returned nil error")
	}
	if want := `invalid argument "http" for "-p, --port" flag: must be an integer`; err.Error() != want {
		t.Errorf("-p http:\n got: %s\nwant: %s", err.Error(), want)
	}
	if *port != 8080 {
		t.Errorf("port = %d after failed parse, want 8080", *port)
	}

	if err := f.Set("port", "x1"); err == nil || !strings.HasSuffix(err.Error(), ": must be an integer") {
		t.Errorf("FlagSet.Set(port, x1) error = %v, want a \"must be an integer\" hint", err)
	}
}

func TestBenchValidTypedValuesStillParse(t *testing.T) {
	f := NewFlagSet("bench", ContinueOnError)
	f.SetOutput(ioutil.Discard)
	b := f.Bool("b", false, "")
	i := f.Int("i", 0, "")
	h := f.Int64("h", 0, "")
	u := f.Uint8("u", 0, "")
	x := f.Float64("x", 0, "")
	d := f.Duration("d", 0, "")
	if err := f.Parse([]string{"--b=true", "--i=-7", "--h=0x10", "--u=255", "--x=1.25", "--d=90s"}); err != nil {
		t.Fatalf("Parse(valid values) = %v", err)
	}
	if !*b || *i != -7 || *h != 16 || *u != 255 || *x != 1.25 || *d != 90*time.Second {
		t.Errorf("parsed values = %v %v %v %v %v %v", *b, *i, *h, *u, *x, *d)
	}
}
