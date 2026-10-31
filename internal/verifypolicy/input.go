package verifypolicy

import (
	"errors"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/google/uuid"
)

// Input is the JSON body the onboarding step and the dashboard admin editor
// accept. Both surfaces turn it into a Policy with ToPolicy, so the rules for
// what an operator must state are the same everywhere.
type Input struct {
	// TestCommand overrides detection; null or blank means "use detection".
	TestCommand *string `json:"test_command"`
	// RequiredChecks defaults to ["test"] when omitted (null); an explicit
	// empty list is kept.
	RequiredChecks []string `json:"required_checks"`
	// NoChecksPolicy must be stated explicitly: "block" or
	// "partial_with_human_review". There is no implicit acceptance.
	NoChecksPolicy string `json:"no_checks_policy"`
	// SecurityReviewPaths defaults to GenericSecurityReviewPaths when
	// omitted (null); an explicit empty list is kept.
	SecurityReviewPaths []string `json:"security_review_paths"`
}

// ErrNoChecksPolicyRequired is returned by ToPolicy when the operator did not
// state what happens when no executable check exists.
var ErrNoChecksPolicyRequired = errors.New(`no_checks_policy is required: choose "block" or "partial_with_human_review"`)

// ToPolicy builds, normalises and validates the policy for repoID as set by
// setBy through source.
func (in Input) ToPolicy(repoID uuid.UUID, source Source, setBy string) (Policy, error) {
	if strings.TrimSpace(in.NoChecksPolicy) == "" {
		return Policy{}, ErrNoChecksPolicyRequired
	}
	p := Policy{
		RepositoryID:   repoID,
		TestCommand:    in.TestCommand,
		NoChecksPolicy: NoChecksPolicy(strings.TrimSpace(in.NoChecksPolicy)),
		Source:         source,
		SetBy:          strings.TrimSpace(setBy),
	}
	if in.RequiredChecks == nil {
		p.RequiredChecks = []evidence.CheckKind{evidence.CheckKindTest}
	} else {
		p.RequiredChecks = []evidence.CheckKind{}
		for _, k := range in.RequiredChecks {
			p.RequiredChecks = append(p.RequiredChecks, evidence.CheckKind(k))
		}
	}
	if in.SecurityReviewPaths == nil {
		p.SecurityReviewPaths = append([]string{}, GenericSecurityReviewPaths...)
	} else {
		p.SecurityReviewPaths = append([]string{}, in.SecurityReviewPaths...)
	}
	p = p.Normalize()
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// View is what both surfaces return for a repository: the effective policy
// (the stored row, or the proposal the operator is asked to confirm), whether
// a row is stored, and the detection facts the form shows.
type View struct {
	RepositoryID string `json:"repository_id"`
	Policy       Policy `json:"policy"`
	// Found is false when no policy is stored: the fail-closed default
	// governs the repository until an operator saves one.
	Found bool `json:"found"`
	// DetectedTestCommand is what internal/testdetect finds in the clone
	// ("" when nothing is detected or the clone is unavailable).
	DetectedTestCommand string `json:"detected_test_command"`
	// CloneAvailable reports whether detection could inspect a clone.
	CloneAvailable bool   `json:"clone_available"`
	Language       string `json:"language"`
	// SuggestedSecurityReviewPaths is the seed list for the language.
	SuggestedSecurityReviewPaths []string `json:"suggested_security_review_paths"`
	SelectableChecks             []string `json:"selectable_checks"`
}

// NewView assembles a View. When no policy is stored, the proposal shown is
// the fail-closed default with the language's suggested security paths;
// no_checks_policy stays "block" so it is preselected.
func NewView(repoID uuid.UUID, stored Policy, found bool, detected string, cloneAvailable bool, language string) View {
	suggested := SuggestedSecurityReviewPaths(language)
	p := stored
	if !found {
		p = Default(repoID)
		p.SecurityReviewPaths = append([]string{}, suggested...)
	}
	selectable := make([]string, 0, len(SelectableCheckKinds))
	for _, k := range SelectableCheckKinds {
		selectable = append(selectable, string(k))
	}
	return View{
		RepositoryID:                 repoID.String(),
		Policy:                       p,
		Found:                        found,
		DetectedTestCommand:          detected,
		CloneAvailable:               cloneAvailable,
		Language:                     language,
		SuggestedSecurityReviewPaths: suggested,
		SelectableChecks:             selectable,
	}
}
