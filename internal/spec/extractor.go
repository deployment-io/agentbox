// Package spec extracts the structured task-spec an interactive agent
// maintains across a conversation. The agent is instructed (via its
// appended system prompt) to keep a fenced ```task-spec``` block of JSON
// at the end of its messages; this package finds the latest valid block
// and parses it into an agent.SpecSnapshot.
//
// Agent-agnostic: the block convention is part of how we prompt any chat
// agent, not specific to Claude Code, so this lives outside the agent
// packages.
package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/deployment-io/agentbox/internal/agent"
)

// fence is the Markdown code-fence delimiter. Defined as a const so the
// regex below can be an interpreted string literal (Go raw strings can't
// contain backticks).
const fence = "```"

// openRe matches an opening ```task-spec fence on a line of its own and
// closeRe a closing ``` fence on a line of its own. Both are anchored to
// the start of a line: a fence marker mentioned mid-sentence (the agent
// describing the block format in prose) must not open a block, or it
// swallows everything up to the real block's opening fence and the spec
// is lost — seen in production 2026-09-18.
var (
	openRe  = regexp.MustCompile("(?m)^" + fence + `task-spec[ \t]*$`)
	closeRe = regexp.MustCompile("(?m)^" + fence + `[ \t]*$`)
)

// span is one fenced block: [start, end) covers both fences; body is the
// text between them.
type span struct {
	start, end int
	body       string
}

// blocks returns every well-formed ```task-spec``` block in text, newest
// first. Openings are paired with the nearest closing fence that follows
// them, scanning from the last opening backwards so that an earlier
// unclosed opening (a prose mention at line start, or a block the agent
// never closed) can neither claim the newest block's closing fence nor
// mask the block itself. An opening with no closing fence after it is
// not a block.
func blocks(text string) []span {
	opens := openRe.FindAllStringIndex(text, -1)
	var out []span
	limit := len(text) // blocks must end before the previously found one starts
	for i := len(opens) - 1; i >= 0; i-- {
		o := opens[i]
		if o[1] >= limit {
			continue // this opening sits inside a block already claimed
		}
		rest := text[o[1]:limit]
		c := closeRe.FindStringIndex(rest)
		if c == nil {
			continue
		}
		out = append(out, span{
			start: o[0],
			end:   o[1] + c[1],
			body:  rest[:c[0]],
		})
		limit = o[0]
	}
	return out
}

// parsedSpec mirrors the JSON the agent emits inside the block. Field
// names track the system-prompt schema.
type parsedSpec struct {
	Title       string   `json:"title"`
	Goal        string   `json:"goal"`
	Context     string   `json:"context"`
	Acceptance  []string `json:"acceptance_criteria"`
	Assumptions []string `json:"assumptions"`
	OutOfScope  []string `json:"out_of_scope"`
	Readiness   string   `json:"readiness"`
	Notes       string   `json:"readiness_notes"`
	Complexity  string   `json:"complexity"`
}

// Extract returns the task-spec parsed from the latest valid
// ```task-spec``` block in text, and ok=true. It returns ok=false when no
// block is present, when no block parses as JSON, or when the latest
// parseable block carries no substance (no title and no goal). Blocks are
// scanned newest-first, so the most recent valid spec wins and an older
// one is used only as a fallback when newer blocks are malformed.
func Extract(text string) (agent.SpecSnapshot, bool) {
	s, ok, _ := Inspect(text)
	return s, ok
}

