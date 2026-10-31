package bench_options_test

import (
	"io/ioutil"
	"log"
	"testing"
	"time"

	cron "github.com/robfig/cron/v3"
)

// Held-out acceptance tests for the functional-options API. This package references the
// new identifiers, so it only compiles once they exist.

func TestBenchWithLocation(t *testing.T) {
	loc := time.FixedZone("bench", 3*3600)
	c := cron.New(cron.WithLocation(loc))
	if c.Location() != loc {
		t.Fatalf("Location() = %v, want %v", c.Location(), loc)
	}
}

func TestBenchWithParserEnablesSeconds(t *testing.T) {
	c := cron.New(cron.WithParser(cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)))
	if _, err := c.AddFunc("*/10 * * * * *", func() {}); err != nil {
		t.Fatalf("6-field spec rejected by seconds parser: %v", err)
	}
	if _, err := c.AddFunc("* * * * *", func() {}); err == nil {
		t.Fatal("5-field spec accepted by a parser that requires seconds, want an error")
	}
	if _, err := c.AddJob("@every 1h", cron.FuncJob(func() {})); err != nil {
		t.Fatalf("AddJob with descriptor failed: %v", err)
	}
	if got := len(c.Entries()); got != 2 {
		t.Fatalf("Entries() = %d, want 2", got)
	}
}

func TestBenchWithParserCustomSchedule(t *testing.T) {
	c := cron.New(cron.WithParser(cron.NewParser(cron.Descriptor)))
	if _, err := c.AddFunc("@daily", func() {}); err != nil {
		t.Fatalf("descriptor rejected: %v", err)
	}
	if _, err := c.AddFunc("0 0 * * *", func() {}); err == nil {
		t.Fatal("field spec accepted by a descriptor-only parser, want an error")
	}
}

func TestBenchSecondOptional(t *testing.T) {
	p := cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	five, err := p.Parse("0 12 * * *")
	if err != nil {
		t.Fatalf("5-field spec rejected with SecondOptional: %v", err)
	}
	six, err := p.Parse("0 0 12 * * *")
	if err != nil {
		t.Fatalf("6-field spec rejected with SecondOptional: %v", err)
	}
	base := time.Date(2020, time.January, 1, 8, 0, 0, 0, time.UTC)
	if a, b := five.Next(base), six.Next(base); !a.Equal(b) {
		t.Fatalf("5- and 6-field forms of the same schedule differ: %v vs %v", a, b)
	}
	if _, err := p.Parse("0 0 0 12 * * *"); err == nil {
		t.Fatal("7-field spec accepted, want an error")
	}
}

func TestBenchMultipleOptionalsPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewParser with SecondOptional|DowOptional did not panic")
		}
	}()
	cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.DowOptional)
}

func TestBenchOptionsCompose(t *testing.T) {
	opts := []cron.Option{
		cron.WithLocation(time.UTC),
		cron.WithParser(cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)),
		cron.WithPanicLogger(log.New(ioutil.Discard, "", 0)),
	}
	c := cron.New(opts...)
	if c.Location() != time.UTC {
		t.Fatalf("Location() = %v, want UTC", c.Location())
	}
	if _, err := c.AddFunc("0 30 8 * * *", func() {}); err != nil {
		t.Fatalf("composed options lost the parser: %v", err)
	}
}
