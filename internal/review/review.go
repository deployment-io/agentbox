package review

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/result"
)

// Plan is everything a review round needs before the agent starts: the prompt
// to run, the passes that will run, and the coverage record that describes
// what was and was not examined.
//
// Coverage is built BEFORE the agent runs, from facts agentbox knows — which
// passes the gate stood down and why — rather than from what the agent says
// afterwards. A reviewer asked to report its own coverage will report that it
// covered everything.
type Plan struct {
	Prompt   string
	Passes   []string
	Coverage []Coverage
	Diff     Diff

	// Open is the previous round's still-open must-fix findings, as the
	// runner supplied them. Carried on the plan so the extractor can bound
	// the status list to the keys that were actually asked about.
	Open []config.ReviewOpenFinding
}

// NothingToReview reports whether the cost gate stood every pass down, so the
// caller can skip spawning an agent entirely. The coverage record still
// explains why — a skipped review is a recorded decision, not a silence.
func (p Plan) NothingToReview() bool {
	return len(p.Passes) == 0
}

// Build computes the diff, applies the cost gate, and assembles the prompt.
//
// An error here means the DIFF could not be computed — a base commit this clone
// does not have, a repository key that names no checkout, git missing. The
// caller must turn that into a FAILED review, never a clean one: the whole
// point of the round is to look at the change, and a round that could not look
// has found nothing because it saw nothing.
func Build(cfg *config.Config) (Plan, error) {
	diff, err := Compute(cfg.WorkDir, cfg.ReviewBaseCommits)
	if err != nil {
		return Plan{}, err
	}
	passes, skipped := SelectPasses(cfg.ReviewPasses, diff, cfg.ReviewSpec, cfg.WorkDir)
	plan := Plan{Passes: passes, Diff: diff, Open: cfg.ReviewOpenFindings}
	if len(passes) > 0 {
		// The diff files are the change the reviewer reads. A round that
		// cannot write them has nothing to hand the reviewer, so it fails
		// the same way a round that could not compute the diff does.
		if err := Write(cfg.WorkDir, &plan.Diff); err != nil {
			return Plan{}, err
		}
		plan.Prompt = buildPrompt(cfg, plan)
	}
	plan.Coverage = BuildCoverage(passes, skipped)
	return plan, nil
}

// FailedCoverage is the coverage record for a round that could not run: every
// parameter NotChecked, each carrying the same reason. Used when the diff
// itself could not be computed, so the result still says what was examined —
// nothing — and why, rather than arriving as an empty findings list that reads
// like a clean bill of health.
func FailedCoverage(reason string) []Coverage {
	reason = truncateRunes(reason, MaxReasonRunes)
	out := make([]Coverage, 0, len(allParameters))
	for _, parameter := range allParameters {
		out = append(out, Coverage{Parameter: parameter, State: stateNotChecked, Reason: reason})
	}
	return out
}

// LiftResult turns the agent's final message into the review half of
// result.json, and returns the message with every <review> block removed.
//
// The coverage that SHIPS is agentbox's, not the agent's. The agent's claims
// are read and used only where they can make the record more honest: a pass
// that ran but reports itself skipped is believed, because an agent saying "I
// could not check this" is information nobody else has. A pass that ran and
// claims a cleaner state than agentbox recorded is not — that direction is the
// one that hides a gap.
//
// A previous-finding status the reviewer did not report does NOT come back as
// "resolved": it simply does not appear, and the consumer reads an absent
// status as still present. An unreadable trailer therefore clears nothing.
func LiftResult(finalMessage string, planned []Coverage, open []config.ReviewOpenFinding) (*result.ReviewResult, string) {
	findings, claimed, previous, stripped, ok := Extract(finalMessage, open)
	if !ok {
		// No parseable trailer. The coverage record still goes out: the run
		// happened, the passes ran, and the consumer needs to know what was
		// examined even when the report came back unreadable.
		return &result.ReviewResult{Findings: []Finding{}, Coverage: planned}, stripped
	}
	out := &result.ReviewResult{
		Findings: findings,
		Coverage: reconcileCoverage(planned, claimed),
		Previous: previous,
	}
	// Deploy requirements come only from a round the deploy pass ran in. The
	// planned coverage is agentbox's own record of that: "deploy readiness"
	// is checked exactly when the pass was in the plan.
	if deployPassRan(planned) {
		out.DeployRequirements = ExtractDeployRequirements(finalMessage)
	}
	return out, stripped
}

