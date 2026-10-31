// Package zones owns codex zones (migration 063; codex coherence design,
// Part 2): per-repository sets of path globs with a policy that decides
// whether agents may change matching files autonomously, only with a human
// approving the PR, or not at all without an escalation.
//
// Globs have exactly the semantics of verifypolicy.MatchPath (the
// security_review_paths matcher), so a pattern means the same thing in a
// zone and in a verification policy. A zone pattern may also be an
// exclusion, "!glob": a file it matches is outside the zone even when
// another pattern matches (the defaults keep *.example files such as
// .env.example out of the restricted zone).
//
// Where zones are enforced:
//   - research: AffectedZones are the zones the affected files and modules
//     fall in (MatchModule / Match);
//   - planner: each task carries the zones its files fall in; a task in a
//     frozen zone is not planned and the item escalates;
//   - implementation finalize: changed paths are matched; human_review sets
//     evidence.FlagZoneReviewRequired, escalate (or a frozen zone) blocks the
//     subtask with an escalation before verification;
//   - pr-coordinator: the PR body lists the zones touched and their policy.
//
// Every write through Store records a codex version with section
// "section:zones" for the zone's product and its repository's branch, so
// work planned against the old zones can be re-planned.
package zones

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/google/uuid"
)

// Policy is what a zone allows (code_zones.policy, enum zone_policy).
type Policy string

const (
	// PolicyAutonomous: agents may change matching files with no extra gate.
	PolicyAutonomous Policy = "autonomous"
	// PolicyHumanReview: the change may be made, but a human must approve
	// the PR (evidence.FlagZoneReviewRequired → human_review), whatever
	// HUMAN_REVIEW_REQUIRED says.
	PolicyHumanReview Policy = "human_review"
	// PolicyEscalate: a change to matching files stops the subtask with an
	// escalation before verification.
	PolicyEscalate Policy = "escalate"
)

// Valid reports whether p is a known policy.
func (p Policy) Valid() bool {
	switch p {
	case PolicyAutonomous, PolicyHumanReview, PolicyEscalate:
		return true
	}
	return false
}

// rank orders policies by strictness.
func (p Policy) rank() int {
	switch p {
	case PolicyEscalate:
		return 2
	case PolicyHumanReview:
		return 1
	}
	return 0
}

// Source records which surface wrote a zone (code_zones.source).
type Source string

const (
	// SourceConfig: a default from configs/codex-config.yaml.
	SourceConfig Source = "config"
	// SourceOnboarding: derived from the onboarding risk_zones answer or
	// confirmed in the onboarding wizard.
	SourceOnboarding Source = "onboarding"
	// SourceDashboard: created or edited in Admin → Products → Repository →
	// Zones.
	SourceDashboard Source = "dashboard"
)

// Valid reports whether s is a known source.
func (s Source) Valid() bool {
	switch s {
	case SourceConfig, SourceOnboarding, SourceDashboard:
		return true
	}
	return false
}

