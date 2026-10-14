package spec

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// productionBroken is the block seen in production 2026-10-14: a criterion
// quoting an HTML attribute with unescaped double quotes.
const productionBroken = `{
  "title": "Link the providers section",
  "goal": "Add a nav link to the LLM providers section",
  "acceptance_criteria": [
    "The nav has <a href="#llm-providers">LLM providers</a>",
    "Clicking it scrolls to the section"
  ],
  "readiness": "ready"
}`

func TestInspect_ProductionUnescapedQuotes(t *testing.T) {
	_, ok, problem := Inspect("Here's the spec.\n\n" + block(productionBroken))
	if ok {
		t.Error("ok = true, want false for the only block broken")
	}
	for _, want := range []string{
		"invalid character '#' after array element",
		"line 5, column 27",
		`<a href="#llm-providers">`,
	} {
		if !strings.Contains(problem, want) {
			t.Errorf("problem = %q, want it to contain %q", problem, want)
		}
	}
	if strings.ContainsAny(problem, "\n\r") {
		t.Errorf("problem spans lines: %q", problem)
	}
}

func TestInspect_ValidSpecNoProblem(t *testing.T) {
	s, ok, problem := Inspect(block(`{"title":"T","goal":"G"}`))
	if !ok || s.Title != "T" || problem != "" {
		t.Errorf("got ok=%v title=%q problem=%q, want ok, T, no problem", ok, s.Title, problem)
	}
}

func TestInspect_NoBlockNoProblem(t *testing.T) {
	if _, ok, problem := Inspect("just prose"); ok || problem != "" {
		t.Errorf("got ok=%v problem=%q, want neither", ok, problem)
	}
}

func TestInspect_ContentFreeBlockNoProblem(t *testing.T) {
	if _, ok, problem := Inspect(block(`{}`)); ok || problem != "" {
		t.Errorf("got ok=%v problem=%q, want neither", ok, problem)
	}
}

func TestInspect_NewerBrokenAfterOlderValid(t *testing.T) {
	text := block(`{"title":"Old","goal":"old goal"}`) + "\n\nUpdated:\n\n" + block(productionBroken)
	s, ok, problem := Inspect(text)
	if !ok || s.Title != "Old" {
		t.Errorf("got ok=%v title=%q, want the older spec", ok, s.Title)
	}
	if problem == "" {
		t.Error("problem empty, want the newer block's error")
	}
}

func TestInspect_OlderBrokenNewerValidNoProblem(t *testing.T) {
	text := block(productionBroken) + "\n\n" + block(`{"title":"New"}`)
	if s, ok, problem := Inspect(text); !ok || s.Title != "New" || problem != "" {
		t.Errorf("got ok=%v title=%q problem=%q, want New and no problem", ok, s.Title, problem)
	}
}

func TestInspect_WrongFieldType(t *testing.T) {
	_, ok, problem := Inspect(block(`{"title":"T","acceptance_criteria":"it works"}`))
	if ok {
		t.Error("ok = true, want false")
	}
	for _, want := range []string{`"acceptance_criteria" is a string, want an array of strings`, "line 1, column", "it works"} {
		if !strings.Contains(problem, want) {
			t.Errorf("problem = %q, want it to contain %q", problem, want)
		}
	}
}

func TestInspect_ProblemCapped(t *testing.T) {
	long := strings.Repeat("é", 400)
	_, _, problem := Inspect(block(`{"title":"` + long + `" "` + long + `"}`))
	if n := utf8.RuneCountInString(problem); n > maxProblemLen {
		t.Errorf("problem is %d characters, want ≤ %d: %q", n, maxProblemLen, problem)
	}
	if problem == "" {
		t.Fatal("problem empty")
	}
	// Excerpt is ~60 characters of the block, not the whole thing.
	if strings.Count(problem, "é") > 2*excerptRadius {
		t.Errorf("excerpt too long: %q", problem)
	}
	// A message whose excerpt alone exceeds the cap still fits.
	if got := capRunes(strings.Repeat("x", 500), maxProblemLen); utf8.RuneCountInString(got) != maxProblemLen || !strings.HasSuffix(got, "…") {
		t.Errorf("capRunes = %d runes", utf8.RuneCountInString(got))
	}
}