func deployPassRan(planned []Coverage) bool {
	for _, c := range planned {
		if c.Parameter == parameterForPass[PassDeploy] {
			return c.State == stateChecked
		}
	}
	return false
}

// reconcileCoverage merges the agent's claims into agentbox's record, one
// parameter at a time, and only ever in the direction that admits a gap.
func reconcileCoverage(planned, claimed []Coverage) []Coverage {
	byParameter := map[string]Coverage{}
	for _, c := range claimed {
		byParameter[normalize(c.Parameter)] = c
	}
	out := make([]Coverage, 0, len(planned))
	for _, p := range planned {
		claim, hasClaim := byParameter[normalize(p.Parameter)]
		if p.State == stateChecked && hasClaim && normalize(claim.State) == normalize(stateSkipped) {
			reason := claim.Reason
			if reason == "" {
				reason = "the pass reported that it could not check this parameter"
			}
			if p.Reason != "" {
				reason = reason + "; " + p.Reason
			}
			out = append(out, Coverage{Parameter: p.Parameter, State: stateSkipped, Reason: truncateRunes(reason, MaxReasonRunes)})
			continue
		}
		out = append(out, p)
	}
	return out
}

func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// severityRule is the one sentence-set that decides what a severity MEANS,
// carried in every pass brief and in the trailer rules so the reviewer meets it
// while it is looking and again while it is writing the block.
//
// It exists because of an observed failure: a Task whose spec required an
// unauthenticated route returning all of process.env was rated Critical in
// round 1, and — after fix rounds that added a comment, then logging, then a
// README section — re-rated Info "because the spec requires it". Severity is a
// statement about the harm the code can do, not about who authorised it; a
// spec can make a finding EXPECTED, and the consumer's policy can decide to
// ship it anyway, but neither makes it harmless. Prose around dangerous code
// changes nothing a caller can reach.
const severityRule = `Rate severity by the harm the code can cause as written. A requirement in the spec never lowers a finding's severity — if the spec requires something dangerous, report it at its real severity and say in 'why' that the spec requires it. Comments, documentation, logging or a README note do not reduce a finding's severity unless they change what the code does.`

// severityRubric says what each of the five words MEANS, so the scale is a
// shared definition rather than each reviewer's private sense of "likelihood
// times impact". Without it every pass invents its own scale, and the two
// rules below are the two places that invention went wrong in live reviews.
//
// Observed: a reaper that finished a stuck conversion but left the session's
// agent Job running forever; a review container that left a newly created
// repository writable while telling the reviewer its repositories were
// read-only; a flag that dropped Codex's own sandbox without checking the
// claim it relied on; an interrupted round that could still mark a must-fix
// finding "resolved". Every one is a gap in a guarantee the change was made to
// provide, every one fails silently — no error, just a leak, an unreviewed
// write or a wrong result — and every one came back rated Low, discounted
// because the trigger is uncommon or because some other control limits the
// damage. Neither discount survives contact with how this review is used: it
// runs on every change, so an uncommon path is exercised regularly, and a
// control that merely narrows the blast radius still leaves the harm.
//
// Carried in every pass brief and in the trailer rules, alongside
// severityRule, so every pass and the report format read the same scale.
const severityRubric = `Use this scale:
- critical — exploitable as written, secrets or credentials exposed, or data lost or corrupted.
- high — a core behaviour or a security guarantee breaks in normal use.
- medium — the change's own stated guarantee can be bypassed or silently fail under a plausible condition; or a resource leaks (a process, container, connection, lock or file that is never released); or a result is silently wrong.
- low — a real defect whose failure is visible and harmless: the caller gets a clear error, or the output is cosmetically wrong.
- info — an observation, not a defect.
Do not lower a severity because the triggering condition is uncommon when the failure is silent or defeats what the change is for. This review runs on every change, so an uncommon path is exercised regularly, and a silent failure is found only after it has done damage.
Other controls lower a severity only when they fully prevent the harm, not when they merely limit it. Say which controls you relied on in 'why'.`

