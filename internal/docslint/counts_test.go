package docslint

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// numberWords are the spellings these pages actually use. Docs here write small
// counts as words and larger ones as digits, and both forms have drifted.
var numberWords = map[string]int{
	"two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8,
	"nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14,
	"fifteen": 15, "sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
	"twenty": 20,
	// The compound spellings, added when the explainer and tool inventories passed
	// twenty. Without them a page saying "twenty-two explainers" is read as a claim
	// of twenty, which is wrong in the direction that matters: it fails a correct
	// sentence and would pass an incorrect one the day the inventory reaches 20.
	"twenty-one": 21, "twenty-two": 22, "twenty-three": 23, "twenty-four": 24,
	"twenty-five": 25, "twenty-six": 26, "twenty-seven": 27, "twenty-eight": 28,
	"twenty-nine": 29, "thirty": 30,
}

// countSubjects maps the noun phrase a claim is about to the inventory that decides
// it. The phrases are narrow on purpose. `rule forms` and `enforceable forms` are
// listed rather than a bare `forms` because these pages use "forms" for half a dozen
// unrelated things — reference forms in HCL, input forms in INTENT.md, the two forms
// of a Go route registration — and a checker that flagged all of them would be
// answered with a waiver list longer than the thing it protects.
var countSubjects = []struct {
	pattern   string
	inventory string
}{
	{`explainers?`, "explainers"},
	{`tools?`, "MCP tools"},
	{`(?:rule|enforceable) forms?`, "rule forms"},
	{`taxonom(?:y|ies)`, "layer taxonomies"},
	{`fact kinds?`, "fact kinds"},
	{`relation kinds?`, "relation kinds"},
}

// countWaivers are count claims that deliberately do NOT track the live inventory.
//
// Each names the document, the exact phrase, and why the number is frozen. A waiver
// whose phrase has disappeared from the page fails as loudly as a wrong count: a
// stale waiver is a rule nobody is following, and it would silently un-check
// whatever sentence replaced it.
var countWaivers = []struct{ Doc, Phrase, Reason string }{
	{
		Doc:    "docs/EXPLAINERS.md",
		Phrase: "enola's eighteen explainers produced",
		Reason: "a measurement from a specific corpus run, not a claim about the live inventory: the finding count was produced by the eighteen that existed then, and re-pointing the number at today's inventory would make the sentence describe a run that never happened.",
	},
	{
		Doc:    "ARCHITECTURE.md",
		Phrase: "since two tools disagreeing about what \"stale\" means",
		Reason: "'two tools' here is any two programs, not the MCP tool inventory.",
	},
}

var claimRe = func() *regexp.Regexp {
	words := make([]string, 0, len(numberWords))
	for w := range numberWords {
		words = append(words, w)
	}
	// Longest first, and not for tidiness: Go's alternation is leftmost-FIRST, so
	// whichever spelling appears earlier in the pattern wins. `\b` is what kept the
	// single words safe from each other ("nine" cannot match inside "nineteen"), but
	// it sits happily between "twenty" and "-two", so "twenty-two explainers" read as
	// a claim of twenty with "two" absorbed as filler. Built from a map, the order was
	// also random, so the bug would have come and gone between runs.
	sort.Slice(words, func(i, j int) bool {
		if len(words[i]) != len(words[j]) {
			return len(words[i]) > len(words[j])
		}
		return words[i] < words[j]
	})
	subjects := make([]string, 0, len(countSubjects))
	for _, s := range countSubjects {
		subjects = append(subjects, s.pattern)
	}
	return regexp.MustCompile(`(?i)\b(` + strings.Join(words, "|") + `|\d{1,3})\b[ -]` +
		`((?:[A-Za-z][A-Za-z-]* ){0,2}?)(` + strings.Join(subjects, "|") + `)\b`)
}()

