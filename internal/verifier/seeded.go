package verifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// Seeded-defect fixtures (cmd/verifier-eval): one directory per fixture
// under testdata/seeded/ holding
//
//	fixture.json  the criteria, the task spec, the author's summary (fed only
//	              to the author-only baseline), and where the defect is
//	base/         the tree of the base commit
//	head/         files written over base for the head commit (fixture.json
//	              "delete" lists files the head removes)
//
// Materialize turns one into a real git repository with a base and a head
// commit, the same shape review-agent verifies.

// SeededDir is the corpus location relative to the repository root.
const SeededDir = "internal/verifier/testdata/seeded"

// Fixture is one seeded-defect case.
type Fixture struct {
	Dir           string           `json:"-"`
	Name          string           `json:"name"`
	DefectClass   string           `json:"defect_class"`
	Criteria      []plan.Criterion `json:"criteria"`
	TaskSpec      plan.TaskSpec    `json:"task_spec"`
	AuthorSummary string           `json:"author_summary"`
	Delete        []string         `json:"delete,omitempty"`
	Expect        FixtureExpect    `json:"expect"`
}

// FixtureExpect says what a detection looks like.
type FixtureExpect struct {
	// CriterionIDs are criteria the defect breaks (a not_met on any counts).
	CriterionIDs []string `json:"criterion_ids"`
	// File (repository-relative) and Line locate the seeded defect.
	File string `json:"file"`
	Line int    `json:"line,omitempty"`
}

// LoadFixtures reads every fixture directory under root, sorted by name.
func LoadFixtures(root string) ([]Fixture, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []Fixture
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "fixture.json"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		var f Fixture
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			return nil, fmt.Errorf("%s/fixture.json: %w", e.Name(), err)
		}
		f.Dir = dir
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Validate checks the fixture's own consistency (not the repository).
func (f Fixture) Validate() error {
	var errs []error
	if f.Name == "" || f.DefectClass == "" {
		errs = append(errs, errors.New("name and defect_class are required"))
	}
	if len(f.Criteria) == 0 {
		errs = append(errs, errors.New("no criteria"))
	}
	ids := map[string]bool{}
	for _, c := range f.Criteria {
		if err := c.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("criterion %s: %w", c.ID, err))
		}
		ids[c.ID] = true
	}
	if err := f.TaskSpec.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("task_spec: %w", err))
	}
	for _, id := range f.TaskSpec.CriterionIDs {
		if !ids[id] {
			errs = append(errs, fmt.Errorf("task_spec cites unknown %s", id))
		}
	}
	for _, id := range f.Expect.CriterionIDs {
		if !ids[id] {
			errs = append(errs, fmt.Errorf("expect cites unknown %s", id))
		}
	}
	if f.Expect.File == "" {
		errs = append(errs, errors.New("expect.file is required"))
	}
	if strings.TrimSpace(f.AuthorSummary) == "" {
		errs = append(errs, errors.New("author_summary is required (the baseline's PR description)"))
	}
	return errors.Join(errs...)
}

// FixtureRepo is a materialized fixture.
type FixtureRepo struct {
	Path    string
	BaseSHA string
	HeadSHA string
}

// Materialize builds the fixture's git repository under dir (which must
// not exist or be empty): base committed, then head written over it.
func (f Fixture) Materialize(ctx context.Context, dir string) (FixtureRepo, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return FixtureRepo{}, err
	}
	g := func(args ...string) (string, error) {
		full := append([]string{"-c", "user.name=verifier-fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.autocrlf=false", "-c", "commit.gpgsign=false"}, args...)
		return git(ctx, dir, full...)
	}
	if _, err := g("init", "-q"); err != nil {
		return FixtureRepo{}, err
	}
	if err := copyTree(filepath.Join(f.Dir, "base"), dir); err != nil {
		return FixtureRepo{}, fmt.Errorf("copy base: %w", err)
	}
	if _, err := g("add", "-A"); err != nil {
		return FixtureRepo{}, err
	}
	if _, err := g("commit", "-q", "-m", "base"); err != nil {
		return FixtureRepo{}, err
	}
	base, err := g("rev-parse", "HEAD")
	if err != nil {
		return FixtureRepo{}, err
	}
	if err := copyTree(filepath.Join(f.Dir, "head"), dir); err != nil {
		return FixtureRepo{}, fmt.Errorf("copy head: %w", err)
	}
	for _, d := range f.Delete {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(d))); err != nil {
			return FixtureRepo{}, fmt.Errorf("delete %s: %w", d, err)
		}
	}
	if _, err := g("add", "-A"); err != nil {
		return FixtureRepo{}, err
	}
	if _, err := g("commit", "-q", "-m", "head: "+f.Name); err != nil {
		return FixtureRepo{}, err
	}
	head, err := g("rev-parse", "HEAD")
	if err != nil {
		return FixtureRepo{}, err
	}
	return FixtureRepo{Path: dir, BaseSHA: base, HeadSHA: head}, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		// Fixtures are checked out with the platform's line endings; the
		// repository a verifier sees has LF, as a real one would.
		data = []byte(strings.ReplaceAll(string(data), "\r\n", "\n"))
		return os.WriteFile(target, data, 0o644)
	})
}

// Detected reports whether a verdict caught the seeded defect: the verifier
// judged the change (no error, no escalation of its own, no criterion it
// could not verify — that is "could not tell") AND either an expected
// criterion is not_met or a critical/major defect names the expected file.
// It does not depend on the gate's fix-round bounds (R16): a security
// defect or an oversized verdict escalates to a human, but it was found.
func (f Fixture) Detected(v Verdict) bool {
	if v.Error != "" || v.Overall == OverallEscalate {
		return false
	}
	gaps := recordedGapIDs(v)
	for _, r := range v.PerCriterion {
		if r.Status == StatusNotVerifiable && !gaps[r.ID] {
			return false
		}
	}
	want := map[string]bool{}
	for _, id := range f.Expect.CriterionIDs {
		want[id] = true
	}
	for _, r := range v.PerCriterion {
		if want[r.ID] && r.Status == StatusNotMet {
			return true
		}
	}
	for _, d := range v.Defects {
		if (d.Severity == SeverityCritical || d.Severity == SeverityMajor) && SameFile(d.File, f.Expect.File) {
			return true
		}
	}
	return false
}

// SameFile compares repository-relative paths, tolerating a leading "./"
// or "/" and backslashes.
func SameFile(a, b string) bool {
	norm := func(s string) string {
		s = strings.ReplaceAll(strings.TrimSpace(s), `\`, "/")
		s = strings.TrimPrefix(s, "./")
		s = strings.TrimPrefix(s, "/")
		return path.Clean(s)
	}
	return a != "" && b != "" && norm(a) == norm(b)
}
