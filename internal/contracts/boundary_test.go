package contracts

import (
	"context"
	"strings"
	"testing"
)

// A dotted topic value names one topic: it must match where that topic is
// written, and never inside a longer dotted name, before or after it.
func TestAliasRefPatternDottedTopicBoundary(t *testing.T) {
	re := aliasRefPattern("user.created")
	for _, l := range []string{
		`bus.Subscribe("user.created", h)`,
		`bus.on('user.created', h)`,
		"topic := `user.created`",
		`user.created`,
		`topics: [user.created]`,
		`// consumes user.created.`,
	} {
		if !re.MatchString(l) {
			t.Errorf("%q does not match user.created", l)
		}
	}
	for _, l := range []string{
		`bus.Subscribe("foo.user.created", h)`,
		`bus.Subscribe("user.created.v2", h)`,
		`bus.Subscribe("user.created.dlq", h)`,
		`bus.Subscribe("acme.user.created.retry", h)`,
		`bus.Subscribe("xuser.created", h)`,
		`bus.Subscribe("user.createdAt", h)`,
	} {
		if re.MatchString(l) {
			t.Errorf("%q matches user.created", l)
		}
	}
	// Identifier aliases may be package-qualified, but not a longer name.
	id := aliasRefPattern("TopicUserCreated")
	if !id.MatchString(`w.bus.Subscribe(events.TopicUserCreated, h)`) || id.MatchString(`w.bus.Subscribe(events.TopicUserCreatedV2, h)`) {
		t.Error("identifier alias boundary")
	}
}

// A consumer that only subscribes to a different, longer dotted topic is no
// decoding site of user.created: the check stays unchecked rather than
// reporting its field uses as mismatches.
func TestCheckEventLongerDottedTopicIsNotADecodingSite(t *testing.T) {
	ctx := context.Background()
	producer := Side{
		Base: tree(t, map[string]string{"internal/events/topics.go": eventConsts, "internal/events/user.go": userPayload("email", "")}),
		Head: tree(t, map[string]string{"internal/events/topics.go": eventConsts, "internal/events/user.go": userPayload("email_address", "")}),
	}
	for _, topic := range []string{"foo.user.created", "user.created.v2"} {
		t.Run(topic, func(t *testing.T) {
			consumer := Side{Head: tree(t, map[string]string{
				"worker/audit.go": "package worker\n\nfunc (w *Worker) Start() error {\n\treturn w.bus.Subscribe(\"" + topic + "\", func(m map[string]any) { log(m[\"email\"]) })\n}\n",
			})}
			got := Check(ctx, Ref{Type: "event", Name: "user.created"}, producer, consumer)
			if got.Status != StatusUnchecked || !strings.Contains(got.Summary(), "no decoding site") {
				t.Fatalf("status %s: %s, want unchecked (no decoding site)", got.Status, got.Summary())
			}
		})
	}
}

// codexConsumerFile never fails on bad provenance: a missing, escaping or
// empty consumer_file is ignored, and an out-of-range or non-positive line
// falls back to the first line.
func TestCodexConsumerFileBadProvenance(t *testing.T) {
	root := tree(t, map[string]string{"src/user.ts": "line one\nline two\n", "src/empty.ts": ""})
	cases := []struct {
		name     string
		ref      Ref
		root     string
		wantOK   bool
		wantLine int
	}{
		{"no consumer file", Ref{}, root, false, 0},
		{"blank consumer file", Ref{ConsumerFile: "  "}, root, false, 0},
		{"no consumer tree", Ref{ConsumerFile: "src/user.ts", ConsumerLine: 1}, "", false, 0},
		{"missing file", Ref{ConsumerFile: "src/gone.ts", ConsumerLine: 3}, root, false, 0},
		{"directory", Ref{ConsumerFile: "src", ConsumerLine: 1}, root, false, 0},
		{"escapes the tree", Ref{ConsumerFile: "../outside.ts", ConsumerLine: 1}, root, false, 0},
		{"escapes after cleaning", Ref{ConsumerFile: "src/../../outside.ts", ConsumerLine: 1}, root, false, 0},
		{"valid line", Ref{ConsumerFile: "./src/user.ts", ConsumerLine: 2}, root, true, 1},
		{"line past the end", Ref{ConsumerFile: "src/user.ts", ConsumerLine: 999}, root, true, 0},
		{"zero line", Ref{ConsumerFile: "src/user.ts"}, root, true, 0},
		{"negative line", Ref{ConsumerFile: "src/user.ts", ConsumerLine: -4}, root, true, 0},
		{"empty file, line out of range", Ref{ConsumerFile: "src/empty.ts", ConsumerLine: 2}, root, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, line, ok := codexConsumerFile(tc.root, tc.ref)
			if ok != tc.wantOK || line != tc.wantLine {
				t.Fatalf("got (%q, %d, %v), want (%d, %v)", f.rel, line, ok, tc.wantLine, tc.wantOK)
			}
			if ok {
				_ = location(f, line) // must not panic
			}
		})
	}
}

