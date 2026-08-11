package opencode

import (
	"encoding/json"
	"strings"
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

// The case three releases of field-picking could not reach: a name with the
// detail somewhere else. "APIError" alone told us nothing across two live
// Bedrock failures while the useful part sat in fields the struct never
// decoded.
func TestErrorMessage_FallsBackToTheRawPayload(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			// The shape that defeated us. Whatever opencode nests under data
			// now reaches a human.
			"name plus unknown fields",
			`{"error":{"name":"APIError","data":{"statusCode":403,"responseBody":"AccessDeniedException"}}}`,
			`{"name":"APIError","data":{"statusCode":403,"responseBody":"AccessDeniedException"}}`,
		},
		{
			// Nothing beyond the name — raw JSON would be a noisier way of
			// writing the same word.
			"name only stays clean",
			`{"error":{"name":"ProviderModelNotFoundError"}}`,
			"ProviderModelNotFoundError",
		},
		{
			// Null extras are not extras.
			"name with null fields stays clean",
			`{"error":{"name":"APIError","data":null}}`,
			"APIError",
		},
		{
			// A message wins outright — a sentence beats a JSON dump.
			"message wins over extras",
			`{"error":{"name":"APIError","message":"model not enabled","data":{"x":1}}}`,
			"APIError: model not enabled",
		},
	}
	for _, c := range cases {
		var ev opencodeEvent
		if err := json.Unmarshal([]byte(c.raw), &ev); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.name, err)
		}
		if got := ev.errorMessage(); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}

// A provider can echo a whole request body back; this lands in a Job document.
func TestErrorMessage_RawPayloadIsCapped(t *testing.T) {
	big := `{"error":{"name":"APIError","data":"` + strings.Repeat("x", 5000) + `"}}`
	var ev opencodeEvent
	if err := json.Unmarshal([]byte(big), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := ev.errorMessage()
	if len([]rune(got)) > 401 {
		t.Errorf("error string is %d runes; it must be capped", len([]rune(got)))
	}
	if !strings.Contains(got, "APIError") {
		t.Errorf("cap dropped the head of the payload, which is where the useful fields are: %q", got)
	}
}

// The real payload from a live Bedrock run, trimmed only of response headers.
// Everything asserted below is verbatim from AWS.
const liveBedrockAccessDenied = `{"error":{"name":"APIError","data":{` +
	`"message":"undefined: anthropic.claude-sonnet-5 is not available for this account. ` +
	`You can explore other available models on Amazon Bedrock. For additional access options, ` +
	`contact AWS Sales at https://aws.amazon.com/contact-us/sales-support/",` +
	`"statusCode":403,"isRetryable":false}}}`

// The sentence lives under data.message, with the TOP-LEVEL message empty —
// which is why reading only {message, name} reported a bare "APIError" three
// times running while the diagnosis sat one level down.
func TestErrorMessage_ReadsTheNestedProviderMessage(t *testing.T) {
	t.Setenv("AWS_REGION", "eu-west-1")
	var ev opencodeEvent
	if err := json.Unmarshal([]byte(liveBedrockAccessDenied), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := ev.errorMessage()

	// AWS names the model; that must survive.
	if !strings.Contains(got, "anthropic.claude-sonnet-5") {
		t.Errorf("lost the model id: %q", got)
	}
	// The SDK's "undefined:" prefix is noise and must not.
	if strings.Contains(got, "undefined:") {
		t.Errorf("kept the SDK's undefined prefix: %q", got)
	}
	// And the remedy AWS does not mention.
	for _, want := range []string{"Bedrock console", "eu-west-1", "direct provider"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q from an actionable message: %q", want, got)
		}
	}
}

// ⚠️ 403 alone must NOT trigger the hint. A bad signature or a denied IAM
// action is also 403, and rewriting those as "enable model access" sends
// someone to the wrong console page — the same conflation that made the
// runner's first fail-fast draft blame model access for missing credentials.
func TestBedrockAccessHint_OnlyForTheAccessSentence(t *testing.T) {
	cases := []struct {
		name   string
		detail string
		status int
		want   bool
	}{
		{"the real one", "anthropic.claude-sonnet-5 is not available for this account.", 403, true},
		{"other 403", "The security token included in the request is invalid", 403, false},
		{"signature failure", "Signature expired", 403, false},
		{"right words, wrong status", "x is not available for this account", 500, false},
		{"empty", "", 403, false},
	}
	for _, c := range cases {
		if got := bedrockAccessHint(c.detail, c.status) != ""; got != c.want {
			t.Errorf("%s: hint=%v, want %v", c.name, got, c.want)
		}
	}
}

// Without AWS_REGION the message still has to read sensibly.
func TestBedrockAccessHint_DegradesWithoutRegion(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	got := bedrockAccessHint("m is not available for this account.", 403)
	if !strings.Contains(got, "this region") || strings.Contains(got, "in  ,") {
		t.Errorf("unset region reads badly: %q", got)
	}
}
