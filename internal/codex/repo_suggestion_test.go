package codex

import (
	"io"
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/agent"
)

const suggestionBlock = "<repo-suggestion>\n" +
	`{"repositories":[{"name":"deployment-io/kit","reason":"the Session model lives here","confidence":"high"}]}` +
	"\n</repo-suggestion>"

// A single message carrying BOTH machine-only blocks forwards both payloads and
// leaves display text with neither.
func TestFinalizeMessage_BothBlocks(t *testing.T) {
	fio := &codexFakeIO{}
	text := "Here is the plan.\n\n```task-spec\n{\"title\":\"T\",\"goal\":\"G\"}\n```\n\n" + suggestionBlock

	(&rpcReader{log: io.Discard}).finalizeMessage(fio, text, false)

	if len(fio.specs) != 1 || fio.specs[0].Goal != "G" {
		t.Errorf("spec = %v, want one with goal G", fio.specs)
	}
	if len(fio.suggestions) != 1 || len(fio.suggestions[0].Repositories) != 1 ||
		fio.suggestions[0].Repositories[0].Name != "deployment-io/kit" {
		t.Fatalf("suggestion = %+v, want one naming deployment-io/kit", fio.suggestions)
	}
	if fio.suggestions[0].Repositories[0].Confidence != "high" {
		t.Errorf("confidence = %q, want high", fio.suggestions[0].Repositories[0].Confidence)
	}
	if len(fio.finals) != 1 {
		t.Fatalf("finals = %v, want 1", fio.finals)
	}
	if strings.Contains(fio.finals[0], "task-spec") || strings.Contains(fio.finals[0], "repo-suggestion") {
		t.Errorf("display text still carries a block: %q", fio.finals[0])
	}
	if fio.finals[0] != "Here is the plan." {
		t.Errorf("display text = %q, want the prose alone", fio.finals[0])
	}
	if len(fio.chunks) != 1 || fio.chunks[0] != "Here is the plan." {
		t.Errorf("chunks = %v, want the stripped prose once", fio.chunks)
	}
}

// A suggestion-only message forwards the suggestion and nothing chat-visible —
// the existing spec-only behaviour.
func TestFinalizeMessage_SuggestionOnly(t *testing.T) {
	fio := &codexFakeIO{}
	(&rpcReader{log: io.Discard}).finalizeMessage(fio, suggestionBlock, false)

	if len(fio.suggestions) != 1 {
		t.Fatalf("suggestions = %+v, want 1", fio.suggestions)
	}
	if len(fio.chunks) != 0 || len(fio.finals) != 0 {
		t.Errorf("a suggestion-only message must forward no chat text: chunks=%v finals=%v", fio.chunks, fio.finals)
	}
	if len(fio.specs) != 0 {
		t.Errorf("no spec block was present: %v", fio.specs)
	}
}

// A message with no blocks at all leaves the suggestion sink untouched — an
// agent that never suggests anything writes nothing.
func TestFinalizeMessage_NoBlocks(t *testing.T) {
	fio := &codexFakeIO{}
	(&rpcReader{log: io.Discard}).finalizeMessage(fio, "Just answering your question.", false)

	if len(fio.suggestions) != 0 || len(fio.specs) != 0 {
		t.Errorf("plain prose forwarded a payload: suggestions=%+v specs=%v", fio.suggestions, fio.specs)
	}
	if len(fio.finals) != 1 || fio.finals[0] != "Just answering your question." {
		t.Errorf("finals = %v", fio.finals)
	}
}

var _ agent.InteractiveSink = (*codexFakeIO)(nil)
