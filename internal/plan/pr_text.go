package plan

import "regexp"

// PR-text criteria (R13 D3, attempt 13). The intake asked that "the pull
// request description MUST state … obtain a TileHost API key and set
// TILES_API_KEY", and the model made that a planning task. The engine, not
// an implementation task, writes the PR text (pr-coordinator), so a
// criterion that asks for something to be stated in the PR description, the
// commit message or the release notes is not repository work when no kept
// step owns it: it is then marked non-code with PRTextReason and carried as
// a recorded gap. One a kept step owns stays an ordinary criterion the
// verifier judges. Either way pr-coordinator puts its text into the PR
// description under "Requirements stated for this change".
//
// The match is narrow (R13 review minor 1): the criterion must be about the
// text being written, so a criterion that merely names commit messages or
// release notes ("the pre-commit hook rejects commit messages without a
// ticket id", "the release notes generator script lists merged PRs") is
// code work and stays subject to the ownership rule.

// PRTextReason is the non-code reason of a PR-text criterion.
const PRTextReason = "satisfied by the PR text"

const (
	// prTextTarget names the PR text: the PR / pull request description(s)
	// (also "pull-request" and "PR's"), the commit message(s), the release
	// notes.
	prTextTarget = `(?:(?:pr|pull[ -]request)(?:'s)?\s+descriptions?|commit\s+messages?|release\s+notes?)`
	// prTextVerb is a stating verb.
	prTextVerb = `(?:state|states|stating|include|includes|mention|mentions|say|says|describe|describes|note|notes|explain|explains|list|lists|document|documents|tell|tells|contain|contains|name|names|reference|references|call\s+out|calls\s+out)`
)

// prTextRE matches the two ways a criterion asks for the PR text to say
// something:
//   - the target as the subject, its verb right after (an optional modal
//     and adverb between): "the PR description MUST state …", "the commit
//     message states …", "the release notes mention …";
//   - a stating verb, then, within the same sentence, "in/into/within/to
//     the <target>": "explicitly state in the pull request description
//     that …", "say so in the PR description".
var prTextRE = regexp.MustCompile(`(?i)` +
	`\b` + prTextTarget + `\s+(?:(?:must|should|shall|will|to)\s+)?(?:(?:also|explicitly|clearly)\s+)?` + prTextVerb + `\b` +
	`|\b` + prTextVerb + `\b[^.;\n]{0,160}?\b(?:in|into|within|to)\s+(?:(?:the|its|this|each|every|a)\s+)?` + prTextTarget + `\b`)

// TargetsPRText reports whether s asks for something to be stated in the
// PR text (prTextRE).
func TargetsPRText(s string) bool { return prTextRE.MatchString(s) }

// IsPRTextCriterion reports whether c's text targets the PR text.
func IsPRTextCriterion(c Criterion) bool { return TargetsPRText(c.Text) }
