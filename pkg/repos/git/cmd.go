package git

import (
	"context"

	"github.com/gptscript-ai/gptscript/pkg/debugcmd"
)

func newGitCommand(ctx context.Context, args ...string) *debugcmd.WrappedCmd {
	cmd := debugcmd.New(ctx, "git", args...)
	return cmd
}

func cloneBare(ctx context.Context, repo, toDir string) error {
	cmd := newGitCommand(ctx, "clone", "--bare", "--depth", "1", repo, toDir)
	return cmd.Run()
}

func gitWorktreeAdd(ctx context.Context, gitDir, commitDir, commit string) error {
	// The double -f is intentional
	cmd := newGitCommand(ctx, "--git-dir", gitDir, "worktree", "add", "-f", "-f", commitDir, commit)
	return cmd.Run()
}

func fetchCommit(ctx context.Context, gitDir, commit string) error {
	cmd := newGitCommand(ctx, "--git-dir", gitDir, "fetch", "origin", commit)
	return cmd.Run()
}
