// Package config loads and validates the agentbox environment contract.
//
// See docs/CONTRACT.md for the full input spec.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultNoActivityTimeout = 10 * time.Minute

// MCPSocketEnv names the env var carrying the runner's per-task MCP tool
// socket path. Load reads it into Config.MCPSocket; a driver that wires MCP
// outside BuildArgs — and so has no *Config in hand — reads the env var
// itself, from here, so the name is spelled once.
const MCPSocketEnv = "MCP_TOOL_RPC_SOCKET"

// Agent execution modes, selected via the AGENT_MODE env var.
const (
	// ModeBatch is the default fire-and-forget mode: one
	// `claude -p "<STEP_PROMPT>"`, one /result.json, exit.
	ModeBatch = "batch"
	// ModeInteractive keeps the container alive for a long-lived,
	// bidirectional stream-json session driven by user messages over a
	// pipe (see internal/agent/interactive.go). Used by repo-aware chat.
	ModeInteractive = "interactive"
	// ModeReview reviews the diff an implement run produced against the
	// Task's spec and reports findings as a <review> trailer. One agent
	// run, no STEP_PROMPT: the work item is the diff itself, computed by
	// internal/review from REVIEW_BASE_COMMITS.
	//
	// A review run reads NOTHING a previous run wrote — no result.json, no
	// progress.json, no message records, no transcript. Its prompt is the
	// diff, the spec and the pass list, and nothing else. The runner
	// enforces this structurally as well (it moves the implementer's output
	// directory out of the work dir before the round), so the guarantee
	// does not rest on restraint here.
	ModeReview = "review"
)

