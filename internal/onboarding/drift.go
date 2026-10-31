package onboarding

import "strings"

// Drift is a single disagreement between a user-reviewed candidate and the
// freshly detected one. Consumers use it to render the drift banner or to
// publish a Kafka event.
type Drift struct {
	QuestionKey string `json:"question_key"`
	OldValue    string `json:"old_value"` // user's confirmed/edited text
	NewValue    string `json:"new_value"` // latest detection
	Extractor   string `json:"extractor"`
}

// DetectDrift compares the stored candidates (with user_status confirmed or
// edited) against the fresh set and returns any mismatches. Pending and
// rejected rows are not watched — we only warn the user when their own
// reviewed answer goes stale. Matching is by question_key; value comparison is
// case- and whitespace-insensitive to avoid false positives on trivial
// reformatting.
//
// Pure function: no DB, no I/O. Confirmed answers stay confirmed — this
// function flags disagreement, it never overwrites.
func DetectDrift(stored []Candidate, fresh []Candidate) []Drift {
	freshByKey := make(map[string]Candidate, len(fresh))
	for _, f := range fresh {
		freshByKey[f.QuestionKey] = f
	}
	var out []Drift
	for _, s := range stored {
		if s.UserStatus != StatusConfirmed && s.UserStatus != StatusEdited {
			continue
		}
		f, ok := freshByKey[s.QuestionKey]
		if !ok {
			continue
		}
		if normalise(s.EffectiveValue()) == normalise(f.Value) {
			continue
		}
		out = append(out, Drift{
			QuestionKey: s.QuestionKey,
			OldValue:    s.EffectiveValue(),
			NewValue:    f.Value,
			Extractor:   f.Extractor,
		})
	}
	return out
}

// normalise lowercases and collapses whitespace so trivial reformatting
// doesn't trigger a drift banner.
func normalise(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