// Zone is one code_zones row.
type Zone struct {
	ID           uuid.UUID `json:"id"`
	RepositoryID uuid.UUID `json:"repository_id"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	// Patterns are repository-relative globs (verifypolicy.MatchPath).
	Patterns []string `json:"patterns"`
	// ModulePaths is derived from Patterns on every write (DeriveModulePaths)
	// so the module-name lookup keeps working; never edited directly.
	ModulePaths  []string          `json:"module_paths"`
	Policy       Policy            `json:"policy"`
	Status       models.ZoneStatus `json:"status"`
	FrozenReason *string           `json:"frozen_reason,omitempty"`
	FrozenAt     *time.Time        `json:"frozen_at,omitempty"`
	Source       Source            `json:"source"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// Frozen reports whether the zone stops new work (status frozen).
func (z Zone) Frozen() bool { return z.Status == models.ZoneStatusFrozen }

// FrozenText is "zone X is frozen: reason" (the planner's escalation
// sentence).
func (z Zone) FrozenText() string {
	reason := ""
	if z.FrozenReason != nil {
		reason = strings.TrimSpace(*z.FrozenReason)
	}
	if reason == "" {
		reason = "no reason given"
	}
	return fmt.Sprintf("zone %s is frozen: %s", z.Name, reason)
}

// MatchPath is verifypolicy.MatchPath: zones and security_review_paths use
// one glob dialect.
func MatchPath(pattern, file string) bool { return verifypolicy.MatchPath(pattern, file) }

// Matches reports whether file falls in the zone: some pattern matches it
// and no exclusion does. A pattern starting with "!" is an exclusion
// ("!**/*.example" keeps .env.example out of a secrets zone); the rest of
// it is an ordinary MatchPath glob.
func (z Zone) Matches(file string) bool {
	// Patterns are anchored to the repository root: an absolute path (a
	// clone path in front) would let "infra/**" match any file of a
	// repository cloned under /srv/infra/. Callers make paths relative
	// with RepoRelative first.
	if isAbsolute(file) {
		return false
	}
	hit := false
	for _, p := range z.Patterns {
		if excl, ok := exclusion(p); ok {
			if MatchPath(excl, file) {
				return false
			}
			continue
		}
		if !hit && MatchPath(p, file) {
			hit = true
		}
	}
	return hit
}

// isAbsolute reports a rooted path in either separator, a Windows drive
// path included.
func isAbsolute(p string) bool {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	if strings.HasPrefix(p, "/") {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && p[2] == '/' &&
		((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z'))
}

// RepoRelative returns file relative to the repository whose clone is at
// root: an absolute path under root loses the root prefix; a relative path
// is kept (a leading "./" dropped); an absolute path outside root — or any
// absolute path when root is empty — is not this repository's and ok is
// false. Separators are normalised to "/".
func RepoRelative(file, root string) (string, bool) {
	f := strings.ReplaceAll(strings.TrimSpace(file), "\\", "/")
	if !isAbsolute(f) {
		return strings.TrimPrefix(f, "./"), f != ""
	}
	r := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(root), "\\", "/"), "/")
	if r == "" {
		return "", false
	}
	if strings.HasPrefix(strings.ToLower(f), strings.ToLower(r)+"/") {
		rel := f[len(r)+1:]
		return rel, rel != ""
	}
	return "", false
}

// exclusion returns the glob of an exclusion pattern ("!glob").
func exclusion(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "!") {
		return strings.TrimSpace(p[1:]), true
	}
	return "", false
}

// MatchesModule reports whether a codex module name falls in the zone: it
// is one of the derived module paths, or (module names are usually
// directory paths) a pattern matches it or a file directly under it.
func (z Zone) MatchesModule(module string) bool {
	m := normPath(module)
	if m == "" {
		return false
	}
	for _, mp := range z.ModulePaths {
		if strings.EqualFold(normPath(mp), m) {
			return true
		}
	}
	return z.Matches(m) || z.Matches(m+"/x")
}

// Hit is one zone some of the files fall in.
type Hit struct {
	Zone  Zone
	Files []string
}

// Match returns the zones files fall in, by zone name, each with its
// matching files (sorted, distinct).
func Match(zs []Zone, files []string) []Hit {
	var out []Hit
	for _, z := range zs {
		seen := map[string]bool{}
		var matched []string
		for _, f := range files {
			if f = strings.TrimSpace(f); f == "" || seen[f] {
				continue
			}
			if z.Matches(f) {
				seen[f] = true
				matched = append(matched, f)
			}
		}
		if len(matched) > 0 {
			sort.Strings(matched)
			out = append(out, Hit{Zone: z, Files: matched})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Zone.Name < out[j].Zone.Name })
	return out
}

// Strictest is the strictest policy among hits (autonomous when none).
func Strictest(hits []Hit) Policy {
	best := PolicyAutonomous
	for _, h := range hits {
		if h.Zone.Policy.rank() > best.rank() {
			best = h.Zone.Policy
		}
	}
	return best
}

// WithPolicy filters hits to those whose zone has policy p.
func WithPolicy(hits []Hit, p Policy) []Hit {
	var out []Hit
	for _, h := range hits {
		if h.Zone.Policy == p {
			out = append(out, h)
		}
	}
	return out
}

// FrozenHits filters hits to frozen zones.
func FrozenHits(hits []Hit) []Hit {
	var out []Hit
	for _, h := range hits {
		if h.Zone.Frozen() {
			out = append(out, h)
		}
	}
	return out
}

// Names are the zone names of hits, in order.
func Names(hits []Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Zone.Name)
	}
	return out
}

// Ref is a zone as carried on messages and evidence: its name, policy and
// status, and the files that fell in it.
type Ref struct {
	Name   string   `json:"name"`
	Policy Policy   `json:"policy"`
	Status string   `json:"status,omitempty"`
	Files  []string `json:"files,omitempty"`
}

// Refs converts hits for a message or the planner prompt.
func Refs(hits []Hit) []Ref {
	out := make([]Ref, 0, len(hits))
	for _, h := range hits {
		out = append(out, Ref{Name: h.Zone.Name, Policy: h.Zone.Policy, Status: string(h.Zone.Status), Files: append([]string{}, h.Files...)})
	}
	return out
}

// DeriveModulePaths is the module_paths a zone with these patterns stores:
// each pattern with a trailing "/**" (or "/*") removed, when what is left
// contains no glob metacharacter — i.e. the directory the pattern names.
// Sorted, distinct; never nil.
func DeriveModulePaths(patterns []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, p := range patterns {
		if _, ok := exclusion(p); ok {
			continue
		}
		p = normPath(p)
		for _, suf := range []string{"/**", "/*"} {
			p = strings.TrimSuffix(p, suf)
		}
		if p == "" || p == "**" || strings.ContainsAny(p, "*?[") || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ErrInvalid wraps every validation failure.
var ErrInvalid = errors.New("invalid zone")

// NormalizePatterns trims, normalises separators and drops empty and
// duplicate patterns, keeping order.
func NormalizePatterns(patterns []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, p := range patterns {
		p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// Validate checks a zone before it is written.
func Validate(name string, patterns []string, policy Policy) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if len(name) > 200 {
		return fmt.Errorf("%w: name longer than 200 characters", ErrInvalid)
	}
	if !policy.Valid() {
		return fmt.Errorf("%w: policy %q (want autonomous, human_review or escalate)", ErrInvalid, policy)
	}
	if len(patterns) == 0 {
		return fmt.Errorf("%w: zone %q has no patterns", ErrInvalid, name)
	}
	positive := 0
	for _, p := range patterns {
		glob := p
		if excl, ok := exclusion(p); ok {
			if excl == "" {
				return fmt.Errorf("%w: empty exclusion pattern %q", ErrInvalid, p)
			}
			glob = excl
		} else {
			positive++
		}
		if err := verifypolicy.ValidGlob(glob); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	if positive == 0 {
		return fmt.Errorf("%w: zone %q has only exclusion patterns", ErrInvalid, name)
	}
	return nil
}

func normPath(p string) string {
	return strings.Trim(strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"), "/")
}