// Config captures the validated inputs for one agentbox run.
type Config struct {
	StepPrompt           string
	WorkDir              string
	PreviousStepsSummary string
	Model                string
	MaxTurns             string
	AgentType            string
	AgentVersion         string

	// TokenBudget is the cumulative input+output token cap for one run, 0
	// when unset (uncapped). Enforced agentbox-side by the Run loop's
	// limit watcher for agents whose CLI lacks a native budget flag (e.g.
	// Codex); Claude Code reports usage only at the end, so the watcher
	// never preempts it.
	TokenBudget int

	// Mode is batch (default), interactive or review — see the Mode*
	// constants. From AGENT_MODE.
	Mode string

	// ReviewSpec is what the diff is reviewed AGAINST: the Task's structured
	// spec as JSON, or its prose description when it has no spec. Passed
	// through to the review prompt verbatim — agentbox does not interpret
	// it, because a spec is for the reviewer to read, not for agentbox to
	// parse. From REVIEW_SPEC.
	ReviewSpec string

	// ReviewPasses names the focused passes to run, in order, e.g.
	// ["security", "correctness"]. From the comma-separated REVIEW_PASSES.
	// Empty falls back to security and correctness, which is the only set
	// this release ships.
	ReviewPasses []string

	// ReviewBaseCommits maps a repository directory (relative to WorkDir) to
	// the commit it was checked out at when the Step began. THE BASELINE IS
	// NOT HEAD: an implementer committing its own work is supported, and
	// diffing against HEAD after that would show nothing at all. From the
	// JSON object in REVIEW_BASE_COMMITS.
	ReviewBaseCommits map[string]string

	// ReviewReadOnlyMounts reports that the RUNNER mounted every repository
	// read-only into this review container, so the working tree cannot be
	// written whatever the agent attempts. From REVIEW_READONLY_MOUNTS ("1"
	// or "true", case-insensitive).
	//
	// It exists because a harness's own sandbox can be worse than no sandbox:
	// Codex implements --sandbox read-only with bubblewrap, which cannot
	// create namespaces inside agentbox's container, so every command the
	// reviewer runs fails and the review examines nothing. When the mounts
	// carry the guarantee, the driver can drop that sandbox.
	//
	// Absent means a runner that predates read-only mounts, and the drivers
	// keep their own read-only enforcement: a review that cannot run is safer
	// than one that could write.
	ReviewReadOnlyMounts bool

	// ReviewRound is the 1-based round number within one Step's Review
	// stage. Carried so the round can be named in logs and so a prompt can
	// say whether this is a first look or a re-check after fixes. From
	// REVIEW_ROUND.
	ReviewRound int

	// ReviewOpenFindings are the must-fix findings the PREVIOUS review round
	// left open, passed by the runner on rounds 2 and 3. From the JSON array
	// in REVIEW_OPEN_FINDINGS; empty on round 1.
	//
	// This is the reviewer's own previous output and NOTHING ELSE. The
	// implementer's summary, transcript and description of what it fixed stay
	// out of the review's input, because a reviewer shown "I fixed it" grades
	// the claim instead of the code — which is exactly how a Critical finding
	// gets waved through after a round that only added a comment.
	ReviewOpenFindings []ReviewOpenFinding

	// SessionID, when set, is forwarded to the agent as a stable session
	// identifier (claude --session-id) so the transcript persists on disk
	// and can be resumed after a container restart. From SESSION_ID.
	SessionID string

	// MaxBudgetUSD caps total spend for the run (claude --max-budget-usd).
	// Empty = uncapped. From MAX_BUDGET_USD.
	MaxBudgetUSD string

	// ReadOnly restricts the agent to read-only investigation. The driver
	// builds a tool allowlist and deliberately omits
	// --dangerously-skip-permissions, so the allowlist is enforced rather
	// than bypassed. From READ_ONLY.
	ReadOnly bool

	// AppendSystemPrompt is extra text appended to the agent's default
	// system prompt (claude --append-system-prompt). Loaded from the file
	// named by APPEND_SYSTEM_PROMPT_FILE — the CLI has no
	// --append-system-prompt-file flag, so agentbox reads the file and
	// passes the contents inline. Empty = none.
	AppendSystemPrompt string

	// MCPSocket is the container path of the runner's per-task MCP tool socket
	// (MCPSocketEnv). Set for the agent phase when the runner exposes
	// runner-executed tools; empty = no tool channel. Each driver points its
	// agent's MCP client at it via agent.BridgeCommand + the `mcp-bridge`
	// subcommand. Credentials stay in the runner; only intent crosses the socket.
	MCPSocket string

	// NoActivityTimeout is zero when the detector is disabled.
	NoActivityTimeout time.Duration

	// Exactly one of AnthropicDirect, Subscription, or Bedrock is populated.
	AnthropicDirect *AnthropicDirectCreds
	Subscription    *SubscriptionCreds
	Bedrock         *BedrockCreds
}

type AnthropicDirectCreds struct {
	APIKey string
}

// SubscriptionCreds is the Claude Code subscription (Pro/Max) OAuth path — a
// token from `claude setup-token`, passed as CLAUDE_CODE_OAUTH_TOKEN. Genuine
// Claude Code reads it from the inherited env; this only records that the path
// was chosen. See the runner's maybeApplyClaudeSubscriptionAuth.
type SubscriptionCreds struct {
	OAuthToken string
}

type BedrockCreds struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Region          string
}

