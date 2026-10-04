package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

// writeServices puts a services.json under <workDir>/context with the given
// lines, one JSON object (or garbage) per line.
func writeServices(t *testing.T, workDir string, lines ...string) {
	t.Helper()
	dir := filepath.Join(workDir, "context")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "services.json"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func codeDiff(dir string, paths ...string) Diff {
	d := Diff{Repos: []RepoDiff{{Dir: dir}}}
	for _, p := range paths {
		d.Paths = append(d.Paths, dir+"/"+p)
		d.Repos[0].Paths = append(d.Repos[0].Paths, p)
	}
	return d
}

func TestKnownReviewPassesIncludeDeploy(t *testing.T) {
	found := false
	for _, p := range config.KnownReviewPasses() {
		if p == PassDeploy {
			found = true
		}
	}
	if !found {
		t.Fatalf("config does not accept %q: %v", PassDeploy, config.KnownReviewPasses())
	}
	if parameterForPass[PassDeploy] != "deploy readiness" {
		t.Errorf("deploy reports as %q, want deploy readiness", parameterForPass[PassDeploy])
	}
}

func TestDeployGate(t *testing.T) {
	matching := `{"service":"api","repo":"Org/Repo","environment":"prod","variablesFrom":"environment"}`
	for _, tc := range []struct {
		name     string
		diff     Diff
		services []string // nil: no services.json at all
		want     string
	}{
		{"an empty diff skips", Diff{}, []string{matching}, ReasonNoChanges},
		{"documentation only skips", codeDiff("0-org/repo", "README.md", "docs/x.go"), []string{matching}, ReasonDocsOnly},
		{"lockfiles only skip", codeDiff("0-org/repo", "go.sum"), []string{matching}, ReasonLockfileOnly},
		{"a missing services.json skips", codeDiff("0-org/repo", "main.go"), nil, ReasonNoDeployContext},
		{"no row for a changed repository skips", codeDiff("0-org/repo", "main.go"), []string{`{"service":"web","repo":"org/other"}`}, ReasonNoDeployContext},
		{"a matching row runs, ignoring case", codeDiff("0-org/repo", "main.go"), []string{matching}, ""},
		{"a malformed line is skipped, not fatal", codeDiff("12-org/repo", "main.go"), []string{"{not json", matching}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			if tc.services != nil {
				writeServices(t, workDir, tc.services...)
			}
			passes, skipped := SelectPasses([]string{PassDeploy}, tc.diff, "", workDir)
			if tc.want == "" {
				if !equalStrings(passes, []string{PassDeploy}) || len(skipped) != 0 {
					t.Errorf("passes = %v, skipped = %v, want deploy to run", passes, skipped)
				}
			} else if skipped[PassDeploy] != tc.want {
				t.Errorf("skipped[deploy] = %q, want %q (passes %v)", skipped[PassDeploy], tc.want, passes)
			}

			coverage := BuildCoverage(passes, skipped)
			for _, c := range coverage {
				if c.Parameter != "deploy readiness" {
					continue
				}
				if tc.want == "" && c.State != stateChecked {
					t.Errorf("deploy readiness = %+v, want checked", c)
				}
				if tc.want != "" && (c.State != stateSkipped || c.Reason != tc.want) {
					t.Errorf("deploy readiness = %+v, want skipped: %s", c, tc.want)
				}
			}
		})
	}
}

func TestRepoNameDropsTheNumericPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"0-deployment-io/kit": "deployment-io/kit",
		"12-acme/api":         "acme/api",
		"acme/api":            "acme/api",
		"0-acme/0-api":        "acme/0-api",
	} {
		if got := repoName(in); got != want {
			t.Errorf("repoName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPromptCarriesTheDeployBriefOnlyWhenThePassRuns(t *testing.T) {
	root := t.TempDir()
	api := filepath.Join(root, "0-acme", "api")
	base := initRepo(t, api)
	write(t, filepath.Join(api, "handler.go"), "package api\n\nfunc Handle() {}\n")

	cfg := reviewConfig(t, nil)
	cfg.WorkDir = root
	cfg.ReviewBaseCommits = map[string]string{"0-acme/api": base}
	cfg.ReviewPasses = []string{PassSecurity, PassDeploy}

	plan, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %s", err)
	}
	if strings.Contains(plan.Prompt, passBriefs[PassDeploy]) {
		t.Error("the prompt carries the deploy brief with no services.json")
	}

	writeServices(t, root, `{"service":"api","repo":"acme/api","variablesFrom":"environment"}`)
	plan, err = Build(cfg)
	if err != nil {
		t.Fatalf("Build: %s", err)
	}
	if !equalStrings(plan.Passes, []string{PassSecurity, PassDeploy}) {
		t.Fatalf("passes = %v, want security and deploy", plan.Passes)
	}
	if want := fmt.Sprintf("\n2. deploy pass — %s\n", passBriefs[PassDeploy]); !strings.Contains(plan.Prompt, want) {
		t.Error("the prompt does not carry the deploy brief")
	}
	if !strings.HasPrefix(passBriefs[PassDeploy], "Look ONLY at whether this change will deploy and run the way this organization deploys it.") {
		t.Error("the deploy brief does not open as specified")
	}
}

func TestTrailerRuleForDeployRequirementsOnlyWithThePass(t *testing.T) {
	cfg := reviewConfig(t, nil)
	if strings.Contains(Instruction(cfg), "deploy_requirements") {
		t.Error("the trailer documents deploy_requirements without the deploy pass")
	}
	cfg.ReviewPasses = []string{PassSecurity, PassDeploy}
	instruction := Instruction(cfg)
	if !strings.Contains(instruction, `- When the deploy readiness pass found a variable this change newly reads that the service's environment does not provide, the block must also carry "deploy_requirements"`) {
		t.Error("the trailer lacks the deploy_requirements rule with the deploy pass")
	}
	if !strings.Contains(parametersLine(t, instruction), "deploy readiness") {
		t.Error("the parameter line does not offer deploy readiness")
	}
}

func TestLiftResultKeepsDeployRequirementsOnlyWhenThePassRan(t *testing.T) {
	var entries []string
	entries = append(entries,
		`{"variable":" STRIPE_KEY ","service":" api ","environment":" prod ","location":"0-acme/api/pay.go:12"}`,
		`{"variable":"STRIPE_KEY","service":"api","environment":"prod","location":"elsewhere"}`, // duplicate
		`{"variable":"STRIPE_KEY","service":"api","environment":"","location":"x"}`,             // different environment: kept
		`{"variable":"1BAD","service":"api"}`,
		`{"variable":"BAD-NAME","service":"api"}`,
		`{"variable":"OK","service":"   "}`,
	)
	for i := 0; i < 30; i++ {
		entries = append(entries, fmt.Sprintf(`{"variable":"V%d","service":"api"}`, i))
	}
	text := "prose\n<review>\n{\"findings\":[],\"coverage\":[],\"deploy_requirements\":[" + strings.Join(entries, ",") + "]}\n</review>"

	ran := BuildCoverage([]string{PassSecurity, PassDeploy}, nil)
	result, stripped := LiftResult(text, ran, nil)
	if stripped != "prose" {
		t.Errorf("stripped = %q", stripped)
	}
	got := result.DeployRequirements
	if len(got) != MaxDeployRequirements {
		t.Fatalf("kept %d requirements, want the cap %d: %+v", len(got), MaxDeployRequirements, got)
	}
	if got[0].Variable != "STRIPE_KEY" || got[0].Service != "api" || got[0].Environment != "prod" || got[0].Location != "0-acme/api/pay.go:12" {
		t.Errorf("first = %+v, want trimmed STRIPE_KEY/api/prod", got[0])
	}
	if got[1].Variable != "STRIPE_KEY" || got[1].Environment != "" {
		t.Errorf("second = %+v, want STRIPE_KEY with no environment", got[1])
	}
	if got[2].Variable != "V0" {
		t.Errorf("third = %+v, want V0 — invalid names and blank services dropped", got[2])
	}

	notRan := BuildCoverage([]string{PassSecurity}, map[string]string{PassDeploy: ReasonNoDeployContext})
	result, _ = LiftResult(text, notRan, nil)
	if len(result.DeployRequirements) != 0 {
		t.Errorf("requirements = %+v, want none without the deploy pass", result.DeployRequirements)
	}
}

func TestDeployRequirementCapsFieldLengths(t *testing.T) {
	long := strings.Repeat("é", 500)
	got := capDeployRequirements([]parsedDeployRequirement{{Variable: "A", Service: long, Environment: long, Location: long}})
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if n := len([]rune(got[0].Service)); n != 200 {
		t.Errorf("service runes = %d, want 200", n)
	}
	if n := len([]rune(got[0].Environment)); n != 200 {
		t.Errorf("environment runes = %d, want 200", n)
	}
	if n := len([]rune(got[0].Location)); n != 300 {
		t.Errorf("location runes = %d, want 300", n)
	}
	if got := capDeployRequirements([]parsedDeployRequirement{{Variable: strings.Repeat("A", 129), Service: "s"}}); len(got) != 0 {
		t.Errorf("a 129-character name was kept: %+v", got)
	}
}

func TestDeployRequirementsJSONOmittedWhenEmpty(t *testing.T) {
	result, _ := LiftResult("<review>\n{\"findings\":[],\"coverage\":[]}\n</review>", BuildCoverage([]string{PassDeploy}, nil), nil)
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "deploy_requirements") {
		t.Errorf("result carries an empty deploy_requirements: %s", raw)
	}
}

func TestDeployBriefEnvFiles(t *testing.T) {
	brief := passBriefs[PassDeploy]
	for _, want := range []string{
		"no variablesFrom means unknown, not empty; envFileNames; buildArgNames",
		"envFileNames are files deployment.io writes into the service's root directory (rootDirectory, or the repository root when it is empty) before every build and deploy: they are in the build and the image although the repository does not contain them, and the variables of dotenv-style ones are already in variableNames.",
		"A variable defined in a dotenv file the repository itself commits and the build ships (.env, .env.production and the like, read by dotenv, Vite, Next.js or a similar loader) is also provided — report no requirement for it. Report as findings,",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("deploy brief is missing %q", want)
		}
	}
}
