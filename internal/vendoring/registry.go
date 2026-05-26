// Package vendoring implements the `agentbox vendor` subcommand: it
// detects the language ecosystems of the checked-out repos under WORK_DIR
// and pre-fetches their dependencies into the shared module cache, using
// the vendor-phase credentials. The credential-less agent phase then
// builds and verifies offline against that cache. (The package is named
// "vendoring" rather than "vendor" because Go reserves a "vendor"
// directory name for module vendoring.) See PLAN_tasks_verification.md.
package vendoring

import (
	"context"
	"fmt"
)

// Detector recognizes one language ecosystem and knows how to fetch its
// dependencies. Implementations live in per-language packages
// (vendoring/golang, future vendoring/node, ...), each registering itself
// via Register in its init(). Mirrors the agent.Driver registry pattern.
type Detector interface {
	// Name is the stable identifier for the ecosystem ("go", "node").
	Name() string
	// Detect reports whether repoDir belongs to this ecosystem (e.g. a
	// go.mod at its root). Filesystem-only; never reaches the network, so
	// it is safe to call before the egress proxy is started.
	Detect(repoDir string) (bool, error)
	// EnsureToolchain makes the language toolchain available. No-op for
	// baseline toolchains baked into the image (Go); long-tail languages
	// install onto the shared shelf here. Runs after the egress proxy is
	// up, so network installs respect the allowlist.
	EnsureToolchain(ctx context.Context) error
	// Vendor fetches repoDir's dependencies into the shared module cache.
	Vendor(ctx context.Context, repoDir string) error
	// AllowedHosts are the hostnames this ecosystem's fetch needs outbound
	// access to (package proxy, checksum db, private git host). Unioned
	// across matched detectors to seed the vendor-phase proxy allowlist.
	AllowedHosts() []string
}

var registry []Detector

// Register adds a detector. Called from a language package's init();
// panics on duplicate Name so misconfigurations surface at startup.
func Register(d Detector) {
	for _, existing := range registry {
		if existing.Name() == d.Name() {
			panic(fmt.Sprintf("vendoring: duplicate Register for %q", d.Name()))
		}
	}
	registry = append(registry, d)
}