// Through Check: a codex consumer_file that does not exist leaves the
// contract unchecked (no panic, never ok), and an out-of-range line on an
// existing file is reported at a real line.
func TestCheckWithBadCodexConsumerProvenance(t *testing.T) {
	ctx := context.Background()
	restProducer := Side{
		Base: tree(t, map[string]string{"server/routes.go": "package server\n\nfunc routes(r *Router) {\n\tr.GET(\"/api/orders\", listOrders)\n}\n"}),
		Head: tree(t, map[string]string{"server/routes.go": "package server\n\nfunc routes(r *Router) {\n}\n"}),
	}
	eventProducer := Side{
		Base: tree(t, map[string]string{"internal/events/topics.go": eventConsts, "internal/events/user.go": userPayload("email", "")}),
		Head: tree(t, map[string]string{"internal/events/topics.go": eventConsts, "internal/events/user.go": userPayload("email_address", "")}),
	}
	dynamic := map[string]string{"web/orders.js": "export const list = () =>\n  http(['api', 'orders'].join('/'))\n"}
	cases := []struct {
		name     string
		ref      Ref
		producer Side
		consumer map[string]string
		want     Status
		wantText string
	}{
		{"rest: missing consumer file", Ref{Type: "rest", Name: "GET /api/orders", ConsumerFile: "web/gone.js", ConsumerLine: 7},
			restProducer, dynamic, StatusUnchecked, "no call of /api/orders"},
		{"rest: escaping consumer file", Ref{Type: "rest", Name: "GET /api/orders", ConsumerFile: "../web/orders.js", ConsumerLine: 1},
			restProducer, dynamic, StatusUnchecked, "no call of /api/orders"},
		{"rest: line past the end of an existing file", Ref{Type: "rest", Name: "GET /api/orders", ConsumerFile: "web/orders.js", ConsumerLine: 400},
			restProducer, dynamic, StatusMismatch, "web/orders.js:1:"},
		{"rest: empty consumer file with a line", Ref{Type: "rest", Name: "GET /api/orders", ConsumerFile: "web/empty.js", ConsumerLine: 3},
			restProducer, map[string]string{"web/empty.js": ""}, StatusMismatch, "web/empty.js:1:"},
		{"event: missing consumer file", Ref{Type: "event", Name: "user.created", ConsumerFile: "worker/gone.go", ConsumerLine: 12},
			eventProducer, map[string]string{"worker/other.go": "package worker\n"}, StatusUnchecked, "no decoding site"},
		{"event: line past the end of an existing file", Ref{Type: "event", Name: "user.created", ConsumerFile: "worker/user.go", ConsumerLine: 9999},
			eventProducer, map[string]string{"worker/user.go": "package worker\n\nfunc h(m map[string]any) { notify(m[\"email\"]) }\n"}, StatusMismatch, "worker/user.go:3:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Check(ctx, tc.ref, tc.producer, Side{Head: tree(t, tc.consumer)})
			if got.Status != tc.want || !strings.Contains(got.Summary(), tc.wantText) {
				t.Fatalf("status %s: %s; want %s containing %q", got.Status, got.Summary(), tc.want, tc.wantText)
			}
		})
	}
}
