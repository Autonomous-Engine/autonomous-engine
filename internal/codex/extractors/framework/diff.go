// internal/codex/extractors/framework/diff.go
package framework

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func ComputeDiff(ctx context.Context, repoPath, parent, current string) (*GitDiff, error) {
	if parent == "" {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "diff", "--name-status", parent+".."+current)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	d := &GitDiff{ParentSHA: parent, CurrentSHA: current}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		status := parts[0]
		path := parts[1]
		switch status[0] {
		case 'A':
			d.FilesAdded = append(d.FilesAdded, path)
		case 'D':
			d.FilesRemoved = append(d.FilesRemoved, path)
		default:
			d.FilesChanged = append(d.FilesChanged, path)
		}
	}
	return d, nil
}