// passBriefs say what each pass is looking for. One focused brief per pass is
// the point of the design: an omnibus "review this diff" prompt returns a
// scattering of style notes, while a pass that has been told it is looking for
// one class of problem finds that class.
//
// Every brief ends with severityRule and severityRubric: a pass that finds the
// right problem and rates it Low has told the consumer's threshold to ignore
// it, so the scale has to be in front of the reviewer in each pass and not
// only in the report format.
var passBriefs = map[string]string{
	PassSecurity:    `Look ONLY for security problems this diff introduces or leaves open: injection (SQL, command, template), missing authentication or authorisation on a new path, secrets or credentials committed or logged, unsafe deserialisation, path traversal, SSRF, missing validation of untrusted input crossing a trust boundary, a dependency change that pulls in something unvetted, and weakened crypto or transport security. Report a finding only when you can point at the line in the diff that causes it. ` + severityRule + "\n" + severityRubric,
	PassSpec:        `Look ONLY at whether the change does what the spec in [What the change is meant to achieve] asks. Not code quality, not security: other passes cover those. When the spec lists acceptance criteria ("Acceptance"), check each one against the change and the code around it; a criterion that is not met, or is met only in part, is a finding. When the spec is a plain description, check its stated goal. A change to something the spec lists as out of scope ("OutOfScope") is a finding, and so are behaviour changes the spec did not ask for. Treat the spec's "Assumptions" as context, not as requirements. Where the spec is ambiguous and the change chose one reading, say which reading at info. Within the scale below: the spec's goal itself not achieved is high; an acceptance criterion not met or met only in part is medium; an out-of-scope or unrequested change is low unless it breaks something, in which case the correctness pass reports the breakage; an ambiguity is info. location is the file where the missing behaviour belongs or the file that contradicts the criterion; when there is no such file, use "spec: acceptance criterion N". key names the criterion, e.g. spec-criterion-3-export-csv, with no line number. ` + severityRule + "\n" + severityRubric,
	PassCorrectness: `Look ONLY for correctness problems this diff introduces: logic that does not do what the surrounding code and the spec say it should, off-by-one and boundary errors, nil or null dereferences, unhandled errors and swallowed failures, race conditions and unsynchronised shared state, and resource leaks. When the diff changes a function signature, a wire shape or an API contract, check every caller and consumer across the repositories in the workspace, and report a contract change whose other side is not part of this change. Follow what the change PRODUCES as well as who calls it: for every new or changed output — text, markup, a record, a request — find the existing code that later consumes or transforms it (cuts it to a size limit, escapes it, parses it, stores it, sends it) and check the combination, even when that code is not in the diff. Cutting text at a character or byte count can split an HTML tag, a code fence, a JSON value or a multi-byte character, so check that every cut the output can go through leaves it well formed. When the change calls a third-party API, check each request against that API's documented rules as you know them, and say which rule a request breaks. Report a finding only when you can point at the line in the diff that causes it — for a problem in how unchanged code handles the change's output, that is the diff line producing the output — and say what input or state makes it go wrong. ` + severityRule + "\n" + severityRubric,
	PassDeploy:      `Look ONLY at whether this change will deploy and run the way this organization deploys it. The deployment facts are in /work/context/services.json, one JSON object per line; use the rows whose "repo" is a repository in this change. A row can carry: environment and environmentId; type (Web Service, Private Service or Static Site); variableNames, the NAMES of the variables the service's environment provides, never values, with variablesFrom saying where they came from — no variablesFrom means unknown, not empty; buildArgNames; ports; healthCheckPath; isSpa; rootDirectory; publishDirectory; deployFromImage. Report as findings, each with a location in the diff: a variable the code reads whose name is close to one the environment has but not the same (DATABASE_URL where the environment has DB_URL); a port the server now listens on that is not in ports; a health check path that no longer answers or now requires authentication; a file or directory the code reads at run time that the build does not put into the image; for a static site, build output that no longer lands in publishDirectory, or client-side routes that need isSpa when it is false; a Docker build argument the Dockerfile now requires that buildArgNames lacks; code the service needs that moved outside rootDirectory. A variable that this change newly reads and that the service's variableNames does not contain is NOT a finding: report it as a deploy requirement (see [How to report]). The change is right to need it and only a person can set its value; never suggest removing it, hard-coding it or giving it a default. A variable that was already read before this change is neither a finding nor a requirement. When variablesFrom is missing for a service, you cannot tell what is set: report no requirement for it. Within the scale below: a deploy that fails, or a service that cannot start or serve, is high; a feature that fails at run time in the deployed environment is medium. ` + severityRule + "\n" + severityRubric,
}

