package agent

import (
	"testing"

	"github.com/deployment-io/agentbox/internal/result"
)

func TestLimitExceeded(t *testing.T) {
	cases := []struct {
		name        string
		st          ParsedState
		maxTurns    int
		tokenBudget int
		wantHit     bool
	}{
		{"no limits set never trips", ParsedState{Turns: 100, TokenUsage: result.TokenUsage{InputTokens: 1000000}}, 0, 0, false},
		{"under turn limit", ParsedState{Turns: 5}, 10, 0, false},
		{"at turn limit", ParsedState{Turns: 10}, 10, 0, true},
		{"over turn limit", ParsedState{Turns: 11}, 10, 0, true},
		{"under token budget", ParsedState{TokenUsage: result.TokenUsage{InputTokens: 100, OutputTokens: 50}}, 0, 1000, false},
		{"at token budget", ParsedState{TokenUsage: result.TokenUsage{InputTokens: 600, OutputTokens: 400}}, 0, 1000, true},
		{"over token budget", ParsedState{TokenUsage: result.TokenUsage{InputTokens: 900, OutputTokens: 400}}, 0, 1000, true},
		// The claude mid-run state: its parser reports 0 turns / 0 tokens
		// until the very end, so even with caps set the watcher must not
		// preempt it. This is what keeps the limit enforcement agent-agnostic
		// without changing claude's behavior.
		{"zero counters never trip (claude mid-run)", ParsedState{}, 30, 1_000_000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := limitExceeded(tc.st, tc.maxTurns, tc.tokenBudget)
			if ok != tc.wantHit {
				t.Errorf("limitExceeded() ok = %v, want %v (msg=%q)", ok, tc.wantHit, msg)
			}
			if ok && msg == "" {
				t.Error("expected a non-empty reason when a limit is hit")
			}
		})
	}
}
