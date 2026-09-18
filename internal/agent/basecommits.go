package agent

import (
	"os/exec"
	"strings"
)

// recordStartCommits captures the commit each checked-out repository is at
// RIGHT NOW, keyed by the repository directory. Called before the agent
// subprocess starts, so the map describes the state handed to this run.
//
// That timing is the whole point. A later `git rev-parse HEAD` is not a
// substitute: an agent committing its own work is explicitly supported, and
// on that path HEAD consists entirely of the agent's commits. Replaying a
// failed verify there would reproduce the agent's OWN failure, call it
// pre-existing, and push genuinely broken code — strictly worse than
// discarding the work.
//
// Best-effort by construction: a repository whose rev-parse fails (unborn
// HEAD on a fresh init, a directory that isn't really a checkout) is simply
// absent from the map. Absence means "no baseline", which the replay treats
// as a reason to leave the gate closed — so a failure here costs diagnosis,
// never correctness, and never fails the run.
func recordStartCommits(workDir string) map[string]string {
	repos := repoDirsUnder(workDir)
	if len(repos) == 0 {
		return nil
	}
	commits := make(map[string]string, len(repos))
	for _, dir := range repos {
		if sha := headCommit(dir); sha != "" {
			commits[dir] = sha
		}
	}
	if len(commits) == 0 {
		return nil
	}
	return commits
}

// headCommit returns the repository's current HEAD as a full SHA, or "" when
// it can't be determined.
func headCommit(repoDir string) string {
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