// buildPrompt assembles the review prompt: the spec, an index of the change
// with the diff file each repository's part was written to, and one focused
// brief per pass. The trailer format is appended by the driver.
//
// It contains the spec, the change index, the build-and-test section, the pass
// list and — on a re-check — the REVIEWER'S OWN still-open findings, AND
// NOTHING ELSE. No previous result.json, no progress file, no transcript, and
// above all no PROSE the implementer wrote: not its summary, not its
// description of what it fixed. A reviewer shown "I fixed it" grades the claim
// instead of the code, which is how a Critical finding survives three rounds of
// comments and ends up recorded as fixed. The runner enforces the same
// boundary structurally by moving the implementer's output directory out of
// the work dir before the round starts, so this is belt and braces rather than
// the only guard.
//
// The one exception is the implementer's build/test RESULT (see
// buildAndTestSection) — what command ran and whether it passed. It survives
// the boundary because it is a fact the reviewer can act on rather than an
// argument about the change, it is labelled as the implementer's own unchecked
// claim, and on read-only mounts the reviewer is invited to re-run the command
// instead of believing it.
//
// The opening line tells the reviewer not to edit anything or change the
// working tree, and it says nothing about whether that is merely asked for.
// It is not: the runner mounts every repository read-only into a review
// container (REVIEW_READONLY_MOUNTS), so a write fails in the kernel whatever
// the prompt says. The instruction stays because it aims the run — a reviewer
// that knows it is not implementing spends its turns reading — and it must
// never be rewritten into a promise that the tree is merely off limits by
// request, which would read as an invitation to try.
//
// The diff itself is NOT here. It is on disk (see Diff), and the prompt tells
// the reviewer where and insists it is read in full before the first pass.
// The prompt therefore stays small whatever the change's size; the drivers
// deliver it on stdin regardless, so nothing about its size is load-bearing.
func buildPrompt(cfg *config.Config, plan Plan) string {
	var b strings.Builder
	b.WriteString("You are reviewing a code change. You are NOT implementing anything: do not edit, create or delete any file, and do not run any command that changes the working tree.\n\n")
	if cfg.ReviewRound > 1 {
		b.WriteString(fmt.Sprintf("This is review round %d. The diff files describe the CURRENT state of the change, including fixes made since the previous round. Judge what you see now.\n\n", cfg.ReviewRound))
		if len(cfg.ReviewOpenFindings) == 0 {
			b.WriteString("You have not been shown the earlier round's findings and should not try to reconstruct them.\n\n")
		}
	}
	b.WriteString("[What the change is meant to achieve]\n")
	if spec := strings.TrimSpace(cfg.ReviewSpec); spec != "" {
		if len(spec) > MaxSpecBytes {
			spec = truncateBytesOnRuneBoundary(spec, MaxSpecBytes) +
				"\n[… spec truncated at its size cap]"
		}
		b.WriteString(spec)
	} else {
		b.WriteString("(no spec was supplied — judge the change on its own terms)")
	}

	b.WriteString("\n\n[The change under review]\n")
	b.WriteString(fmt.Sprintf("The complete diff of each repository against the commit it was checked out at when the Step began has been written to a file under %s. READ EVERY DIFF FILE IN FULL, with your file-reading tool, before starting the first pass: the file IS the change, and a pass that has not read it has reviewed nothing. Read a large file in pages rather than skipping it. These files sit outside every repository and are not part of the change. The repositories themselves are checked out under %s, so open any file the diff touches when you need its surroundings, and search the workspace for the callers of anything the diff changes.\n", filepath.Join(cfg.WorkDir, DirName), cfg.WorkDir))
	for _, r := range plan.Diff.Repos {
		b.WriteString(fmt.Sprintf("\n- repository %s (against %s): %s (%d bytes), %d changed file(s)\n", r.Dir, shortSHA(r.Base), r.File, len(r.Text), len(r.Paths)))
		for i, p := range r.Paths {
			if i == MaxIndexPathsPerRepo {
				b.WriteString(fmt.Sprintf("    (and %d more — every path is in the diff file)\n", len(r.Paths)-i))
				break
			}
			b.WriteString("    " + p + "\n")
		}
	}

	if cfg.ReviewRound > 1 {
		b.WriteString(fixDiffSection(cfg.ReviewFixDiffs))
	}
	b.WriteString(previousIssuesSection(cfg.ReviewOpenFindings))
	b.WriteString(buildAndTestSection(cfg))

	b.WriteString("\n[Passes]\n")
	b.WriteString("Run these passes ONE AT A TIME, in order, over the change in the diff files. Each is a separate, focused examination of the same change — finish one before starting the next, and do not merge them into a single sweep.\n")
	for i, pass := range plan.Passes {
		brief := passBriefs[pass]
		if brief == "" {
			brief = "Look only for problems of this kind that the diff introduces."
		}
		b.WriteString(fmt.Sprintf("\n%d. %s pass — %s\n", i+1, pass, brief))
	}
	return b.String()
}

