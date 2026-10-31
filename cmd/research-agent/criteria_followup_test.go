package main

import (
	"context"
	"errors"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A raw provider id that names exactly one call is accepted, and what is
// stored is that call's positional id c<n>, never the raw id: for a file,
// a symbol and a contract citation alike.
func TestUniqueRawProviderIDIsStoredAsPositionalID(t *testing.T) {
	cat := buildEvidenceCatalogue(fixtureCalls())
	roots := criteriaRootsFor([]string{repoFixture(t)})

	f := groundFile(plan.FileRef{Path: "src/components/Contact.tsx", Evidence: plan.EvidenceRef{Kind: plan.EvidenceRepo, Ref: "call_read", Confidence: 0.9}}, cat, roots)
	if f.Evidence.Kind != plan.EvidenceRepo || f.Evidence.Ref != "c1" || f.Evidence.Note != "" {
		t.Fatalf("file citation of the unique raw id call_read: %+v, want repo c1", f.Evidence)
	}
	// Without a tree to stat, a valid citation alone still supports it.
	f = groundFile(plan.FileRef{Path: "src/components/Contact.tsx", Evidence: plan.EvidenceRef{Kind: plan.EvidenceRepo, Ref: "call_read", Confidence: 0.9}}, cat, nil)
	if f.Evidence.Ref != "c1" {
		t.Fatalf("no-roots file citation: %+v, want c1", f.Evidence)
	}
	if s := groundCitation(plan.EvidenceRef{Kind: plan.EvidenceLSP, Ref: "call_sym", Confidence: 0.8}, cat); s.Kind != plan.EvidenceLSP || s.Ref != "c2" {
		t.Fatalf("symbol citation of call_sym: %+v, want lsp c2", s)
	}
	if c := groundCitation(plan.EvidenceRef{Kind: plan.EvidenceCodex, Ref: "  call_mod ", Confidence: 0.7}, cat); c.Kind != plan.EvidenceCodex || c.Ref != "c3" {
		t.Fatalf("contract citation of call_mod: %+v, want codex c3", c)
	}
	// The raw id of a call that is not evidence (errored) is still refused.
	if c := groundCitation(plan.EvidenceRef{Kind: plan.EvidenceRepo, Ref: "call_bad"}, cat); c.Kind != plan.EvidenceHypothesis {
		t.Fatalf("errored call cited by raw id: %+v, want hypothesis", c)
	}
}

// failingBudgetDB fails the budget read; it counts inference rows.
type failingBudgetDB struct{ execs int }

type failingRow struct{}

func (failingRow) Scan(...any) error { return errors.New("connection reset") }

func (d *failingBudgetDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	d.execs++
	return pgconn.CommandTag{}, nil
}
func (d *failingBudgetDB) QueryRow(context.Context, string, ...any) pgx.Row { return failingRow{} }

// The token-budget check fails open: when the inference_logs read fails,
// the budget is not treated as exhausted and the criteria pass runs (and is
// recorded), as in the tool loop.
func TestCriteriaBudgetFailsOpenWhenTheReadFails(t *testing.T) {
	t.Setenv("WORK_ITEM_TOKEN_BUDGET", "1000")
	db := &failingBudgetDB{}
	exhausted, used, budget := criteriaBudgetExhausted(context.Background(), db, uuid.New())
	if exhausted || used != 0 || budget != 1000 {
		t.Fatalf("read error: exhausted=%v used=%d budget=%d, want not exhausted, 0 of 1000", exhausted, used, budget)
	}
	a := &scriptedAdapter{responses: []string{goodCriteria}}
	got, err := runCriteriaPass(context.Background(), db, a, uuid.New(), nil, researchTaskMessage{TaskID: "t"}, nil, "", fixtureCalls())
	if err != nil || got == nil || len(got.Criteria) == 0 {
		t.Fatalf("criteria pass after a failed budget read: %+v, %v; want criteria", got, err)
	}
	if len(a.requests) != 1 || db.execs != 1 {
		t.Fatalf("requests=%d inference rows=%d, want the pass to run once and be recorded", len(a.requests), db.execs)
	}
}
