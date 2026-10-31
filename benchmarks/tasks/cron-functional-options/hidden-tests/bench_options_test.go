package cron

import (
	"testing"
	"time"
)

// Held-out acceptance test for the default parser of New(). It uses only API that exists
// at the base revision, so it compiles there and fails on behaviour. The new option API
// (Option, WithLocation, WithParser, WithPanicLogger, SecondOptional) is exercised in the
// bench_options package, which the fixture's test command runs after this package.

func TestBenchNewDefaultsUseStandardFiveFieldParser(t *testing.T) {
	c := New()
	if _, err := c.AddFunc("*/5 * * * *", func() {}); err != nil {
		t.Fatalf("5-field spec rejected by default parser: %v", err)
	}
	if _, err := c.AddFunc("@hourly", func() {}); err != nil {
		t.Fatalf("descriptor rejected by default parser: %v", err)
	}
	if _, err := c.AddFunc("* * * * * *", func() {}); err == nil {
		t.Fatal("6-field spec accepted by default parser, want an error")
	}
	if c.Location() != time.Local {
		t.Fatalf("default location = %v, want time.Local", c.Location())
	}
}

func TestBenchNewDefaultFiveFieldSemantics(t *testing.T) {
	c := New()
	id, err := c.AddFunc("30 12 * * *", func() {})
	if err != nil {
		t.Fatalf("5-field spec rejected by default parser: %v", err)
	}
	sched := c.Entry(id).Schedule
	if sched == nil {
		t.Fatal("entry has no schedule")
	}
	from := time.Date(2020, time.March, 1, 8, 0, 0, 0, time.Local)
	want := time.Date(2020, time.March, 1, 12, 30, 0, 0, time.Local)
	if got := sched.Next(from); !got.Equal(want) {
		t.Fatalf("\"30 12 * * *\" next after %v = %v, want %v (first field must be minutes)", from, got, want)
	}
}
