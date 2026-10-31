package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// R10 W3: a stuck control that ends the loop. A live run (attempt 9) issued
// the same rejected edit_file 21 times: the old control compared adjacent
// turns only (a read in between reset it), stripped a tool the next turn's
// refresh put back, and its only exits were a FINAL/INSUFFICIENT
// round-trip the caller ignored.

// ErrLoopStuck is wrapped by every *LoopStuck.
var ErrLoopStuck = errors.New("tool loop stuck")

// LoopStuck is the structured failure a loop with StuckControl ends with:
// the same failing call (tool, canonical arguments, target file contents)
// a third time, a fifth failed edit of one path, or the model answering
// INSUFFICIENT. Fingerprint identifies the call for a later pass
// (LoopOptions.PriorStuck).
type LoopStuck struct {
	Tool string `json:"tool"`
	Path string `json:"path,omitempty"`
	// Kind is set by the R17 exits: StuckKindCycle, StuckKindRepeatedRead,
	// StuckKindSlidingWindow. The R10 exits leave it empty.
	Kind        string `json:"kind,omitempty"`
	Reason      string `json:"reason"`
	Attempts    int    `json:"attempts"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

func (s *LoopStuck) Error() string {
	where := s.Tool
	if s.Path != "" {
		where += " on " + s.Path
	}
	if s.Kind != "" {
		return fmt.Sprintf("LoopStuck (%s): %s, %d times: %s", s.Kind, where, s.Attempts, s.Reason)
	}
	return fmt.Sprintf("LoopStuck: %s failed %d times: %s", where, s.Attempts, s.Reason)
}

func (s *LoopStuck) Unwrap() error { return ErrLoopStuck }

const (
	// stuckRepeatLimit: the third identical failing call ends the loop.
	stuckRepeatLimit = 3
	// stuckPathFailCap: failed edits of one path end the loop, whatever
	// their arguments.
	stuckPathFailCap = 5
	// stuckReasonMax bounds a LoopStuck reason and the repeated-call note.
	stuckReasonMax = 600
)

// stuckTracker counts failing file-tool calls over the whole loop.
type stuckTracker struct {
	fails     map[string]int
	reasons   map[string]string
	pathFails map[string]int
	prior     map[string]LoopStuck
	last      *LoopStuck // the latest failure, for INSUFFICIENT
}

func newStuckTracker(prior []LoopStuck) *stuckTracker {
	t := &stuckTracker{fails: map[string]int{}, reasons: map[string]string{}, pathFails: map[string]int{}, prior: map[string]LoopStuck{}}
	for _, p := range prior {
		if p.Fingerprint != "" {
			t.prior[p.Fingerprint] = p
		}
	}
	return t
}

// callFingerprint is tool + canonical arguments + the sha256 of the file
// the call targets as it is now; "" for a call without a path (run_command
// and searches are not tracked).
func (e *Executor) callFingerprint(call adapter.ToolCall) (fp, path string) {
	var args map[string]any
	if json.Unmarshal([]byte(call.Arguments), &args) != nil {
		return "", ""
	}
	p, _ := args["path"].(string)
	if strings.TrimSpace(p) == "" {
		return "", ""
	}
	path = pinPath(p)
	digest := "absent"
	full := filepath.Join(e.repoPath, filepath.Clean(e.normalizePath(p)))
	if strings.HasPrefix(full, e.repoPath) {
		if b, err := os.ReadFile(full); err == nil {
			sum := sha256.Sum256(b)
			digest = hex.EncodeToString(sum[:8])
		}
	}
	return call.Name + "\x00" + canonicalArgs(call.Arguments) + "\x00" + digest, path
}

func isEditTool(name string) bool { return IsFileEditTool(name) }

// callFailed reports a failing tool result: an error, or a rejection.
func callFailed(r adapter.ToolResult) bool {
	return r.IsError || strings.HasPrefix(strings.TrimSpace(r.Content), "REJECTED")
}

// observe records one executed call. A second identical failure gets the
// previous failure appended verbatim (with the hints it carried); the
// third, or the fifth failed edit of one path, or any repeat of a call a
// previous pass was stuck on, returns the LoopStuck that ends the loop.
func (t *stuckTracker) observe(call adapter.ToolCall, fp, path string, res *adapter.ToolResult) *LoopStuck {
	if fp == "" || !callFailed(*res) {
		return nil
	}
	reason := truncateRunes(strings.TrimSpace(res.Content), stuckReasonMax)
	t.fails[fp]++
	n := t.fails[fp]
	t.last = &LoopStuck{Tool: call.Name, Path: path, Reason: reason, Attempts: n, Fingerprint: fp}
	if p, ok := t.prior[fp]; ok {
		return &LoopStuck{Tool: call.Name, Path: path, Reason: "repeated a call an earlier pass was stuck on: " + reason, Attempts: p.Attempts + n, Fingerprint: fp}
	}
	if isEditTool(call.Name) && path != "" {
		t.pathFails[path]++
		if t.pathFails[path] >= stuckPathFailCap {
			return &LoopStuck{Tool: call.Name, Path: path, Reason: fmt.Sprintf("%d failed edits of this file; the last: %s", t.pathFails[path], reason), Attempts: t.pathFails[path], Fingerprint: fp}
		}
	}
	if n >= stuckRepeatLimit {
		return &LoopStuck{Tool: call.Name, Path: path, Reason: reason, Attempts: n, Fingerprint: fp}
	}
	if n == 2 {
		res.Content += "\n\n[repeated call] You made this exact call before, on the same file contents, and it failed the same way. " +
			"The previous result was:\n" + t.reasons[fp] +
			"\nDo not repeat it: change the arguments using the lines named above, or move on to another step. A third identical call ends this step as stuck."
	}
	t.reasons[fp] = reason
	return nil
}

// insufficient turns a final "INSUFFICIENT: …" answer into a LoopStuck
// naming the latest failing call (if any).
func (t *stuckTracker) insufficient(content string) *LoopStuck {
	reason := truncateRunes(strings.TrimSpace(content), stuckReasonMax)
	if t.last == nil {
		return &LoopStuck{Tool: "(none)", Reason: reason, Attempts: 1}
	}
	s := *t.last
	s.Reason = reason + " (last failure: " + truncateRunes(t.last.Reason, 200) + ")"
	return &s
}

func isInsufficient(content string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(content)), "INSUFFICIENT")
}
