package spec

import (
	"strings"
	"testing"
)

// block wraps JSON in a ```task-spec``` fence, matching what the agent emits.
func block(json string) string {
	return "```task-spec\n" + json + "\n```"
}

func TestExtract_Missing(t *testing.T) {
	if _, ok := Extract("a normal assistant reply, no spec block here"); ok {
		t.Error("expected ok=false when no task-spec block is present")
	}
}

func TestExtract_Valid(t *testing.T) {
	text := "Here's the plan.\n\n" + block(`{
	  "title": "Add OAuth",
	  "goal": "Add OAuth login to auth-service",
	  "context": "auth-service uses sessions today",
	  "acceptance_criteria": ["login works", "tests pass"],
	  "assumptions": ["existing provider"],
	  "out_of_scope": ["SSO"],
	  "complexity": "medium",
	  "readiness": "ready",
	  "readiness_notes": "good to go"
	}`)
	s, ok := Extract(text)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if s.Title != "Add OAuth" || s.Goal != "Add OAuth login to auth-service" {
		t.Errorf("title/goal mismatch: %+v", s)
	}
	if len(s.Acceptance) != 2 || s.Acceptance[0] != "login works" {
		t.Errorf("acceptance = %v", s.Acceptance)
	}
	if len(s.OutOfScope) != 1 || s.OutOfScope[0] != "SSO" {
		t.Errorf("outOfScope = %v", s.OutOfScope)
	}
	if s.Readiness != "ready" {
		t.Errorf("readiness = %q", s.Readiness)
	}
	if s.Complexity != "medium" {
		t.Errorf("complexity = %q, want medium", s.Complexity)
	}
	if s.Notes != "good to go" {
		t.Errorf("notes = %q", s.Notes)
	}
	if !strings.Contains(s.Raw, "Add OAuth") {
		t.Error("Raw should preserve the JSON payload")
	}
}

func TestExtract_InvalidJSON(t *testing.T) {
	if _, ok := Extract(block(`{not valid json`)); ok {
		t.Error("expected ok=false for invalid JSON")
	}
}

func TestExtract_EmptyBlockIgnored(t *testing.T) {
	if _, ok := Extract(block(`{}`)); ok {
		t.Error("expected ok=false for a content-free spec block")
	}
}

func TestExtract_MultipleLatestWins(t *testing.T) {
	text := block(`{"title":"v1","goal":"first"}`) +
		"\n\nmore discussion\n\n" +
		block(`{"title":"v2","goal":"second"}`)
	s, ok := Extract(text)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if s.Title != "v2" || s.Goal != "second" {
		t.Errorf("latest block should win, got %+v", s)
	}
}

func TestExtract_LatestInvalidFallsBackToPrevValid(t *testing.T) {
	text := block(`{"title":"good","goal":"valid one"}`) + "\n\n" + block(`{broken`)
	s, ok := Extract(text)
	if !ok {
		t.Fatal("expected ok=true (fall back to the previous valid block)")
	}
	if s.Title != "good" {
		t.Errorf("expected the previous valid block, got %+v", s)
	}
}

func TestStrip(t *testing.T) {
	text := "Here is my analysis.\n\n" + block(`{"title":"T","goal":"G"}`) + "\n\nthanks!"
	got := Strip(text)
	if strings.Contains(got, "task-spec") || strings.Contains(got, "\"goal\"") {
		t.Errorf("Strip should remove the spec block, got: %q", got)
	}
	if !strings.Contains(got, "Here is my analysis.") || !strings.Contains(got, "thanks!") {
		t.Errorf("Strip should keep prose, got: %q", got)
	}
}

func TestStrip_NoBlock(t *testing.T) {
	text := "just prose, nothing to strip"
	if got := Strip(text); got != text {
		t.Errorf("Strip with no block = %q, want unchanged", got)
	}
}

// A mid-sentence mention of the fence marker (the agent describing the
// block format in prose) must not open a block: it would otherwise run
// to the real block's opening fence and take the spec with it.
func TestExtract_ProseMentionDoesNotOpenBlock(t *testing.T) {
	text := "stripTaskSpec is fence-based (```task-spec\nfences), so a tag needs its own pattern.\n\n" +
		"4. more prose the strip must keep\n\n" +
		block(`{"title":"real","goal":"the actual spec"}`)
	s, ok := Extract(text)
	if !ok || s.Title != "real" {
		t.Fatalf("expected the real block, got ok=%v %+v", ok, s)
	}
	got := Strip(text)
	if !strings.Contains(got, "fence-based (```task-spec") || !strings.Contains(got, "4. more prose") {
		t.Errorf("Strip should keep the prose mention and what follows it, got: %q", got)
	}
	if strings.Contains(got, "\"goal\"") {
		t.Errorf("Strip should remove the real block, got: %q", got)
	}
}

// An opening fence on its own line that the agent never closed must not
// claim the closing fence of the real block that follows it.
func TestExtract_UnclosedEarlierOpeningIgnored(t *testing.T) {
	text := "```task-spec\n{\"title\":\"abandoned\"\n\nprose\n\n" +
		block(`{"title":"real","goal":"g"}`)
	s, ok := Extract(text)
	if !ok || s.Title != "real" {
		t.Fatalf("expected the real block, got ok=%v %+v", ok, s)
	}
}

// Fences are only fences at the start of a line: an indented or trailing
// closing marker does not end a block, and a lone opening is not a block.
func TestExtract_UnclosedBlockIsNotABlock(t *testing.T) {
	if _, ok := Extract("```task-spec\n{\"title\":\"t\",\"goal\":\"g\"}"); ok {
		t.Error("expected ok=false for a block with no closing fence")
	}
	if got := Strip("prose\n\n```task-spec\n{\"title\":\"t\"}"); got != "prose\n\n```task-spec\n{\"title\":\"t\"}" {
		t.Errorf("Strip should leave an unclosed block alone, got %q", got)
	}
}

// The fence marker inside a JSON string value (the agent quoting the block
// format in an assumption) is mid-line, so it is not a closing fence: the
// block must end at the real closing fence and parse whole. This was the
// second production failure on 2026-09-18.
func TestExtract_FenceMarkerInsideJSONString(t *testing.T) {
	text := "Below is the spec.\n\n" +
		block(`{"title":"t","goal":"g","assumptions":["strip is fence-based (` + "```task-spec```" + `); tags need their own pattern"]}`)
	s, ok := Extract(text)
	if !ok || s.Title != "t" || len(s.Assumptions) != 1 {
		t.Fatalf("expected the whole block to parse, got ok=%v %+v", ok, s)
	}
	if got := Strip(text); got != "Below is the spec." {
		t.Errorf("Strip should remove the whole block, got %q", got)
	}
}