// fixDiffSection points a re-check at the last fix run's OWN diff: what it
// changed since the previous round, one file per repository it touched. It is
// where the reviewer starts — the previously reported issues are checked
// against it, and it is where a fix most likely broke something — but the
// passes still run over the whole change.
//
// Empty when there are no entries, so a round without fix diffs gets exactly
// the prompt it always did. Entries are sorted by directory so the prompt is
// stable; a file whose size cannot be read shows 0 bytes rather than vanishing
// (config already checked it was readable).
func fixDiffSection(diffs map[string]string) string {
	if len(diffs) == 0 {
		return ""
	}
	dirs := make([]string, 0, len(diffs))
	for dir := range diffs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	var b strings.Builder
	b.WriteString("\n[What the last fix changed]\n")
	b.WriteString("The last fix run's own changes — everything it changed since the previous review round, and nothing else — are in these files. Read them FIRST, in full. Check each previously reported issue against them, and look for anything the fix broke or introduced. Then run the passes over the whole change as usual: the fix diff is where to start, not a replacement for the full diff.\n")
	for _, dir := range dirs {
		var size int64
		if info, err := os.Stat(diffs[dir]); err == nil {
			size = info.Size()
		}
		b.WriteString(fmt.Sprintf("- repository %s: %s (%d bytes)\n", dir, diffs[dir], size))
	}
	b.WriteString("Repositories not listed here were not changed by the last fix.\n")
	return b.String()
}