// Inspect returns what Extract returns plus a problem: a one-line
// description of why the NEWEST ```task-spec``` block in text can't be
// read (invalid JSON, or a field of the wrong type), naming the error, its
// line and column within the block, and an excerpt around it. problem is
// empty when the newest block parses, when it parses but is content-free
// (skipped, as in Extract), and when text has no block at all. A newer
// broken block after an older valid one yields both the older spec and a
// problem: the spec the agent meant to send is the broken one.
//
// The problem is built only from fixed wording and the block's own text,
// and capped at maxProblemLen characters.
func Inspect(text string) (spec agent.SpecSnapshot, ok bool, problem string) {
	for i, b := range blocks(text) {
		raw := strings.TrimSpace(b.body)
		var p parsedSpec
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			if i == 0 {
				problem = describeProblem(b.body, err)
			}
			continue
		}
		// A content-free block ({} or whitespace) isn't a spec — keep
		// looking so it doesn't mask an earlier substantive one and so
		// the dashboard's "no spec yet" state stays accurate.
		if p.Title == "" && p.Goal == "" {
			continue
		}
		return agent.SpecSnapshot{
			Title:       p.Title,
			Goal:        p.Goal,
			Context:     p.Context,
			Acceptance:  p.Acceptance,
			Assumptions: p.Assumptions,
			OutOfScope:  p.OutOfScope,
			Readiness:   p.Readiness,
			Notes:       p.Notes,
			Complexity:  p.Complexity,
			Raw:         raw,
		}, true, problem
	}
	return agent.SpecSnapshot{}, false, problem
}

const (
	// maxProblemLen caps the whole problem string, in characters. It is
	// quoted back to the agent and may be shown to the user.
	maxProblemLen = 300
	// excerptRadius is how many characters of the block are shown on each
	// side of the error position.
	excerptRadius = 30
)

// describeProblem turns an unmarshal error on a block's body into the
// one-line problem Inspect reports. Line and column are 1-based and count
// from the first line of JSON inside the block (the line after the
// opening fence).
func describeProblem(body string, err error) string {
	// The body starts with the newline that ends the opening fence's line;
	// drop it so line 1 is the block's first content line.
	body = strings.TrimPrefix(body, "\n")
	lead := len(body) - len(strings.TrimLeft(body, " \t\r\n"))
	raw := strings.TrimSpace(body)

	var (
		msg    string
		offset int64
	)
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syn):
		msg = syn.Error()
		offset = syn.Offset - 1 // Offset is just past the offending byte
	case errors.As(err, &typ):
		msg = fmt.Sprintf("%q is %s, want %s", typ.Field, article(typ.Value), describeType(typ.Type))
		offset = typ.Offset - 1 // Offset is just past the offending value
	default:
		msg = "invalid JSON"
	}
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(raw)) {
		offset = int64(len(raw))
	}
	pos := lead + int(offset)

	before := body[:pos]
	line := strings.Count(before, "\n") + 1
	col := utf8.RuneCountInString(before[strings.LastIndex(before, "\n")+1:]) + 1

	out := fmt.Sprintf("%s at line %d, column %d", msg, line, col)
	if ex := excerpt(raw, int(offset)); ex != "" {
		out += ", near: " + ex
	}
	return capRunes(out, maxProblemLen)
}

// excerpt returns about 2*excerptRadius characters of raw centred on byte
// offset pos, on one line, with "…" marking a cut at either end.
func excerpt(raw string, pos int) string {
	rs := []rune(raw)
	at := utf8.RuneCountInString(raw[:pos])
	start, end := max(at-excerptRadius, 0), min(at+excerptRadius, len(rs))
	ex := strings.Join(strings.Fields(string(rs[start:end])), " ")
	if ex == "" {
		return ""
	}
	if start > 0 {
		ex = "…" + ex
	}
	if end < len(rs) {
		ex += "…"
	}
	return ex
}

// article prefixes a JSON value kind as encoding/json names it ("string",
// "number", "array", "object", "bool") with "a" / "an".
func article(kind string) string {
	switch kind {
	case "array", "object":
		return "an " + kind
	case "bool":
		return "a boolean"
	}
	return "a " + kind
}

// describeType names the JSON shape a parsedSpec field expects.
func describeType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Slice:
		return "an array of " + describeType(t.Elem()) + "s"
	case reflect.String:
		return "string"
	}
	return t.String()
}

// capRunes truncates s to at most n characters, ending in "…" when cut.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// Strip removes all ```task-spec``` blocks from text and trims surrounding
// whitespace, leaving the user-facing prose. Used to keep the machine-only
// spec block out of the rendered chat message.
func Strip(text string) string {
	// blocks is newest-first, so removing in order keeps earlier offsets valid.
	for _, b := range blocks(text) {
		text = text[:b.start] + text[b.end:]
	}
	return strings.TrimSpace(text)
}
