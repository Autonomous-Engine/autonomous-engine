package tools

import (
	"encoding/json"
	"fmt"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// toolPathArg is a file tool call's path argument ("" when it has none).
func toolPathArg(c adapter.ToolCall) string {
	var args map[string]any
	if json.Unmarshal([]byte(c.Arguments), &args) != nil {
		return ""
	}
	p, _ := args["path"].(string)
	return p
}

// The edit gate (LoopOptions.EditGate). Live, the implementation agent
// spent 21 calls (243k tokens) reading files and describing what it would
// do, through three "you did not call edit_file" nudges, and never edited.
// After EditGate consecutive read turns with no edit, reads are not served
// until an edit is made: the files in scope are pinned in the ledger as
// they are on disk, which is what an edit needs.

func editGateRefusal(reads int) string {
	return fmt.Sprintf("read_file: not served — you have read for %d turns without editing. "+
		"The files in scope are in the ledger under \"Current file contents\", as they are on disk: "+
		"call edit_file (old_string copied exactly from there) or write_file now. Reads resume after an edit.", reads)
}