// previousIssuesSection lists the findings the previous round left open and
// asks for a verdict on each, read off the CURRENT code.
//
// It sits between the change index and the passes deliberately: the reviewer
// has just been told where the code is and has not yet started looking, so the
// status question is answered by reading, not by recalling. And it says what
// "resolved" means, because the loop this fixes ended with a Critical marked
// fixed after three rounds that added a comment, some logging and a README
// section — every one of which leaves the vulnerable code exactly as it was.
//
// Empty when nothing is open: round 1 must ask nothing about earlier rounds.
func previousIssuesSection(open []config.ReviewOpenFinding) string {
	if len(open) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n[Previously reported issues — check each one]\n")
	b.WriteString("A previous round of this review reported the following and they were not resolved then.\n")
	for _, f := range open {
		b.WriteString(fmt.Sprintf("\n- key: %s\n", strings.TrimSpace(f.Key)))
		b.WriteString(fmt.Sprintf("  parameter: %s\n", fieldOrUnknown(f.Parameter)))
		b.WriteString(fmt.Sprintf("  severity: %s\n", fieldOrUnknown(f.Severity)))
		b.WriteString(fmt.Sprintf("  location: %s\n", fieldOrUnknown(f.Location)))
		b.WriteString(fmt.Sprintf("  what: %s\n", fieldOrUnknown(f.What)))
	}
	b.WriteString("\nFor each, read the CURRENT code and decide whether the problem is still present. 'resolved' means the code no longer has the problem. A comment, documentation, logging, a README note, or a spec requirement does NOT resolve it. Then run the passes below as a fresh review; a problem that is still present is reported in the status list, not repeated as a new finding.\n")
	return b.String()
}

// MaxVerifyStepsShown caps how many repository lines the build-and-test
// section prints. A workspace has a handful of repositories, not hundreds; the
// cap exists so a malformed-but-parseable input cannot pad the prompt.
const MaxVerifyStepsShown = 50

// MaxVerifyFieldRunes caps one reported command. It is a shell line, and a
// runaway value would push the passes off the reviewer's attention.
const MaxVerifyFieldRunes = 300

// buildAndTestSection tells the reviewer what the implementer says it verified
// and whether this review may check that for itself.
//
// Both halves matter, and they matter for opposite reasons. The result is
// SELF-REPORTED by the agent whose work is under review, so it is the one
// claim in the whole round that nobody has checked — which is exactly why it
// is worth naming, and exactly why it must not be the last word. When the
// runner mounted every repository read-only AND the write probe agreed (see
// agent.verifyReadOnlyMounts, which runs before the drivers read this config),
// running the build is free of the risk that made review mode read-only in the
// first place: the kernel refuses the write whatever the command does. A
// reviewer that can run `go test` is an independent check on the implementer's
// "it passed". The sentence follows cfg.ReviewCanRunCommands, not the mounts
// alone: the harness must also let a reviewer run commands (opencode's review
// config denies bash whatever the mounts are).
//
// A reviewer that may run the build also has to be told which failures are
// the container's rather than the change's. The network is restricted to an
// allowlist, so a build that fetches a dependency can fail on the download
// alone; and the repositories are mounted read-only, which Go tolerates
// (its cache lives outside the tree) but cargo's target/, a test cache under
// node_modules and the like do not. Either one reads exactly like a broken
// change to a reviewer that was never told the run is fenced.
//
// Without that permission the section says so plainly rather than staying
// silent. A Claude reviewer held to the read-only allowlist spent turns
// retrying `go build` into permission denials it had no way to interpret; a
// reviewer told the commands are unavailable spends those turns reading. It
// is pointed at the implementer's result only when there IS one: with none,
// "rely on the result above" pointed at the sentence saying no result was
// reported, which asks the reviewer to lean on nothing.
//
// It sits before [Passes] on purpose: the reviewer decides how it is going to
// examine the change before it starts examining it.
func buildAndTestSection(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("\n[Build and tests]\n")
	b.WriteString(implementerVerifyLines(cfg.ReviewVerifyResult))
	if cfg.ReviewCanRunCommands {
		b.WriteString("You may run the repository's build and test commands. They cannot change the repositories, so running them does not break the instruction at the top. Prefer the narrowest command that exercises the change, such as the tests of the packages the diff touches, over the whole suite: this review round has a time limit. Report a failure as a finding only when this diff causes it, and say which command you ran. A failure caused by the environment is not a finding: a download the restricted network blocks, or a tool that tries to write inside the read-only repositories (a build cache or a target directory).\n")
	} else if cfg.ReviewVerifyResult == nil {
		b.WriteString("Build and test commands are not available in this review. Do not try to run them; judge the change by reading it.\n")
	} else {
		b.WriteString("Build and test commands are not available in this review. Do not try to run them; rely on the result above.\n")
	}
	return b.String()
}

