package main

import (
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/google/uuid"
)

// newResearchExecutor is the task's executor: rooted at the pinned snapshot
// (primaryPath, M2-03) and reading it through the codex read tools as the
// task's repository. Before this, the read tools resolved the product's
// shared clone instead of the pin, so no read counted as coverage and
// record_finding refused every citation (review #1); the pin is what the
// result's base_sha says was read.
func newResearchExecutor(primaryPath, repoName string, cx *codex.Codex, vs *vector.VectorStore, productID *uuid.UUID, kb bus.MessageBus, workID *uuid.UUID, taskID, researchType string) *tools.Executor {
	ex := tools.NewExecutorWithCodex(primaryPath, cx, vs, productID, kb, workID)
	ex.SetCodexReadsFromWorktree(true)
	ex.SetWorktreeRepo(repoName)
	ex.SetFindingTask(taskID, researchType)
	// Reads come in windows a finding can cite whole: the live code_trace
	// task read 240 lines at 10k tokens a call, could not cite them under
	// the finding cap, and re-read until the repeat guard ended it.
	ex.SetReadWindow(researchReadWindow, researchReadWindowMax)
	return ex
}

const (
	researchReadWindow    = 120
	researchReadWindowMax = 240
)

// primaryRepoName is the name of the repository whose read path is primary
// (pinned.Paths parallels the selected repos); "" when it cannot be told,
// which makes repo-less reads use the executor's tree.
func primaryRepoName(repos []repoRef, paths []string, primary string) string {
	for i, p := range paths {
		if p == primary && i < len(repos) {
			return repos[i].Name
		}
	}
	return ""
}
