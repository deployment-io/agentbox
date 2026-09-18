package reposuggestion

import (
	"fmt"
	"strings"
	"testing"
)

func block(body string) string {
	return openTag + "\n" + body + "\n" + closeTag
}

func TestExtract_OneBlock(t *testing.T) {
	text := "You'll want the shared model too.\n\n" + block(
		`{"repositories":[{"name":"deployment-io/kit","reason":"the Session model lives here","confidence":"high"}]}`)

	got, ok := Extract(text)
	if !ok {
		t.Fatal("a well-formed block should extract")
	}
	if len(got.Repositories) != 1 {
		t.Fatalf("repositories = %d, want 1", len(got.Repositories))
	}
	r := got.Repositories[0]
	if r.Name != "deployment-io/kit" || r.Reason != "the Session model lives here" || r.Confidence != "high" {
		t.Errorf("parsed = %+v", r)
	}
	if strip := Strip(text); strip != "You'll want the shared model too." {
		t.Errorf("Strip left %q", strip)
	}
}

// Newest-first, like spec.Extract: the latest valid block wins, and an earlier
// one is the fallback only when the newer block is malformed.
func TestExtract_LatestValidBlockWins(t *testing.T) {
	older := block(`{"repositories":[{"name":"owner/old"}]}`)
	newer := block(`{"repositories":[{"name":"owner/new"}]}`)

	got, ok := Extract("first\n" + older + "\nthen\n" + newer)
	if !ok || got.Repositories[0].Name != "owner/new" {
		t.Fatalf("latest block should win, got %+v (ok=%v)", got.Repositories, ok)
	}

	// The newest block is unparseable JSON — fall back to the older one rather
	// than losing the suggestion entirely.
	got, ok = Extract("first\n" + older + "\nthen\n" + block(`{"repositories":[{"name":`))
	if !ok || got.Repositories[0].Name != "owner/old" {
		t.Fatalf("a malformed newest block should fall back to the older one, got %+v (ok=%v)", got.Repositories, ok)
	}

	// A newest block that parses but names nothing must not mask the older one
	// either — otherwise an empty emission blanks a good suggestion.
	got, ok = Extract("first\n" + older + "\nthen\n" + block(`{"repositories":[]}`))
	if !ok || got.Repositories[0].Name != "owner/old" {
		t.Fatalf("an empty newest block should fall back, got %+v (ok=%v)", got.Repositories, ok)
	}
}

func TestExtract_NoUsableSuggestion(t *testing.T) {
	for _, tt := range []struct {
		name string
		text string
	}{
		{"no block at all", "just prose about repositories"},
		{"unparseable json", block(`{"repositories":`)},
		{"no repositories key", block(`{}`)},
		{"empty list", block(`{"repositories":[]}`)},
		{"entries with no name", block(`{"repositories":[{"reason":"why","confidence":"high"},{"name":"  "}]}`)},
		{"unclosed block", openTag + `{"repositories":[{"name":"owner/repo"}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := Extract(tt.text); ok {
				t.Errorf("%s should not extract", tt.name)
			}
		})
	}
}

// A tag named in prose before the real block must not swallow it — the hazard
// internal/spec hit in production with a prose-mentioned fence.
func TestExtract_ProseMentionDoesNotMaskTheBlock(t *testing.T) {
	text := "I'd emit a " + openTag + " block if you want.\n\n" +
		block(`{"repositories":[{"name":"owner/repo","reason":"needed"}]}`)
	got, ok := Extract(text)
	if !ok || got.Repositories[0].Name != "owner/repo" {
		t.Fatalf("prose mention masked the block: %+v (ok=%v)", got.Repositories, ok)
	}
	if display := Strip(text); !strings.HasPrefix(display, "I'd emit a") || strings.Contains(display, "owner/repo") {
		t.Errorf("Strip removed the wrong span: %q", display)
	}
}

func TestExtract_CapsRepositoryCount(t *testing.T) {
	var entries []string
	for i := 0; i < MaxRepositories+4; i++ {
		entries = append(entries, fmt.Sprintf(`{"name":"owner/repo-%d"}`, i))
	}
	got, ok := Extract(block(`{"repositories":[` + strings.Join(entries, ",") + `]}`))
	if !ok {
		t.Fatal("should extract")
	}
	if len(got.Repositories) != MaxRepositories {
		t.Fatalf("repositories = %d, want the cap (%d)", len(got.Repositories), MaxRepositories)
	}
	// The cap keeps the FIRST entries, which is the order the agent ranked them in.
	if got.Repositories[0].Name != "owner/repo-0" || got.Repositories[MaxRepositories-1].Name != "owner/repo-4" {
		t.Errorf("truncation did not keep the leading entries: %+v", got.Repositories)
	}
}

func TestExtract_NormalizesConfidence(t *testing.T) {
	for in, want := range map[string]string{
		`"high"`: "high", `"HIGH"`: "high", `"  low "`: "low", `"medium"`: "medium",
		`"pretty sure"`: "medium", `""`: "medium",
	} {
		got, ok := Extract(block(`{"repositories":[{"name":"owner/repo","confidence":` + in + `}]}`))
		if !ok {
			t.Fatalf("confidence %s should still extract", in)
		}
		if got.Repositories[0].Confidence != want {
			t.Errorf("confidence %s → %q, want %q", in, got.Repositories[0].Confidence, want)
		}
	}
	// An absent confidence is the same neutral middle as an unrecognised one.
	got, _ := Extract(block(`{"repositories":[{"name":"owner/repo"}]}`))
	if got.Repositories[0].Confidence != "medium" {
		t.Errorf("absent confidence = %q, want medium", got.Repositories[0].Confidence)
	}
}

func TestExtract_CapsFieldLengths(t *testing.T) {
	// Multi-byte runes: the cap is on runes, not bytes, and truncation must not
	// split one.
	name := strings.Repeat("é", MaxNameRunes+50)
	reason := strings.Repeat("ü", MaxReasonRunes+50)
	got, ok := Extract(block(fmt.Sprintf(`{"repositories":[{"name":%q,"reason":%q}]}`, name, reason)))
	if !ok {
		t.Fatal("an oversized entry should still extract, truncated")
	}
	r := got.Repositories[0]
	if n := len([]rune(r.Name)); n != MaxNameRunes {
		t.Errorf("name runes = %d, want %d", n, MaxNameRunes)
	}
	if n := len([]rune(r.Reason)); n != MaxReasonRunes {
		t.Errorf("reason runes = %d, want %d", n, MaxReasonRunes)
	}
	if !strings.HasPrefix(name, r.Name) || !strings.HasPrefix(reason, r.Reason) {
		t.Error("truncation must keep a valid prefix")
	}
}

func TestStrip(t *testing.T) {
	one := block(`{"repositories":[{"name":"owner/a"}]}`)
	two := block(`{"repositories":[{"name":"owner/b"}]}`)
	got := Strip("before\n" + one + "\nmiddle\n" + two + "\nafter")
	if strings.Contains(got, openTag) || strings.Contains(got, "owner/") {
		t.Errorf("Strip left a block behind: %q", got)
	}
	for _, want := range []string{"before", "middle", "after"} {
		if !strings.Contains(got, want) {
			t.Errorf("Strip dropped prose %q: %q", want, got)
		}
	}
	if Strip(one) != "" {
		t.Errorf("a block-only message should strip to nothing, got %q", Strip(one))
	}
	if got := Strip("no blocks here"); got != "no blocks here" {
		t.Errorf("Strip mangled plain text: %q", got)
	}
}
