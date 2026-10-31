package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// depSymlinkDirs are the untracked dependency directories symlinkDeps links
// from the clone into a worktree. None of them may ever be committed as a
// symlink.
var depSymlinkDirs = []string{
	"node_modules",
	".next",
	// Python
	"venv",
	".venv",
	// PHP
	"vendor",
}

// stageAll stages the worktree (`git add -A`) and then refuses, with an
// error naming each path, when the staged change adds or changes a symlink
// that points outside the worktree or that sits where symlinkDeps links a
// dependency directory. A linked dependency directory is left out by
// pathspec (stageArgs) and by info/exclude; this guard is the backstop so
// one can never reach a commit.
func stageAll(ctx context.Context, worktreePath string) error {
	if _, err := guardGit(ctx, worktreePath, stageArgs(worktreePath)...); err != nil {
		return err
	}
	return guardStagedSymlinks(ctx, worktreePath)
}

// stageArgs is `git add -A` with every dependency directory that is a
// symlink at the worktree's top level excluded by pathspec (M6): the
// exclusion holds even when the shared info/exclude lacks the entry. A
// real directory of the same name (a committed vendor/) is staged as
// usual.
func stageArgs(worktreePath string) []string {
	args := []string{"add", "-A", "--", ":/"}
	for _, d := range depSymlinkDirs {
		fi, err := os.Lstat(filepath.Join(worktreePath, d))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		// An ignored path named in a pathspec, even an exclude, makes
		// git add fail; one that is ignored is left out anyway.
		ignored := exec.Command("git", "-C", worktreePath, "check-ignore", "-q", "--", d).Run() == nil
		if !ignored {
			args = append(args, ":(exclude,top)"+d)
		}
	}
	return args
}

// guardStagedSymlinks inspects the index against HEAD and returns an error
// listing every staged symlink (mode 120000) whose target is absolute or
// escapes the worktree, or whose path is a top-level dependency directory.
func guardStagedSymlinks(ctx context.Context, worktreePath string) error {
	raw, err := guardGit(ctx, worktreePath, "diff", "--cached", "--raw", "--no-renames", "-z", "HEAD")
	if err != nil {
		return fmt.Errorf("commit guard: %w", err)
	}
	var refused, paths []string
	// -z --raw: ":<old mode> <new mode> <old sha> <new sha> <status>\0<path>\0" per entry.
	fields := strings.Split(raw, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		p := fields[i+1]
		if len(meta) < 5 || meta[1] != "120000" {
			continue
		}
		target, err := guardGit(ctx, worktreePath, "cat-file", "blob", meta[3])
		if err != nil {
			return fmt.Errorf("commit guard: read symlink %s: %w", p, err)
		}
		if reason := refusedSymlink(p, target); reason != "" {
			refused = append(refused, fmt.Sprintf("%s -> %s (%s)", p, target, reason))
			paths = append(paths, p)
		}
	}
	if len(refused) > 0 {
		return &refusedSymlinksError{paths: paths, detail: refused}
	}
	return nil
}

// refusedSymlinksError is the guard's refusal; paths are the refused
// repository paths, still staged.
type refusedSymlinksError struct {
	paths  []string
	detail []string
}

func (e *refusedSymlinksError) Error() string {
	return fmt.Sprintf("commit guard: refusing to commit symlink(s) that must stay out of the repository: %s; unstage them or add them to .git/info/exclude", strings.Join(e.detail, "; "))
}

// commitContinuation stages and commits a continuation pass's work (M1).
// When the guard refuses symlinks, only those paths are unstaged (they stay
// on disk) and the rest is committed; refused names them. When nothing is
// left to commit, or staging or the commit fail, it returns an error and
// leaves nothing staged — the caller fails the task: work is never left
// staged while generation reports success.
func commitContinuation(ctx context.Context, worktreePath, message string) (refused []string, err error) {
	unstageAll := func() { _, _ = guardGit(ctx, worktreePath, "reset", "-q") }
	if err := stageAll(ctx, worktreePath); err != nil {
		var re *refusedSymlinksError
		if !errors.As(err, &re) {
			unstageAll()
			return nil, err
		}
		refused = re.paths
		// Literal pathspecs: a refused "[c]at" must not unstage "cat".
		args := append([]string{"--literal-pathspecs", "reset", "-q", "--"}, re.paths...)
		if _, rerr := guardGit(ctx, worktreePath, args...); rerr != nil {
			unstageAll()
			return refused, fmt.Errorf("%v; unstaging the refused paths failed: %w", err, rerr)
		}
		// Keep them out of later passes' status and staging too (escaped:
		// exclude patterns are globs).
		for _, p := range re.paths {
			_ = excludeFromGit(worktreePath, "/"+escapeIgnorePattern(p))
		}
		if staged, _ := guardGit(ctx, worktreePath, "diff", "--cached", "--name-only"); strings.TrimSpace(staged) == "" {
			return refused, fmt.Errorf("nothing left to commit after the guard: %w", err)
		}
	}
	if _, err := guardGit(ctx, worktreePath, "commit", "-q", "-m", message); err != nil {
		unstageAll()
		return refused, err
	}
	return refused, nil
}

// refusedSymlink returns why a staged symlink at repository path p with the
// given target must not be committed, or "" when it may.
func refusedSymlink(p, target string) string {
	for _, d := range depSymlinkDirs {
		if p == d {
			return "dependency directory linked into the worktree"
		}
	}
	t := strings.ReplaceAll(target, "\\", "/")
	if strings.HasPrefix(t, "/") || (len(t) >= 2 && t[1] == ':') {
		return "absolute target outside the worktree"
	}
	resolved := path.Clean(path.Join(path.Dir(p), t))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "target outside the worktree"
	}
	return ""
}

// escapeIgnorePattern makes a path match itself only in a gitignore
// pattern: glob characters and a leading '!' or '#' are backslash-escaped.
func escapeIgnorePattern(p string) string {
	var b strings.Builder
	for i, r := range p {
		switch {
		case r == '*' || r == '?' || r == '[' || r == ']' || r == '\\':
			b.WriteByte('\\')
		case i == 0 && (r == '!' || r == '#'):
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// guardGit runs git in dir and returns stdout; stderr goes into the error.
func guardGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
