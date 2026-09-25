package docslint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// benchHeadlineFile holds the few benchmark figures the website shows, and
// benchDoc is the page they are taken from.
//
// BENCHMARKS.md is rewritten after every sweep; the website is not. The site
// copies its numbers from benchHeadlineFile, so the file must never state a
// figure BENCHMARKS.md has moved away from, or the site quotes a number the
// methodology no longer supports.
const (
	benchHeadlineFile = "docs/benchmarks-headline.json"
	benchDoc          = "docs/BENCHMARKS.md"
)

type benchHeadline struct {
	ID       string   `json:"id"`
	Question string   `json:"question,omitempty"`
	Figure   string   `json:"figure"`
	Label    string   `json:"label"`
	Anchor   string   `json:"anchor"`
	Evidence []string `json:"evidence"`
}

// numberRe finds every number a reader would take as a claim: 91, 1,859, 3.5.
var numberRe = regexp.MustCompile(`\d+(?:[.,]\d+)*`)

// TestBenchmarkHeadlinesAreStatedInBenchmarks checks each headline against the
// section it names: the anchor must exist, every evidence phrase must appear in
// that section, and so must every number in the figure and the label.
//
// Evidence phrases alone would let a label drift ("none of 1,620 repeated")
// while the phrase they quote stayed true; the number check closes that.
func TestBenchmarkHeadlinesAreStatedInBenchmarks(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, benchHeadlineFile))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Headlines []benchHeadline `json:"headlines"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("%s: %v", benchHeadlineFile, err)
	}
	if len(file.Headlines) == 0 {
		t.Fatalf("%s lists no headlines", benchHeadlineFile)
	}

	body, err := os.ReadFile(filepath.Join(repoRoot, benchDoc))
	if err != nil {
		t.Fatal(err)
	}
	doc := Doc{Path: benchDoc, Body: string(body), Prose: blankFences(string(body))}

	seen := map[string]bool{}
	for _, h := range file.Headlines {
		// Question is optional: without one, a headline is a figure other pages
		// quote rather than a block of its own on the benchmarks page.
		if h.ID == "" || h.Figure == "" || h.Label == "" || len(h.Evidence) == 0 {
			t.Errorf("%s: headline %q is missing id, figure, label or evidence", benchHeadlineFile, h.ID)
			continue
		}
		if seen[h.ID] {
			t.Errorf("%s: headline id %q appears twice", benchHeadlineFile, h.ID)
		}
		seen[h.ID] = true

		section, ok := sectionByAnchor(doc, h.Anchor)
		if !ok {
			t.Errorf("%s: headline %q names #%s, which is not a heading in %s",
				benchHeadlineFile, h.ID, h.Anchor, benchDoc)
			continue
		}
		for _, phrase := range h.Evidence {
			if !strings.Contains(section, phrase) {
				t.Errorf("%s: headline %q quotes %q, which %s § #%s no longer says.\n"+
					"    Update the headline in the same change as the benchmark.",
					benchHeadlineFile, h.ID, phrase, benchDoc, h.Anchor)
			}
		}
		for _, n := range numberRe.FindAllString(h.Figure+" "+h.Label, -1) {
			if !strings.Contains(section, n) {
				t.Errorf("%s: headline %q states %s, which %s § #%s does not.",
					benchHeadlineFile, h.ID, n, benchDoc, h.Anchor)
			}
		}
	}
}

// sectionByAnchor returns the text under the heading whose GitHub anchor is
// anchor, up to the next heading of the same or higher level.
func sectionByAnchor(doc Doc, anchor string) (string, bool) {
	for _, m := range headingRe.FindAllStringSubmatch(doc.Prose, -1) {
		if Slug(m[1]) == anchor {
			return doc.Section(m[1])
		}
	}
	return "", false
}
