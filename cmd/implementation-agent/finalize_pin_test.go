package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
)

// M2-02 (D1 ruling): the evidence base is always the merge-base that
// files_changed was diffed from. When it differs from the pin, one
// drift_detected event records old = pin, new = the base actually used, and
// detail = {head_sha, reason}. Nothing else about finalization changes.

func pinnedInputs(f *finalizeFixture, pin string) finalizeInputs {
	in := f.inputs()
	in.task.RepositoryID = uuid.NewString()
	in.task.BaseSHA = pin
	in.task.BaseRef = "main"
	return in
}

func requireDrift(t *testing.T, sink *recordingSink, in finalizeInputs, pin, base, head string) {
	t.Helper()
	if len(sink.baseEvents) != 1 {
		t.Fatalf("base events = %+v, want exactly one drift_detected", sink.baseEvents)
	}
	ev := sink.baseEvents[0]
	if ev.Kind != snapshot.EventDriftDetected || ev.OldSHA != pin || ev.NewSHA != base ||
		ev.WorkItemID != in.taskID || ev.RepositoryID.String() != in.task.RepositoryID {
		t.Fatalf("drift event = %+v, want old=%s (pin) new=%s (base used)", ev, pin, base)
	}
	var d driftDetail
	if err := json.Unmarshal([]byte(ev.Detail), &d); err != nil {
		t.Fatalf("detail %q is not JSON: %v", ev.Detail, err)
	}
	if d.HeadSHA != head || d.Reason != driftReason {
		t.Fatalf("detail = %+v, want head %s and the drift reason", d, head)
	}
}

func requireEvidenceBase(t *testing.T, sink *recordingSink, f *finalizeFixture, want string) {
	t.Helper()
	msg := sink.published[len(sink.published)-1]
	if msg.BaseSHA != want || msg.Evidence == nil || msg.Evidence.BaseSHA != want {
		t.Fatalf("message base=%s evidence=%+v, want the diffed base %s", msg.BaseSHA, msg.Evidence, want)
	}
}

// The local base branch is the pin: pin used, no event.
func TestFinalizePinned_EqualBaseNoDrift(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	f.commit(t, "feature.txt", "value=2\n", "feat: initial")
	in := pinnedInputs(f, f.baseSHA)

	sink := &recordingSink{}
	if !finalizeImplementation(context.Background(), in, sink) {
		t.Fatalf("finalize failed: %+v", sink.published)
	}
	requireEvidenceBase(t, sink, f, f.baseSHA)
	if len(f.store.inserted) != 1 || f.store.inserted[0].BaseSHA != f.baseSHA {
		t.Fatalf("persisted evidence base = %+v", f.store.inserted)
	}
	if len(sink.baseEvents) != 0 {
		t.Fatalf("drift recorded when the base is the pin: %+v", sink.baseEvents)
	}
}

// The local base is BEHIND the pin (main moved on after the worktree was
// made): the merge-base stays the evidence base and the drift is audited.
func TestFinalizePinned_LocalBehindRecordsDrift(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	gitRun(t, f.worktree, "checkout", "-q", "main")
	pin := f.commit(t, "upstream.txt", "x\n", "upstream: base moved")
	gitRun(t, f.worktree, "checkout", "-q", f.branch)
	head := f.commit(t, "feature.txt", "value=2\n", "feat: initial")

	in := pinnedInputs(f, pin)
	sink := &recordingSink{}
	if !finalizeImplementation(context.Background(), in, sink) {
		t.Fatalf("drift changed the outcome: %+v", sink.published)
	}
	requireEvidenceBase(t, sink, f, f.baseSHA)
	requireDrift(t, sink, in, pin, f.baseSHA, head)
	if msg := sink.published[0]; len(msg.FilesChanged) != 1 || msg.FilesChanged[0] != "feature.txt" {
		t.Fatalf("files_changed = %v, want [feature.txt]", msg.FilesChanged)
	}
}

// The local base is AHEAD of the pin (the pin is an older commit of main):
// same rule — merge-base is the evidence base, drift is audited.
func TestFinalizePinned_LocalAheadRecordsDrift(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	pin := gitOutput(t, f.worktree, "rev-parse", f.baseSHA+"~1")
	head := f.commit(t, "feature.txt", "value=2\n", "feat: initial")

	in := pinnedInputs(f, pin)
	sink := &recordingSink{}
	if !finalizeImplementation(context.Background(), in, sink) {
		t.Fatalf("drift changed the outcome: %+v", sink.published)
	}
	requireEvidenceBase(t, sink, f, f.baseSHA)
	requireDrift(t, sink, in, pin, f.baseSHA, head)
}