// Load reads environment variables and validates the contract.
// Returns an error if required vars are missing or if the credential
// path is ambiguous (both set or neither set).
func Load() (*Config, error) {
	c := &Config{
		StepPrompt:           strings.TrimSpace(os.Getenv("STEP_PROMPT")),
		WorkDir:              envOr("WORK_DIR", "/work"),
		PreviousStepsSummary: os.Getenv("PREVIOUS_STEPS_SUMMARY"),
		Model:                os.Getenv("MODEL"),
		MaxTurns:             os.Getenv("MAX_TURNS"),
		AgentType:            envOr("AGENT_TYPE", "claude-code"),
		Mode:                 envOr("AGENT_MODE", ModeBatch),
		SessionID:            strings.TrimSpace(os.Getenv("SESSION_ID")),
		MaxBudgetUSD:         strings.TrimSpace(os.Getenv("MAX_BUDGET_USD")),
		ReadOnly:             parseBoolEnv("READ_ONLY"),
		MCPSocket:            strings.TrimSpace(os.Getenv(MCPSocketEnv)),
	}

	switch c.Mode {
	case ModeBatch, ModeInteractive, ModeReview:
	default:
		return nil, fmt.Errorf("invalid AGENT_MODE %q: must be %q, %q or %q", c.Mode, ModeBatch, ModeInteractive, ModeReview)
	}

	// STEP_PROMPT is the batch-mode work item. Interactive mode receives
	// user turns over the message pipe, and review mode builds its own
	// prompt from the diff and the spec, so neither requires it.
	if c.Mode == ModeBatch && c.StepPrompt == "" {
		return nil, fmt.Errorf("STEP_PROMPT is required")
	}

	if c.Mode == ModeReview {
		if err := c.loadReviewInputs(); err != nil {
			return nil, err
		}
	}

	appendPrompt, err := loadAppendSystemPrompt()
	if err != nil {
		return nil, err
	}
	c.AppendSystemPrompt = appendPrompt

	if _, err := os.Stat(c.WorkDir); err != nil {
		return nil, fmt.Errorf("WORK_DIR %q is not accessible: %w", c.WorkDir, err)
	}

	c.AgentVersion = agentVersionForType(c.AgentType)

	timeout, err := parseNoActivityTimeout(os.Getenv("NO_ACTIVITY_TIMEOUT"))
	if err != nil {
		return nil, err
	}
	c.NoActivityTimeout = timeout

	// TOKEN_BUDGET is an optional integer cap; absent / malformed / negative
	// all resolve to 0 (uncapped). Strict validation isn't worth a hard
	// failure here — the limit watcher simply doesn't engage at 0.
	if tb, convErr := strconv.Atoi(os.Getenv("TOKEN_BUDGET")); convErr == nil && tb > 0 {
		c.TokenBudget = tb
	}

	if err := c.loadCredentials(); err != nil {
		return nil, err
	}

	return c, nil
}

// defaultReviewPasses is the pass set this release ships: the two parameters
// the platform default policy gates on. The other six review parameters have
// no pass and are reported NotChecked rather than silently omitted.
var defaultReviewPasses = []string{"security", "correctness"}

// knownReviewPasses is every pass name this release can actually run. A pass
// must map to a review parameter, because the coverage record is per parameter:
// a pass with no parameter would run, cost a model call, and then have nowhere
// to report what it covered.
//
// Deliberately duplicated rather than imported from internal/review — review
// imports config, so the dependency cannot go the other way. Two names is a
// small enough mirror to keep by hand; adding a pass means adding it here and
// to internal/review's parameterForPass. internal/review's
// TestConfigAndReviewAgreeOnThePassList pins the two lists to each other.
var knownReviewPasses = map[string]bool{
	"security":    true,
	"correctness": true,
}

// KnownReviewPasses returns every pass name this release can run, for the
// cross-check test in internal/review (which can import config; config
// cannot import review).
func KnownReviewPasses() []string {
	out := make([]string, 0, len(knownReviewPasses))
	for p := range knownReviewPasses {
		out = append(out, p)
	}
	return out
}

// ReviewOpenFinding is one must-fix finding a previous review round reported
// and that was still open when that round ended. The runner passes the list
// back in on the next round so the reviewer states, per finding, whether the
// problem is still in the code.
//
// Key is the identity: the runner matches the status the reviewer reports back
// against the key it handed over, so a finding cannot quietly change identity
// between rounds and read as a new, lower-severity one.
type ReviewOpenFinding struct {
	Key       string `json:"key"`
	Parameter string `json:"parameter"`
	Severity  string `json:"severity"`
	Location  string `json:"location"`
	What      string `json:"what"`
}