// implementerVerifyLines renders the implementer's reported result as one line
// per repository, or says plainly that there is none.
//
// "No result" and "a result that says nothing ran" are different answers and
// are reported differently: the first means nobody told this round anything,
// the second means the implementer looked and decided there was nothing to
// run. A reviewer that cannot tell them apart cannot judge either.
func implementerVerifyLines(v *config.ReviewVerifyResult) string {
	if v == nil {
		return "The implementer reported no build or test result.\n"
	}
	var b strings.Builder
	b.WriteString("The implementer reported this result for its own work. It is the implementer's own claim about its own change, not a checked fact.\n")
	if !v.Ran {
		reason := verifyField(v.SkippedReason)
		if reason == "" {
			reason = "no reason given"
		}
		b.WriteString("- no build or test was run: " + reason + "\n")
		return b.String()
	}
	if len(v.Steps) == 0 {
		b.WriteString("- every repository: " + verifyCommandOf(v.Command) + " — " + passedWord(v.Passed) + "\n")
	}
	for i, s := range v.Steps {
		if i == MaxVerifyStepsShown {
			b.WriteString(fmt.Sprintf("- (and %d more repository result(s), not shown)\n", len(v.Steps)-i))
			break
		}
		repo := verifyField(s.Repo)
		if repo == "" {
			repo = "(repository not recorded)"
		}
		b.WriteString("- " + repo + ": " + verifyCommandOf(s.Command) + " — " + passedWord(s.Passed) + "\n")
	}
	if v.PreExisting {
		b.WriteString("The implementer reported that every failure above was already present before this change.\n")
	}
	return b.String()
}

func passedWord(passed bool) string {
	if passed {
		return "passed"
	}
	return "failed"
}

func verifyCommandOf(command string) string {
	if c := verifyField(command); c != "" {
		return c
	}
	return "(command not recorded)"
}

// verifyField flattens one reported value onto a single line and caps it. The
// value comes from an agent's free-form trailer by way of the runner, so a
// newline in it would forge extra lines in a section the reviewer reads as
// structure.
func verifyField(s string) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	return truncateRunes(s, MaxVerifyFieldRunes)
}

func fieldOrUnknown(s string) string {
	if s = strings.TrimSpace(s); s != "" {
		return s
	}
	return "(not recorded)"
}

// Instruction is the machine-readable half of the ask: how to report what the
// passes found. Each driver appends it the way its harness takes extra
// instruction — claude through --append-system-prompt, codex and opencode
// folded into the prompt — and in review mode NONE of them appends the
// implementer's instruction, so no <verify> or <pr_title> trailer is asked for
// or produced.
//
// It lives here rather than in the drivers because the trailer format is a
// property of the review contract, not of any one harness; three copies would
// be three things to keep in step with the extractor.
func Instruction(cfg *config.Config) string {
	passes := cfg.ReviewPasses
	if len(passes) == 0 {
		passes = []string{PassSecurity, PassCorrectness}
	}
	return reviewTrailerInstruction(trailerPasses(passes, cfg.ReviewSpec), cfg.ReviewOpenFindings)
}