// The drift event commits with the outcome; a fallback tier drops it with
// the evidence row (as for any other part of a failed outcome).
func TestFinalizePinned_FallbackTierDropsTheDriftEvent(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	pin := gitOutput(t, f.worktree, "rev-parse", f.baseSHA+"~1")
	f.commit(t, "feature.txt", "value=2\n", "feat: initial")

	in := pinnedInputs(f, pin)
	sink := &recordingSink{failNext: 1}
	if finalizeImplementation(context.Background(), in, sink) {
		t.Fatal("a failed commit reported success")
	}
	if len(sink.attempts) < 2 || len(sink.attempts[0].baseEvents) != 1 {
		t.Fatalf("first attempt did not carry the drift event: %+v", sink.attempts)
	}
	for i, a := range sink.attempts[1:] {
		if len(a.baseEvents) != 0 {
			t.Fatalf("fallback tier %d carries base events: %+v", i+1, a.baseEvents)
		}
	}
	if len(sink.baseEvents) != 0 {
		t.Fatalf("drift committed by a fallback tier: %+v", sink.baseEvents)
	}
}

// A failing outcome carries the drift event too.
func TestFinalizePinned_DriftTravelsWithAFailure(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	pin := gitOutput(t, f.worktree, "rev-parse", f.baseSHA+"~1")
	f.commit(t, "feature.txt", "value=3\n", "feat: breaks the suite")
	in := pinnedInputs(f, pin)
	sink := &recordingSink{}
	if finalizeImplementation(context.Background(), in, sink) {
		t.Fatal("a failing suite finalized as success")
	}
	if len(sink.blocked) != 1 || len(sink.baseEvents) != 1 {
		t.Fatalf("blocked=%v events=%+v, want the drift committed with the failure", sink.blocked, sink.baseEvents)
	}
}

// A worktree whose base could not be read is not drift: skipped with a log.
func TestFinalizePinned_UnknownWorktreeBaseIsNotDrift(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	f.commit(t, "feature.txt", "value=2\n", "feat: initial")
	in := pinnedInputs(f, "0123456789abcdef0123456789abcdef01234567")
	in.baseSHA = ""
	sink := &recordingSink{}
	finalizeImplementation(context.Background(), in, sink)
	if len(sink.baseEvents) != 0 {
		t.Fatalf("drift reported for an unknown worktree base: %+v", sink.baseEvents)
	}
}

type fakePinLookup struct {
	pin   snapshot.Pin
	found bool
	err   error
	calls int
}

func (f *fakePinLookup) Get(_ context.Context, workID, repoID uuid.UUID) (snapshot.Pin, bool, error) {
	f.calls++
	return f.pin, f.found, f.err
}

// A subtask built outside the planner (review fix, dashboard retry) has no
// base_sha on its message: its pin row is looked up and compared, and it is
// not counted as legacy.
func TestFinalize_MessageWithoutPinLooksUpTheSubtaskPin(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	pin := gitOutput(t, f.worktree, "rev-parse", f.baseSHA+"~1")
	head := f.commit(t, "feature.txt", "value=2\n", "feat: initial")
	in := pinnedInputs(f, "")
	lookup := &fakePinLookup{pin: snapshot.Pin{BaseSHA: pin}, found: true}
	in.pins = lookup

	before := snapshot.UnpinnedCount(agentID)
	sink := &recordingSink{}
	finalizeImplementation(context.Background(), in, sink)
	if lookup.calls != 1 {
		t.Fatalf("store lookups = %d, want 1", lookup.calls)
	}
	requireDrift(t, sink, in, pin, f.baseSHA, head)
	if snapshot.UnpinnedCount(agentID) != before {
		t.Fatal("a subtask with a stored pin was counted as legacy-unpinned")
	}
}

func TestFinalize_PinLookupErrorIsNotLegacy(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	f.commit(t, "feature.txt", "value=2\n", "feat: initial")
	in := pinnedInputs(f, "")
	in.pins = &fakePinLookup{err: errors.New("db down")}
	before := snapshot.UnpinnedCount(agentID)
	sink := &recordingSink{}
	finalizeImplementation(context.Background(), in, sink)
	if len(sink.baseEvents) != 0 || snapshot.UnpinnedCount(agentID) != before {
		t.Fatalf("events=%+v count delta=%d, want neither", sink.baseEvents, snapshot.UnpinnedCount(agentID)-before)
	}
}

func TestFinalizeUnpinned_KeepsMergeBaseAndCounts(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	f.commit(t, "feature.txt", "value=2\n", "feat: initial")
	in := f.inputs()
	in.task.RepositoryID = uuid.NewString()
	in.pins = &fakePinLookup{found: false}
	before := snapshot.UnpinnedCount(agentID)
	sink := &recordingSink{}
	finalizeImplementation(context.Background(), in, sink)
	requireEvidenceBase(t, sink, f, f.baseSHA)
	if snapshot.UnpinnedCount(agentID) != before+1 {
		t.Fatal("legacy-unpinned task was not counted")
	}
	if len(sink.baseEvents) != 0 {
		t.Fatalf("unpinned task recorded base events: %+v", sink.baseEvents)
	}
}
