// Command verify-browser is the entrypoint of the verify-browser image: it
// loads one page of a Task's preview in headless Chromium, runs a short list
// of steps and assertions, and reports what the browser saw.
//
// The runner bind-mounts a per-call directory at /io holding input.json and
// collects /io/result.json once the container exits. This program validates
// the input, starts agentbox's egress proxy (which refuses private, link-local,
// loopback and CGN addresses at dial time), runs the Node script that drives
// Chromium through that proxy, bounds it by timeout_ms, and moves the
// script's result into place — or writes status "error" saying why there is
// none. When input.json asks for a screenshot, the script's screenshot of the
// page the check ended on is moved to /io/screenshot.jpg beside it. The contract is docs/VERIFY_BROWSER.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/deployment-io/agentbox/internal/proxy"
	"github.com/deployment-io/agentbox/internal/signals"
)

const (
	defaultIODir  = "/io"
	defaultScript = "/opt/verify-browser/run.mjs"
	defaultNode   = "node"

	// scriptGrace is how long the script has to exit after SIGTERM before it
	// is killed.
	scriptGrace = 10 * time.Second
	// stderrTailBytes is how much of the script's stderr an error result
	// carries.
	stderrTailBytes = 1000
)

// config is where things are. Flags exist for running the program by hand
// and for tests; the image runs it with the defaults.
type config struct {
	ioDir  string
	node   string
	script string
	grace  time.Duration
	// timeout overrides timeout_ms when non-zero (tests only).
	timeout time.Duration
}

// startProxy is a seam so tests can see how the proxy is configured.
var startProxy = proxy.Start

func main() {
	cfg := config{grace: scriptGrace}
	flag.StringVar(&cfg.ioDir, "io", defaultIODir, "directory holding input.json; result.json is written here")
	flag.StringVar(&cfg.node, "node", defaultNode, "node binary")
	flag.StringVar(&cfg.script, "script", defaultScript, "browser script")
	flag.Parse()

	ctx, cancel := signals.NewContext(context.Background())
	code := run(ctx, cfg)
	cancel()
	os.Exit(code)
}

// run performs one check and returns the exit code: 0 when result.json was
// written with status "completed", 1 otherwise.
func run(ctx context.Context, cfg config) int {
	resultPath := filepath.Join(cfg.ioDir, "result.json")
	data, err := os.ReadFile(filepath.Join(cfg.ioDir, "input.json"))
	if err != nil {
		return fail(resultPath, fmt.Errorf("reading input.json: %w", err), "", "")
	}
	in, err := parseInput(data)
	if err != nil {
		return fail(resultPath, fmt.Errorf("invalid input: %w", err), "", rawSecret(data))
	}
	srv, err := startProxy(proxy.NewAllowAllList(), proxy.Config{Logger: os.Stderr, BlockPrivateIPs: true})
	if err != nil {
		return fail(resultPath, fmt.Errorf("starting the egress proxy: %w", err), "", in.secret())
	}
	defer srv.Close()

	timeout := time.Duration(in.TimeoutMS) * time.Millisecond
	if cfg.timeout > 0 {
		timeout = cfg.timeout
	}
	tmpResult := filepath.Join(cfg.ioDir, fmt.Sprintf(".result-%d.tmp.json", os.Getpid()))
	defer os.Remove(tmpResult)
	tmpScreenshot := filepath.Join(cfg.ioDir, fmt.Sprintf(".screenshot-%d.tmp.jpg", os.Getpid()))
	defer os.Remove(tmpScreenshot)
	outcome := runScript(ctx, scriptRun{
		node:      cfg.node,
		script:    cfg.script,
		args:      []string{filepath.Join(cfg.ioDir, "input.json"), tmpResult, srv.Addr(), tmpScreenshot},
		timeout:   timeout,
		grace:     cfg.grace,
		stderrMax: stderrTailBytes,
	})
	return finish(resultPath, tmpResult, tmpScreenshot, outcome, in.secret())
}

// fail writes an error result, with secret (sign_in's password, or "")
// redacted, and returns exit code 1.
func fail(resultPath string, err error, stderrTail, secret string) int {
	msg := err.Error()
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, redacted)
	}
	fmt.Fprintf(os.Stderr, "[verify-browser] %s\n", msg)
	if werr := writeErrorResult(resultPath, err.Error(), stderrTail, secret); werr != nil {
		fmt.Fprintf(os.Stderr, "[verify-browser] writing result.json: %v\n", werr)
	}
	return 1
}
