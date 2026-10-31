package verifier

import "regexp"

// gitWriteRE matches git subcommands that write refs, config, remotes or
// history, or reach another repository, with any leading global options
// (`git -C <dir> ...`, `git -c k=v ...`, `git --git-dir=... ...`).
var gitWriteRE = regexp.MustCompile(`(?i)\bgit((\s+-[cC]\s+\S+)|(\s+--[a-z-]+(=\S+)?))*\s+` +
	`(push|fetch|pull|clone|config|update-ref|symbolic-ref|remote|worktree|replace|notes|commit|reset|tag|rebase|merge|cherry-pick|revert|am|gc|prune|repack|filter-branch|switch|stash` +
	`|checkout\s+(-[bB]\b|--orphan)` +
	`|branch\s+(-[fDdmMc]\b|--force|--delete|--move|--copy|--set-upstream))\b`)

// CommandGuard is the verifier's run_command check, on top of the
// executor's built-in denylist. The scratch repository already has its own
// refs and config and no remote, so this is defence in depth: a verifier
// only reads, builds and tests; it never needs git to write.
func CommandGuard(command string) string {
	if gitWriteRE.MatchString(command) {
		return "the verifier may not run git commands that write refs, config, remotes or history, or reach another repository (read with git diff/log/show/status instead)"
	}
	return ""
}
