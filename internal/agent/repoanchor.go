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
//
// It also names the pre-built context directory when the runner wrote one (see
// contextDirUnder) — the same directory the interactive session prompt points
// a session at, which no implement run was ever told about.
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
	b.WriteString("Make ALL file changes inside these repository directories — anything written elsewhere under " + workDir + " is discarded (not committed, no PR). ")
	b.WriteString("\"Root\" and \"top level\" mean the repository root above, not " + workDir + ".\n")
	if dir, ok := contextDirUnder(workDir); ok {
		b.WriteString("Pre-built context about these repositories and how they are deployed is at " + dir + " (start with index.md). ")
		b.WriteString("Consult it when the change touches deployment, configuration, or how services connect; a change confined to code does not need it.\n")
	}
	b.WriteString("\n")
	b.WriteString(prompt)
	return b.String()
}

// contextDirUnder reports the pre-built context directory the runner writes
// for a Task — the organisation's services, how they are deployed, how they
// reach each other — and whether it is there at all.
//
// Only the interactive session prompt used to mention it, so an implement run
// was handed a directory nobody told it about and answered deployment
// questions from the code alone. The index file is the existence test rather
// than the directory: an empty /work/context is a directory with nothing to
// read, and pointing an agent at it would cost a turn to learn that.
func contextDirUnder(workDir string) (string, bool) {
	dir := filepath.Join(workDir, "context")
	if _, err := os.Stat(filepath.Join(dir, "index.md")); err != nil {
		return "", false
	}
	return dir, true
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
