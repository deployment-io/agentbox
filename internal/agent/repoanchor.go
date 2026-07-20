package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// The agent runs with cwd = WORK_DIR (e.g. /work), but the runner checks each
// repository out into a SUBDIRECTORY (/work/<owner>/<repo>) and commits per-repo
// from that subdir's git diff. So anything an agent writes at the /work root —
// which is where "create a top-level file" lands when cwd is /work — is outside
// every repository and is silently discarded: no commit, no PR. This bites all
// agents (observed with both claude-code and opencode), because nothing tells
// the agent where the repositories actually are.
//
// anchorPromptToRepos prepends a preamble naming the checked-out repository
// directories and instructing the agent to make all changes inside them. It is
// agent-agnostic (every batch driver folds cfg.StepPrompt into its args) and a
// no-op when no repositories are present (e.g. analysis-only tasks), so it never
// invents a constraint that doesn't apply.
func anchorPromptToRepos(prompt, workDir string) string {
	repos := repoDirsUnder(workDir)
	if len(repos) == 0 {
		return prompt
	}
	var b strings.Builder
	b.WriteString("Repositories for this task are checked out at:\n")
	for _, r := range repos {
		b.WriteString("- " + r + "\n")
	}
	b.WriteString("Make ALL file changes inside these repository directories. ")
	b.WriteString("Files created or modified elsewhere under " + workDir + " (including " + workDir + " itself) are NOT part of any repository and will be discarded — they are not committed and appear in no pull request. ")
	b.WriteString("When a file is described as being at the \"root\" or \"top level\", it means the root of the relevant repository above, not " + workDir + ".\n\n")
	b.WriteString(prompt)
	return b.String()
}

// repoDirsUnder returns the checked-out repository directories under workDir,
// following the runner's /work/<owner>/<repo> layout. A candidate counts as a
// repository only if it contains a .git entry — which naturally excludes
// /work/context, /work/.agentbox-input, /work/.agentbox-output, and the like.
func repoDirsUnder(workDir string) []string {
	owners, err := os.ReadDir(workDir)
	if err != nil {
		return nil
	}
	var repos []string
	for _, o := range owners {
		if !o.IsDir() || strings.HasPrefix(o.Name(), ".") || o.Name() == "context" {
			continue
		}
		ownerDir := filepath.Join(workDir, o.Name())
		children, err := os.ReadDir(ownerDir)
		if err != nil {
			continue
		}
		for _, c := range children {
			if !c.IsDir() {
				continue
			}
			repoDir := filepath.Join(ownerDir, c.Name())
			if _, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil {
				repos = append(repos, repoDir)
			}
		}
	}
	return repos
}
