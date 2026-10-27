package vendoring_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/deployment-io/agentbox/internal/vendoring"
	_ "github.com/deployment-io/agentbox/internal/vendoring/golang"
	_ "github.com/deployment-io/agentbox/internal/vendoring/node"
)

// planFor builds a plan over a runner-layout work dir holding one repo with
// the given marker file at its root.
func planFor(t *testing.T, marker string) *vendoring.Plan {
	t.Helper()
	work := t.TempDir()
	repo := filepath.Join(work, "0-acme", "svc")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, marker), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := vendoring.BuildPlan(work)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return p
}

// vuln.go.dev (govulncheck's database) is reachable in the agent phase only
// when the work dir has a Go module, and never in the vendor phase.
func TestPlanVulnGoDevOnlyWithGo(t *testing.T) {
	node := planFor(t, "package.json")
	if slices.Contains(node.VerifyHosts(), "vuln.go.dev") {
		t.Errorf("Node-only VerifyHosts() = %v, should not contain vuln.go.dev", node.VerifyHosts())
	}

	goPlan := planFor(t, "go.mod")
	if !slices.Contains(goPlan.VerifyHosts(), "vuln.go.dev") {
		t.Errorf("Go VerifyHosts() = %v, want vuln.go.dev", goPlan.VerifyHosts())
	}
	if slices.Contains(goPlan.AllowedHosts(), "vuln.go.dev") {
		t.Errorf("Go AllowedHosts() = %v, should not contain vuln.go.dev", goPlan.AllowedHosts())
	}
}
