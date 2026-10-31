package main

import "testing"

// M4-06: only a replay review request with a branch and a verified head is
// reviewed without a PR; every other PR-less pr.ready is still skipped.
func TestReviewableWithoutPR(t *testing.T) {
	for _, tc := range []struct {
		p    prReadyMessage
		want bool
	}{
		{prReadyMessage{Replay: true, Branch: "agent/x/y", HeadSHA: "abc"}, true},
		{prReadyMessage{Replay: true, Branch: "agent/x/y"}, false},
		{prReadyMessage{Replay: true, HeadSHA: "abc"}, false},
		{prReadyMessage{Branch: "agent/x/y", HeadSHA: "abc"}, false},
	} {
		if got := reviewableWithoutPR(tc.p); got != tc.want {
			t.Errorf("%+v: %t, want %t", tc.p, got, tc.want)
		}
	}
}