// loadReviewInputs reads and validates the REVIEW_* half of the contract.
//
// Only REVIEW_BASE_COMMITS is strictly required: without a baseline there is
// no diff, and a review of nothing would report a clean bill of health for
// work it never saw — the one failure mode a review must not have. Everything
// else has a defensible default.
func (c *Config) loadReviewInputs() error {
	c.ReviewSpec = strings.TrimSpace(os.Getenv("REVIEW_SPEC"))
	c.ReviewPasses = parseReviewPasses(os.Getenv("REVIEW_PASSES"))

	// Strict "1"/"true" only — not parseBoolEnv's looser set. This flag
	// RELAXES a driver's own sandbox, so anything that isn't an unambiguous
	// yes from the runner leaves the stricter path in place.
	c.ReviewReadOnlyMounts = parseStrictBoolEnv("REVIEW_READONLY_MOUNTS")

	// REVIEW_OPEN_FINDINGS is optional (round 1 has none), but malformed JSON
	// fails the load the way REVIEW_BASE_COMMITS does: the runner meant to
	// hand over open must-fix findings, and silently reviewing without them
	// would let a round report a change clean while the same problem stands.
	openFindings, err := parseReviewOpenFindings(os.Getenv("REVIEW_OPEN_FINDINGS"))
	if err != nil {
		return err
	}
	c.ReviewOpenFindings = openFindings

	raw := strings.TrimSpace(os.Getenv("REVIEW_BASE_COMMITS"))
	if raw == "" {
		return fmt.Errorf("REVIEW_BASE_COMMITS is required in %s mode", ModeReview)
	}
	commits := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &commits); err != nil {
		return fmt.Errorf("invalid REVIEW_BASE_COMMITS: %w", err)
	}
	for dir, sha := range commits {
		if strings.TrimSpace(dir) == "" || strings.TrimSpace(sha) == "" {
			return fmt.Errorf("REVIEW_BASE_COMMITS has an empty repository directory or commit")
		}
		// Every key is joined onto WORK_DIR and handed to git -C. A key
		// that escapes the work dir ("..", an absolute path) would point
		// the review at a repository outside the Step's workspace, so it
		// is rejected here rather than resolved.
		if !filepath.IsLocal(dir) {
			return fmt.Errorf("REVIEW_BASE_COMMITS key %q must be a path inside WORK_DIR", dir)
		}
	}
	if len(commits) == 0 {
		return fmt.Errorf("REVIEW_BASE_COMMITS names no repositories")
	}
	c.ReviewBaseCommits = commits

	// REVIEW_ROUND labels the round. Absent or unreadable means the first
	// one — a wrong label is not worth failing a review over.
	c.ReviewRound = 1
	if round, err := strconv.Atoi(strings.TrimSpace(os.Getenv("REVIEW_ROUND"))); err == nil && round > 0 {
		c.ReviewRound = round
	}
	return nil
}

// parseReviewOpenFindings reads the JSON array in REVIEW_OPEN_FINDINGS.
//
// Absent or empty is no findings — the shape of round 1. Anything present but
// unparseable is an error: a runner that sent a list meant the review to see
// it, and a review that silently dropped it would re-report or re-rate the
// same problems as if it had never seen them.
//
// An entry with no key is dropped: the key is how the runner matches the
// status back to the finding it asked about, and a status it cannot match is
// indistinguishable from one that never arrived.
func parseReviewOpenFindings(raw string) ([]ReviewOpenFinding, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var parsed []ReviewOpenFinding
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("invalid REVIEW_OPEN_FINDINGS: %w", err)
	}
	out := make([]ReviewOpenFinding, 0, len(parsed))
	for _, f := range parsed {
		if strings.TrimSpace(f.Key) == "" {
			continue
		}
		f.Key = strings.TrimSpace(f.Key)
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// parseReviewPasses splits the comma-separated pass list, dropping empties,
// duplicates and names this release cannot run, while preserving order.
//
// An unknown name is DROPPED WITH A WARNING rather than accepted. Carried
// through, it would reach the prompt as a pass the agent is asked to run and
// the trailer instruction as a parameter it may report against — producing
// findings under a parameter no consumer can map, which the runner then
// annotates rather than acts on. The warning is what makes a newer runner
// talking to an older image visible instead of merely quiet.
//
// An empty list — whether unset or emptied by the filter — is the default set
// rather than "no passes": a review asked to run with no passes at all would
// report nothing and look clean.
func parseReviewPasses(raw string) []string {
	seen := map[string]bool{}
	var out []string
	var dropped []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		if !knownReviewPasses[p] {
			dropped = append(dropped, p)
			continue
		}
		out = append(out, p)
	}
	if len(dropped) > 0 {
		fmt.Fprintf(os.Stderr, "[agentbox] review: ignoring unknown REVIEW_PASSES %s; this image can run %s\n",
			strings.Join(dropped, ", "), strings.Join(defaultReviewPasses, ", "))
	}
	if len(out) == 0 {
		return append([]string{}, defaultReviewPasses...)
	}
	return out
}

