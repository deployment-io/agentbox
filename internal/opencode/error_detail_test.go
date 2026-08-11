package opencode

import (
	"encoding/json"
	"testing"
)

// opencode's distinctive failures arrive as a NAME with little or no message —
// ProviderModelNotFoundError being the one that matters, since it is what an
// unroutable model id produces. Discarding it left a live run reporting only
// "opencode opencode reported an error", with nothing to act on.
func TestErrorMessage_KeepsTheName(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			"name and message",
			`{"error":{"name":"ProviderModelNotFoundError","message":"amazon-bedrock/qwen3-coder-480b"}}`,
			"ProviderModelNotFoundError: amazon-bedrock/qwen3-coder-480b",
		},
		{
			// The case that produced a useless error: a name, no message.
			"name only",
			`{"error":{"name":"ProviderModelNotFoundError"}}`,
			"ProviderModelNotFoundError",
		},
		{"message only", `{"error":{"message":"rate limit exceeded"}}`, "rate limit exceeded"},
		// "" rather than a placeholder: a generic reason is WORSE than none,
		// because failureMessage prefers FailureReason over the stderr tail and
		// would suppress the only remaining explanation.
		{"neither", `{"error":{}}`, ""},
		{"no error block", `{"type":"error"}`, ""},
	}
	for _, c := range cases {
		var ev opencodeEvent
		if err := json.Unmarshal([]byte(c.raw), &ev); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.name, err)
		}
		if got := ev.errorMessage(); got != c.want {
			t.Errorf("%s: errorMessage() = %q, want %q", c.name, got, c.want)
		}
	}
}
