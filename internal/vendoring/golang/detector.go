// Package golang is the Go ecosystem detector for the vendor subcommand.
// It registers itself with internal/vendoring at init time.
package golang

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/deployment-io/agentbox/internal/vendoring"
)

func init() { vendoring.Register(&detector{}) }

type detector struct{}

func (*detector) Name() string { return "go" }

// Detect matches a Go module by a go.mod at the repo root.
func (*detector) Detect(repoDir string) (bool, error) {
	_, err := os.Stat(filepath.Join(repoDir, "go.mod"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// EnsureToolchain is a no-op: Go is a baseline toolchain baked into the
// agentbox image, so it is present in both the vendor and agent phases.
func (*detector) EnsureToolchain(context.Context) error { return nil }

// Vendor populates the shared module cache with the repo's full module
// graph. GOMODCACHE is pointed at the /cache shelf by the runner;
// GOWORK=off forces standalone resolution so a checked-out repo vendors
// the same set it would resolve in CI, not against an ambient workspace.
func (*detector) Vendor(ctx context.Context, repoDir string) error {
	cmd := exec.CommandContext(ctx, "go", "mod", "download", "all")
	cmd.Dir = repoDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go mod download in %s: %w", repoDir, err)
	}
	return nil
}

// AllowedHosts are the hosts `go mod download` reaches:
//   - proxy.golang.org / sum.golang.org — public module proxy + checksum db
//   - github.com / objects.githubusercontent.com — direct fetch of
//     GOPRIVATE modules (e.g. github.com/deployment-io) and their archives
func (*detector) AllowedHosts() []string {
	return []string{
		"proxy.golang.org",
		"sum.golang.org",
		"github.com",
		"objects.githubusercontent.com",
	}
}
