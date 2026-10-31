package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
)

const (
	pinA = "7af0d7084cd8b7c8a96b50da4311b560bebcb8e8"
	pinB = "0123456789abcdef0123456789abcdef01234567"
)

type fakePin struct {
	pin   snapshot.Pin
	found bool
	err   error
}

func (f fakePin) Get(context.Context, uuid.UUID, uuid.UUID) (snapshot.Pin, bool, error) {
	return f.pin, f.found, f.err
}

func TestResolveReviewBase_MessagePinIsTheDiffBase(t *testing.T) {
	rb, err := resolveReviewBase(context.Background(), reviewBaseDeps{
		pins:    fakePin{pin: snapshot.Pin{BaseSHA: pinB}, found: true},
		present: func(context.Context, string) bool { return true },
	}, uuid.New(), uuid.NewString(), pinA, "main")
	if err != nil || rb.Ref != pinA || rb.SHA != pinA {
		t.Fatalf("rb=%+v err=%v, want the message pin %s", rb, err, pinA)
	}
}

func TestResolveReviewBase_PinRowWhenTheMessageHasNone(t *testing.T) {
	rb, err := resolveReviewBase(context.Background(), reviewBaseDeps{
		pins:    fakePin{pin: snapshot.Pin{BaseSHA: pinB}, found: true},
		present: func(context.Context, string) bool { return true },
	}, uuid.New(), uuid.NewString(), "", "main")
	if err != nil || rb.Ref != pinB {
		t.Fatalf("rb=%+v err=%v", rb, err)
	}
}

func TestResolveReviewBase_LegacyKeepsOriginBranch(t *testing.T) {
	before := snapshot.UnpinnedCount(agentID)
	rb, err := resolveReviewBase(context.Background(), reviewBaseDeps{pins: fakePin{}}, uuid.New(), uuid.NewString(), "", "develop")
	if err != nil || rb.Ref != "origin/develop" || rb.SHA != "" {
		t.Fatalf("rb=%+v err=%v", rb, err)
	}
	if snapshot.UnpinnedCount(agentID) != before+1 {
		t.Fatal("legacy review must be counted as unpinned")
	}
	// A pin read error is not legacy, but the review still has a base.
	rb, err = resolveReviewBase(context.Background(), reviewBaseDeps{pins: fakePin{err: errors.New("db")}}, uuid.New(), uuid.NewString(), "", "main")
	if err != nil || rb.Ref != "origin/main" {
		t.Fatalf("rb=%+v err=%v", rb, err)
	}
}

func TestResolveReviewBase_FetchesAMissingPinAndBlocksWhenStillMissing(t *testing.T) {
	fetched := ""
	have := false
	deps := reviewBaseDeps{
		present: func(context.Context, string) bool { return have },
		fetch: func(_ context.Context, ref string) error {
			fetched = ref
			have = true
			return nil
		},
	}
	rb, err := resolveReviewBase(context.Background(), deps, uuid.New(), "r", pinA, "main")
	if err != nil || rb.Ref != pinA || fetched != "main" {
		t.Fatalf("rb=%+v err=%v fetched=%q", rb, err, fetched)
	}
	deps.fetch = func(context.Context, string) error { return errors.New("auth failed") }
	have = false
	_, err = resolveReviewBase(context.Background(), deps, uuid.New(), "r", pinA, "main")
	if err == nil || !strings.Contains(err.Error(), "not in the local clone") || !strings.Contains(err.Error(), "auth failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestBaseBranchLine_StatesThePinnedBase(t *testing.T) {
	line := baseBranchLine("main", reviewBase{Ref: pinA, SHA: pinA}, nil)
	if line != "Base branch: main — base 7af0d7084cd8 (pinned)" {
		t.Fatalf("line = %q", line)
	}
	if got := baseBranchLine("main", reviewBase{Ref: "origin/main"}, nil); got != "Base branch: main" {
		t.Fatalf("legacy line = %q", got)
	}
	drift := baseBranchLine("main", reviewBase{Ref: pinA, SHA: pinA}, &baseDrift{Old: pinA, New: pinB, Clean: true})
	if !strings.Contains(drift, "moved from 7af0d7084cd8 to 0123456789ab") || !strings.Contains(drift, "merges cleanly") {
		t.Fatalf("drift line = %q", drift)
	}
}

func TestPRReady_DecodesBaseSHAAndDrift(t *testing.T) {
	var m prReadyMessage
	raw := `{"work_id":"w","base_sha":"` + pinA + `","base_drift":{"old":"` + pinA + `","new":"` + pinB + `","clean":true}}`
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if m.BaseSHA != pinA || m.BaseDrift == nil || m.BaseDrift.New != pinB || !m.BaseDrift.Clean {
		t.Fatalf("decoded %+v", m)
	}
}