// agentVersionForType reads the env var that holds the pinned version
// for the given agent. Unknown types return "" (the Driver lookup
// will reject them later with a clearer error).
func agentVersionForType(agentType string) string {
	switch agentType {
	case "claude-code":
		return os.Getenv("CLAUDE_CODE_VERSION")
	case "codex":
		return os.Getenv("CODEX_VERSION")
	case "opencode":
		return os.Getenv("OPENCODE_VERSION")
	}
	return ""
}

// parseNoActivityTimeout returns the default for "", zero for "0", or
// a parsed Go duration. Negatives and non-durations are errors.
func parseNoActivityTimeout(v string) (time.Duration, error) {
	if v == "" {
		return defaultNoActivityTimeout, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid NO_ACTIVITY_TIMEOUT %q: %w", v, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("NO_ACTIVITY_TIMEOUT must be non-negative, got %s", d)
	}
	return d, nil
}

// loadCredentials validates that the credentials the chosen agent needs
// are present, failing fast at startup. The agent CLI itself reads its key
// from the inherited process env (buildEnv forwards os.Environ); this is a
// presence check, not a hand-off.
func (c *Config) loadCredentials() error {
	switch c.AgentType {
	case "codex":
		return c.loadCodexCredentials()
	case "opencode":
		return c.loadOpencodeCredentials()
	}
	return c.loadClaudeCredentials()
}

// loadCodexCredentials requires OPENAI_API_KEY — the key the Codex CLI
// actually authenticates with; without it codex falls back to interactive
// ChatGPT login (blocked by the proxy) and 401s on api.openai.com.
// CODEX_API_KEY — the original contract name, which nothing reads — is
// accepted as a legacy alias and mapped onto OPENAI_API_KEY when only it is
// set, so control planes that predate the rename keep authenticating.
// OpenAI direct only in v1 (no provider dispatch).
func (c *Config) loadCodexCredentials() error {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
		return nil
	}
	if legacy := strings.TrimSpace(os.Getenv("CODEX_API_KEY")); legacy != "" {
		if err := os.Setenv("OPENAI_API_KEY", legacy); err != nil {
			return fmt.Errorf("error mapping CODEX_API_KEY to OPENAI_API_KEY: %w", err)
		}
		return nil
	}
	return fmt.Errorf("OPENAI_API_KEY is required for codex (CODEX_API_KEY is accepted as a legacy alias)")
}

// loadOpencodeCredentials validates the API key for the provider implied by the
// selected model. opencode is multi-provider: the model is a "provider/model"
// id (e.g. "anthropic/claude-sonnet-4-6"), so the key it needs depends on the
// provider prefix. For a known provider the matching key must be present
// (fail-fast, mirroring the codex/claude checks). For an unknown or prefix-less
// model this is lenient — opencode autodetects whatever provider key is in the
// env and 401s at runtime if none matches (the parser flags the auth failure →
// exit 2). The agent CLI reads the key from the inherited env; this is a
// presence check, not a hand-off.
func (c *Config) loadOpencodeCredentials() error {
	provider, _, _ := strings.Cut(c.Model, "/")
	envKey := opencodeProviderEnvKey(provider)
	if envKey == "" {
		return nil
	}
	if strings.TrimSpace(os.Getenv(envKey)) == "" {
		return fmt.Errorf("%s is required for opencode model %q", envKey, c.Model)
	}
	return nil
}