// deployTrailerRule documents "deploy_requirements", and only when the deploy
// pass is one of the passes: a block from a round without it has nothing to
// put there, and LiftResult discards the field when the pass did not run.
func deployTrailerRule(passes []string) string {
	for _, p := range passes {
		if p == PassDeploy {
			return `
- When the deploy readiness pass found a variable this change newly reads that the service's environment does not provide, the block must also carry "deploy_requirements": [{"variable":"<NAME>","service":"<service exactly as in services.json>","environment":"<environment exactly as in services.json, or empty>","location":"<file:line where the code reads it>"}] — one entry per variable and service. Never put a value in it. Leave it out when there are none.`
		}
	}
	return ""
}

// trailerPasses drops the spec pass when there is no spec, because the gate
// will stand it down and its parameter must not be offered for findings. The
// diff-dependent half of the gate is not known here and is not applied.
func trailerPasses(passes []string, spec string) []string {
	if strings.TrimSpace(spec) != "" {
		return passes
	}
	out := make([]string, 0, len(passes))
	for _, p := range passes {
		if p != PassSpec {
			out = append(out, p)
		}
	}
	return out
}

// trailerParameters names the parameters the requested passes report
// against. The pass name and the parameter name differ for the spec pass
// ("spec" reports as "spec conformance").
func trailerParameters(passes []string) []string {
	out := make([]string, 0, len(passes))
	for _, p := range passes {
		if parameter, ok := parameterForPass[p]; ok {
			out = append(out, parameter)
			continue
		}
		out = append(out, p)
	}
	return out
}

func reviewTrailerInstruction(passes []string, open []config.ReviewOpenFinding) string {
	return fmt.Sprintf(`

[How to report]
Your final message must end with ONE <review> block. Put the opening and closing tags on lines of their own, with compact JSON between them:

<review>
{"findings":[{"key":"sec-auth-missing","parameter":"security","severity":"high","location":"0-acme/api/handler.go:41","what":"the new /export handler does not check the caller's session","why":"any unauthenticated caller can read another org's data"}],"coverage":[{"parameter":"security","state":"checked"},{"parameter":"correctness","state":"checked"}]}
</review>

Rules for the block:
- parameter is one of: %s. severity is one of: info, low, medium, high, critical. state is one of: checked, skipped.
- Report a finding ONLY for a problem the diff introduces or leaves open, with a location a reader can open. Do not report style preferences, pre-existing issues the diff does not touch, or things you would have done differently.
- %s
%s
- key is a short stable slug naming the parameter, the file and the rule, e.g. sec-unauthenticated-env-dump-app-js. Never include a line number: lines move between rounds, and the key must stay the same for the same problem.
- what is what you saw; why is why it matters. Keep both to a few sentences.
- If a pass found nothing, say so with coverage state "checked" and no findings for it. An empty findings list is a legitimate and common answer.%s%s
- Emit the block once, at the very end. Everything outside it is prose for a human and will be shown in the pull request.`,
		strings.Join(trailerParameters(passes), ", "), severityRule, severityRubric, previousTrailerRule(open), deployTrailerRule(passes))
}

// previousTrailerRule documents the "previous" field, and only when the round
// was actually given findings to report a status for. Asking for the field on
// round 1 would invite an agent to invent entries for findings nobody made.
func previousTrailerRule(open []config.ReviewOpenFinding) string {
	if len(open) == 0 {
		return ""
	}
	return fmt.Sprintf(`
- You were given %d previously reported issue(s). The block must also carry "previous": [{"key":"<the key given>","status":"resolved"|"still_present","note":"<one sentence>"}] — ONE ENTRY PER LISTED FINDING, using the key exactly as it was given to you. status is "resolved" only when the current code no longer has the problem; a comment, documentation, logging, a README note or a spec requirement does not make it "resolved". An entry whose key was not in the list is discarded.`, len(open))
}
