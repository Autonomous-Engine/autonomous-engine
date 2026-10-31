package stage

import (
	"context"
	"fmt"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SetupError is a failure to set up the verification tree: a structured
// failure (the stage blocks the parent with it), never a verdict.
type SetupError struct {
	// Step names what failed: "head", "base", "scratch" or "diff".
	Step string
	Err  error
}

func (e *SetupError) Error() string { return e.Step + ": " + e.Err.Error() }
func (e *SetupError) Unwrap() error { return e.Err }

// Tree is the verifier's scratch repository at the head under verification
// (a shared clone in a temp dir, discarded by Close; it shares no refs or
// config with the clone) with the diff base...head.
type Tree struct {
	Scratch *verifier.Scratch
	// Base is the diff base resolved to a commit in the clone.
	Base string
	Diff verifier.Diff
}

// PrepareTree resolves baseRef in clonePath (origin/<base_branch> exists
// only there), materialises the scratch repository at head and diffs
// base...head in it. baseSHA, when empty, is the resolved base. Every
// failure is a *SetupError; nothing is left on disk after one.
func PrepareTree(ctx context.Context, clonePath, baseRef, baseSHA, head string) (*Tree, error) {
	if head == "" {
		return nil, &SetupError{Step: "head", Err: fmt.Errorf("no head commit to verify")}
	}
	base, err := verifier.ResolveCommit(ctx, clonePath, baseRef)
	if err != nil {
		return nil, &SetupError{Step: "base", Err: fmt.Errorf("cannot resolve the diff base %s: %w", baseRef, err)}
	}
	if baseSHA == "" {
		baseSHA = base
	}
	scratch, err := verifier.NewScratch(ctx, clonePath, head)
	if err != nil {
		return nil, &SetupError{Step: "scratch", Err: fmt.Errorf("cannot check out head %s: %w", head, err)}
	}
	files, patch, truncated, err := verifier.DiffOf(ctx, scratch.Path, base, scratch.Head)
	if err != nil {
		if cerr := scratch.Close(ctx); cerr != nil {
			log.Printf("verifier stage: remove scratch %s: %v", scratch.Path, cerr)
		}
		return nil, &SetupError{Step: "diff", Err: fmt.Errorf("cannot diff %s...%s: %w", baseRef, scratch.Head, err)}
	}
	return &Tree{
		Scratch: scratch,
		Base:    base,
		Diff:    verifier.Diff{BaseRef: baseRef, BaseSHA: baseSHA, HeadSHA: scratch.Head, Files: files, Patch: patch, Truncated: truncated},
	}, nil
}

// Close removes the scratch repository.
func (t *Tree) Close(ctx context.Context) {
	if t == nil || t.Scratch == nil {
		return
	}
	if err := t.Scratch.Close(ctx); err != nil {
		log.Printf("verifier stage: remove scratch %s: %v", t.Scratch.Path, err)
	}
}

// ModelRun is the model side of one verification.
type ModelRun struct {
	Adapter      adapter.ModelAdapter
	Info         verifier.ModelInfo
	SystemPrompt string
	// NewExecutor builds the tool executor rooted at the scratch
	// repository; nil uses tools.NewExecutor.
	NewExecutor func(path string) *tools.Executor
	OnTool      tools.ToolCallback
	DB          *pgxpool.Pool
	WorkID      *uuid.UUID
}

// Verify runs the verifier in the tree. src carries the requirements and
// the evidence; its diff and repository tree are the tree's.
func (t *Tree) Verify(ctx context.Context, m ModelRun, src verifier.Sources) verifier.Verdict {
	src.Diff = t.Diff
	src.RepoTree = t.Scratch.Path
	in := verifier.Build(src)
	var ex *tools.Executor
	if m.NewExecutor != nil {
		ex = m.NewExecutor(t.Scratch.Path)
	} else {
		ex = tools.NewExecutor(t.Scratch.Path)
	}
	ex.SetDiffBase(t.Base)
	ex.SetDiffHead(t.Scratch.Head)
	defer func() {
		if lm := ex.LSPManager(); lm != nil {
			lm.Close()
		}
	}()
	return verifier.Run(ctx, verifier.Runner{
		Adapter:      m.Adapter,
		Executor:     ex,
		SystemPrompt: m.SystemPrompt,
		Info:         m.Info,
		DB:           m.DB,
		WorkItemID:   m.WorkID,
		OnTool:       m.OnTool,
	}, in)
}

// VerifyAt is PrepareTree + Verify for a caller that turns a setup failure
// into a verdict instead of a block: every failure to set up the tree
// becomes a not_verifiable escalation naming the failure (review-agent's
// pr.ready fallback, M3-07 behaviour).
func VerifyAt(ctx context.Context, m ModelRun, workID uuid.UUID, reqs verifier.Requirements, ev verifier.Evidence, clonePath, baseRef, baseSHA, head string) verifier.Verdict {
	src := verifier.Sources{WorkID: workID, Requirements: reqs, Evidence: ev}
	tree, err := PrepareTree(ctx, clonePath, baseRef, baseSHA, head)
	if err != nil {
		src.Unavailable = err.Error()
		if se, ok := err.(*SetupError); ok {
			src.Unavailable = se.Err.Error()
		}
		return verifier.Run(ctx, verifier.Runner{}, verifier.Build(src))
	}
	defer tree.Close(ctx)
	return tree.Verify(ctx, m, src)
}