// TestCountClaimsMatchTheCode is the check the whole package is built around.
//
// Documentation states how many of a thing there are, and that number is correct
// exactly once — on the day it is typed. This repository had five wrong at the same
// time: fourteen MCP tools against seventeen registered, eleven rule forms against
// thirteen, eight and nine taxonomies in two pages against ten, and one page calling
// the explainers fifteen and sixteen four paragraphs apart.
//
// The number is resolved against the value the binary uses, never a list kept here.
func TestCountClaimsMatchTheCode(t *testing.T) {
	byKey := map[string]Inventory{}
	for _, inv := range Inventories() {
		byKey[inv.Key] = inv
	}
	subjectFor := map[string]string{}
	for _, s := range countSubjects {
		subjectFor[s.pattern] = s.inventory
	}

	docs := corpus(t)
	waived := waivedRanges(t, docs)

	var checked int
	for _, d := range docs {
		for _, m := range claimRe.FindAllStringSubmatchIndex(d.Prose, -1) {
			claimed, ok := parseCount(d.Prose[m[2]:m[3]])
			if !ok || claimed < 2 {
				continue
			}
			subject := strings.ToLower(d.Prose[m[6]:m[7]])
			inv, ok := byKey[inventoryFor(subject)]
			if !ok {
				continue
			}
			if inRanges(waived[d.Path], m[0]) {
				continue
			}
			checked++
			if claimed != len(inv.Items) {
				line := strings.Count(d.Prose[:m[0]], "\n") + 1
				t.Errorf("%s:%d: %q claims %d %s, but %s has %d (%s).\n"+
					"    Fix the sentence, or — if the number is deliberately frozen — "+
					"add it to countWaivers with the reason.",
					d.Path, line, strings.TrimSpace(d.Prose[m[0]:m[1]]), claimed, inv.Key,
					inv.Source, len(inv.Items), strings.Join(inv.Items, ", "))
			}
		}
	}
	if checked < 5 {
		t.Errorf("only %d count claims found; the pages or claimRe changed", checked)
	}
}

// TestNoStaleCountWaivers refuses a waiver for a sentence that is no longer there.
func TestNoStaleCountWaivers(t *testing.T) {
	body := map[string]string{}
	for _, d := range corpus(t) {
		body[d.Path] = d.Prose
	}
	for _, w := range countWaivers {
		text, ok := body[w.Doc]
		if !ok {
			t.Errorf("countWaivers names %s, which is not in the corpus", w.Doc)
			continue
		}
		if !strings.Contains(text, w.Phrase) {
			t.Errorf("countWaivers still waives %q in %s, but that phrase is gone — "+
				"the sentence was rewritten, so drop the waiver and let the count be checked.\n"+
				"    Waived because: %s", w.Phrase, w.Doc, w.Reason)
		}
	}
}

// TestEveryInventoryIsEnumeratedSomewhere is the weakest useful form of "everything
// is represented where it needs to be": every member of every counted vocabulary has
// to be NAMED in the documentation at least once. A tool or a rule form that ships
// with no page mentioning it is one no reader can discover.
//
// It says nothing about WHERE, which is the part that needs the engine — see the
// per-surface contract table in pkg/bootstrap's docs test.
func TestEveryInventoryIsEnumeratedSomewhere(t *testing.T) {
	var all strings.Builder
	for _, d := range corpus(t) {
		all.WriteString(d.Prose)
	}
	text := all.String()
	for _, inv := range Inventories() {
		if !inv.UserFacing {
			continue
		}
		for _, item := range inv.Items {
			if !strings.Contains(text, item) {
				t.Errorf("%s ships %q (from %s), which no documentation page names",
					inv.Key, item, inv.Source)
			}
		}
	}
}

func inventoryFor(subject string) string {
	switch {
	case strings.HasPrefix(subject, "explainer"):
		return "explainers"
	case strings.HasPrefix(subject, "tool"):
		return "MCP tools"
	case strings.HasSuffix(subject, "form") || strings.HasSuffix(subject, "forms"):
		return "rule forms"
	case strings.HasPrefix(subject, "taxonom"):
		return "layer taxonomies"
	}
	return ""
}

func parseCount(s string) (int, bool) {
	if n, ok := numberWords[strings.ToLower(s)]; ok {
		return n, true
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

type byteRange struct{ start, end int }

func waivedRanges(t *testing.T, docs []Doc) map[string][]byteRange {
	t.Helper()
	out := map[string][]byteRange{}
	for _, w := range countWaivers {
		for _, d := range docs {
			if d.Path != w.Doc {
				continue
			}
			for i := 0; ; {
				j := strings.Index(d.Prose[i:], w.Phrase)
				if j < 0 {
					break
				}
				out[d.Path] = append(out[d.Path], byteRange{i + j, i + j + len(w.Phrase)})
				i += j + len(w.Phrase)
			}
		}
	}
	return out
}

func inRanges(rs []byteRange, off int) bool {
	for _, r := range rs {
		if off >= r.start && off < r.end {
			return true
		}
	}
	return false
}