// opencodeProviderEnvKey maps an opencode provider id to the env var holding its
// API key, for the common providers. Returns "" for providers not in the table
// (the lenient path). The names follow opencode's provider conventions and
// should be confirmed per provider; GEMINI_API_KEY in particular has also been
// GOOGLE_GENERATIVE_AI_API_KEY historically. Kept roughly aligned with
// providerHostFromModel in internal/opencode (deliberately not shared — config
// is imported by drivers, so importing the driver here would cycle).
func opencodeProviderEnvKey(provider string) string {
	switch provider {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "openai":
		return "OPENAI_API_KEY"
	case "openrouter":
		return "OPENROUTER_API_KEY"
	// Keyed on opencode's provider id ("novita-ai"), not our catalogue key
	// ("novita"). Must stay in step with providerHostFromModel in
	// internal/opencode — the two tables are deliberately not shared to avoid
	// an import cycle, so a provider added to one and not the other either
	// fails the credential check or gets its egress denied.
	case "novita-ai":
		return "NOVITA_API_KEY"
	case "google":
		return "GEMINI_API_KEY"
	case "groq":
		return "GROQ_API_KEY"
	case "xai":
		return "XAI_API_KEY"
	case "deepseek":
		return "DEEPSEEK_API_KEY"
	case "mistral":
		return "MISTRAL_API_KEY"
	}
	return ""
}

// loadClaudeCredentials validates the Anthropic Direct, subscription OAuth, or
// Bedrock path (exactly one must be set). Like the other loaders this is a
// presence check — the agent CLI reads the actual credential from the
// inherited env; agentbox just fails fast when none (or more than one) is set.
func (c *Config) loadClaudeCredentials() error {
	anthropicKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	oauthToken := strings.TrimSpace(os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"))
	bedrockFlag := os.Getenv("CLAUDE_CODE_USE_BEDROCK") == "1"

	set := 0
	if anthropicKey != "" {
		set++
	}
	if oauthToken != "" {
		set++
	}
	if bedrockFlag {
		set++
	}
	if set > 1 {
		return fmt.Errorf("multiple Claude credential paths set; specify exactly one of ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN, or CLAUDE_CODE_USE_BEDROCK=1")
	}

	if anthropicKey != "" {
		c.AnthropicDirect = &AnthropicDirectCreds{APIKey: anthropicKey}
		return nil
	}

	if oauthToken != "" {
		c.Subscription = &SubscriptionCreds{OAuthToken: oauthToken}
		return nil
	}

	if bedrockFlag {
		creds := &BedrockCreds{
			AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
			SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
			Region:          os.Getenv("AWS_REGION"),
		}
		if creds.AccessKeyID == "" || creds.SecretAccessKey == "" || creds.Region == "" {
			return fmt.Errorf("CLAUDE_CODE_USE_BEDROCK=1 requires AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, and AWS_REGION")
		}
		c.Bedrock = creds
		return nil
	}

	return fmt.Errorf("no credentials provided: set ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN, or CLAUDE_CODE_USE_BEDROCK=1 plus AWS credentials")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseBoolEnv reports whether the named env var holds a truthy value
// ("1", "true", or "yes", case-insensitive). Anything else, including
// unset, is false.
func parseBoolEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// parseStrictBoolEnv reports whether the named env var holds "1" or "true"
// (case-insensitive, surrounding space ignored). Anything else — including
// "yes", "0" and unset — is false. Used where a true value drops a safeguard,
// so only the two spellings a runner is contracted to send count.
func parseStrictBoolEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true":
		return true
	}
	return false
}

// loadAppendSystemPrompt reads the file named by APPEND_SYSTEM_PROMPT_FILE
// and returns its contents, to be passed inline via the agent's
// --append-system-prompt flag (the CLI has no --append-system-prompt-file
// variant). Returns "" when the env var is unset; errors when it is set
// but the file cannot be read.
func loadAppendSystemPrompt() (string, error) {
	path := strings.TrimSpace(os.Getenv("APPEND_SYSTEM_PROMPT_FILE"))
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("APPEND_SYSTEM_PROMPT_FILE %q is not readable: %w", path, err)
	}
	return string(b), nil
}
