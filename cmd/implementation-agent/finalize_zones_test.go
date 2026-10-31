package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/autonomousengine/autonomous-engine/internal/zones"
	"github.com/google/uuid"
)

type staticZoneSource struct {
	zones []zones.Zone
	err   error
	asked []uuid.UUID
}

func (s *staticZoneSource) ListForRepository(_ context.Context, repoID uuid.UUID) ([]zones.Zone, error) {
	s.asked = append(s.asked, repoID)
	return s.zones, s.err
}

func testZones() []zones.Zone {
	reason := "release freeze"
	return []zones.Zone{
		{Name: "safe", Patterns: []string{"docs/**"}, Policy: zones.PolicyAutonomous, Status: models.ZoneStatusActive},
		{Name: "payments", Patterns: []string{"payments/**"}, Policy: zones.PolicyHumanReview, Status: models.ZoneStatusActive},
		{Name: "restricted", Patterns: []string{"migrations/**"}, Policy: zones.PolicyEscalate, Status: models.ZoneStatusActive},
		{Name: "ledger", Patterns: []string{"ledger/**"}, Policy: zones.PolicyAutonomous, Status: models.ZoneStatusFrozen, FrozenReason: &reason},
	}
}

// Codex zones at finalize (migration 063): the changed paths are matched
// against the repository's zones. human_review → verified evidence flagged
// zone_review_required with the zone names (release gate: human review);
// escalate or a frozen zone → the subtask is blocked before verification,
// nothing is pushed; autonomous → recorded, released.
func TestFinalizeZones(t *testing.T) {
	requireSh(t)
	cases := []struct {
		name      string
		file      string
		wantOK    bool
		wantFlag  bool
		wantBlock string
		wantZones []string
	}{
		{"autonomous zone", "docs/guide.txt", true, false, "", []string{"safe"}},
		{"human_review zone", "payments/charge.go", true, true, "", []string{"payments"}},
		{"escalate zone", "migrations/063_x.up.sql", false, false, "codex zone restricted (policy escalate)", []string{"restricted"}},
		{"frozen zone", "ledger/post.go", false, false, "zone ledger is frozen: release freeze", []string{"ledger"}},
		{"no zone", "other/file.txt", true, false, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFinalizeFixture(t)
			f.commit(t, "feature.txt", "value=2\n", "feat: passes")
			if err := os.MkdirAll(filepath.Join(f.worktree, filepath.Dir(tc.file)), 0o755); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tc.file, "package x\n", "feat: touch "+tc.file)

			in := f.inputs()
			// No security_review_paths: only zones can flag the run.
			p := verifypolicy.Default(uuid.Nil)
			p.SecurityReviewPaths = []string{}
			in.policies = policySource(t, &in, p)
			src := &staticZoneSource{zones: testZones()}
			in.zones = src
			sink := &recordingSink{}
			ok := finalizeImplementation(context.Background(), in, sink)
			if ok != tc.wantOK {
				t.Fatalf("finalize ok = %v, want %v: %+v", ok, tc.wantOK, sink.published)
			}
			if len(src.asked) != 1 || src.asked[0].String() != in.task.RepositoryID {
				t.Fatalf("zones asked for %v, want the task's repository", src.asked)
			}
			ev := publishedEvidence(t, f, sink)
			var names []string
			for _, z := range ev.Zones {
				names = append(names, z.Name)
			}
			if !reflect.DeepEqual(names, tc.wantZones) {
				t.Fatalf("evidence zones = %v, want %v", names, tc.wantZones)
			}
			if tc.wantBlock != "" {
				if ev.Outcome != evidence.OutcomeFailed {
					t.Fatalf("outcome = %s, want failed", ev.Outcome)
				}
				if len(sink.blocked) != 1 || !strings.Contains(sink.blocked[0], tc.wantBlock) {
					t.Fatalf("blocked = %v, want one block naming %q", sink.blocked, tc.wantBlock)
				}
				if pushed := f.remoteSHA(t); pushed != "" {
					t.Fatalf("pushed %s although a zone stopped the subtask", pushed)
				}
				if msg := sink.published[0]; msg.Success || !strings.Contains(msg.Reason, tc.wantBlock) {
					t.Fatalf("message = success %v reason %q", msg.Success, msg.Reason)
				}
				return
			}
			if ev.Outcome != evidence.OutcomeVerified {
				t.Fatalf("outcome = %s, want verified", ev.Outcome)
			}
			d := evidence.ReleaseDecision(ev, evidence.ReleasePolicy{})
			if tc.wantFlag {
				if !reflect.DeepEqual(ev.Flags, []string{evidence.FlagZoneReviewRequired}) || !strings.Contains(ev.Reason, "payments") {
					t.Fatalf("flags = %v reason = %q", ev.Flags, ev.Reason)
				}
				if d.Action != evidence.ActionHumanReview {
					t.Fatalf("decision = %+v, want human_review", d)
				}
				return
			}
			if len(ev.Flags) != 0 || d.Action != evidence.ActionRelease {
				t.Fatalf("flags = %v decision = %+v, want release without flags", ev.Flags, d)
			}
		})
	}
}

// Without a database pool there is no zone store: the interface must be a
// real nil (applyZones' "no zone store" branch), not a nil *zones.Store
// wrapped in a non-nil interface that panics on first use.
func TestNewZoneSourceWithoutPoolIsNil(t *testing.T) {
	if src := newZoneSource(nil); src != nil {
		t.Fatalf("newZoneSource(nil) = %#v, want a nil interface", src)
	}
}

// A zone lookup that fails stops the subtask (fail closed): the run cannot
// prove it stays out of escalate or frozen zones.
func TestFinalizeZonesLookupFailureBlocks(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	f.commit(t, "feature.txt", "value=2\n", "feat: passes")
	in := f.inputs()
	in.policies = policySource(t, &in, verifypolicy.Default(uuid.Nil))
	in.zones = &staticZoneSource{err: errors.New("db down")}
	sink := &recordingSink{}
	if finalizeImplementation(context.Background(), in, sink) {
		t.Fatal("finalize succeeded although zones could not be read")
	}
	if len(sink.blocked) != 1 || !strings.Contains(sink.blocked[0], "codex zones could not be read") {
		t.Fatalf("blocked = %v", sink.blocked)
	}
	if pushed := f.remoteSHA(t); pushed != "" {
		t.Fatalf("pushed %s", pushed)
	}
}
