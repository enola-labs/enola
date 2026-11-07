package docslint

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/workspace"
)

// repoRoot is the module root, found by walking up from the test's working directory
// until go.mod appears. It is computed rather than a fixed "../.." so the corpus can
// be read from any package: Tier A's extractor contract lives in pkg/bootstrap, which
// happens to sit at the same depth today and would silently read the wrong tree the
// day something moved.
//
// Every path a check reports is repo-relative regardless, so a failure names the file
// the way the author would type it rather than the way `go test` was invoked.
var repoRoot = findRepoRoot()

func findRepoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "../.."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without finding go.mod. Fall back to the
			// old relative guess rather than panicking in a package initialiser.
			return "../.."
		}
		dir = parent
	}
}

// skipDirs are directories whose markdown is not documentation ABOUT enola.
// `.enola/` holds generated llm_context.md, `testdata/` holds fixture repositories
// that are deliberately miniature and wrong, and `grammar/` carries vendored
// upstream attribution files this project does not write.
var skipDirs = map[string]bool{
	".git": true, ".enola": true, "testdata": true, "grammar": true, "node_modules": true,
}

// Doc is one markdown file, read once and shared by every check.
type Doc struct {
	// Path is repo-relative, e.g. "docs/EXPLAINERS.md".
	Path string
	// Body is the file verbatim.
	Body string
	// Prose is Body with every fenced code block blanked out, line for line, so
	// line numbers still match the file. A YAML example declaring eleven rules is
	// not a claim about how many rule forms exist, and a path inside a shell
	// snippet is an illustration rather than a reference.
	Prose string
}

// Corpus reads every documentation page in the repository.
func Corpus() ([]Doc, error) {
	var docs []Doc
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || (path != repoRoot && workspace.IsLinkedWorktree(path)) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		docs = append(docs, Doc{
			Path:  filepath.ToSlash(rel),
			Body:  string(body),
			Prose: blankFences(string(body)),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Path < docs[j].Path })
	return docs, nil
}

// blankFences replaces the contents of every ``` fenced block with empty lines,
// preserving the line count so a reported line number still points at the source.
func blankFences(body string) string {
	lines := strings.Split(body, "\n")
	inFence := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			lines[i] = ""
			continue
		}
		if inFence {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// Line returns the 1-indexed line number that offset falls on.
func (d Doc) Line(offset int) int {
	return strings.Count(d.Body[:offset], "\n") + 1
}

var headingRe = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.*)$`)

// inlineMarkup is what GitHub's slugger drops before slugifying: HTML tags, and the
// markdown emphasis/code punctuation that never survives into an anchor.
var inlineMarkup = regexp.MustCompile(`<[^>]+>`)

// Anchors returns the set of fragment identifiers this page defines, computed the
// way GitHub computes them: lowercase, drop everything that is not a word
// character, space or hyphen, then turn each remaining space into one hyphen.
//
// The "each" matters. Collapsing runs of whitespace instead — the obvious
// implementation — reports every anchor containing an em dash as broken, because
// "A — B" loses the dash and keeps both spaces, and GitHub renders that as "a--b".
func (d Doc) Anchors() map[string]bool {
	out := map[string]bool{}
	// Headings inside a fence are code, not structure.
	for _, m := range headingRe.FindAllStringSubmatch(d.Prose, -1) {
		out[Slug(m[1])] = true
	}
	return out
}

// Slug converts heading text to a GitHub anchor.
func Slug(text string) string {
	s := inlineMarkup.ReplaceAllString(text, "")
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || r == '\'':
			// GitHub keeps hyphens and underscores; apostrophes are dropped.
			if r != '\'' {
				b.WriteRune(r)
			}
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r > 127:
			// Non-ASCII word characters (accented letters) survive; punctuation
			// like an em dash does not.
			if isWordRune(r) {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func isWordRune(r rune) bool {
	// A crude but sufficient test for the accented letters that appear in these
	// pages. Dashes, quotes and arrows are the punctuation that actually shows up,
	// and all of them sit below this range.
	return r >= 0x00C0 && r <= 0x024F
}

// Link is one markdown link target found in prose.
type Link struct {
	// Target is the raw href, e.g. "../ARCHITECTURE.md#the-idea".
	Target string
	// Line is where it appears in the containing document.
	Line int
}

var linkRe = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// Links returns every markdown link target on the page, code blocks excluded.
func (d Doc) Links() []Link {
	var out []Link
	for _, m := range linkRe.FindAllStringSubmatchIndex(d.Prose, -1) {
		out = append(out, Link{
			Target: d.Prose[m[2]:m[3]],
			Line:   strings.Count(d.Prose[:m[0]], "\n") + 1,
		})
	}
	return out
}

// Section returns the body beneath the first heading whose text contains title,
// stopping at the next heading of the same or higher level. It reports false when no
// such heading exists — a contract naming a section that has been renamed must fail
// loudly rather than silently check an empty string and pass.
//
// Matching is on a substring of the heading text, case-insensitively, so a contract
// can say "The tools" without restating the em-dashed subtitle that follows it.
func (d Doc) Section(title string) (string, bool) {
	lines := strings.Split(d.Prose, "\n")
	var body []string
	level := 0
	for _, line := range lines {
		m := headingLineRe.FindStringSubmatch(line)
		if m != nil {
			if level == 0 {
				if strings.Contains(strings.ToLower(m[2]), strings.ToLower(title)) {
					level = len(m[1])
				}
				continue
			}
			if len(m[1]) <= level {
				break
			}
		}
		if level > 0 {
			body = append(body, line)
		}
	}
	if level == 0 {
		return "", false
	}
	return strings.Join(body, "\n"), true
}

var headingLineRe = regexp.MustCompile(`^(#{1,6})[ \t]+(.*)$`)

// tableCellMarkup is the inline markup a table cell wraps its label in — a link to the
// page, bold, or code ticks. The label is what a reader sees, so all of it comes off
// before comparison.
var tableCellMarkup = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)

// LanguageTableRows returns the first-column label of every body row of every markdown
// table in text whose header column is titled "Language".
//
// Keying on the header is what makes this safe to run over a whole page: three
// documents carry a language table, and two of them carry other tables as well
// (fixture paths, per-tool parameters) whose first column would otherwise read as a
// language nobody registered.
func LanguageTableRows(text string) []string {
	var out []string
	inLanguageTable := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			inLanguageTable = false
			continue
		}
		cell := firstCell(trimmed)
		if cell == "" || isTableRule(cell) {
			continue
		}
		if strings.EqualFold(cell, "Language") {
			inLanguageTable = true
			continue
		}
		if inLanguageTable {
			out = append(out, cell)
		}
	}
	return out
}

func firstCell(row string) string {
	cell, _, _ := strings.Cut(strings.TrimPrefix(row, "|"), "|")
	cell = tableCellMarkup.ReplaceAllString(cell, "$1")
	cell = strings.ReplaceAll(cell, "**", "")
	cell = strings.ReplaceAll(cell, "`", "")
	return strings.TrimSpace(cell)
}

// isTableRule reports whether a cell is the |---|---| separator rather than content.
func isTableRule(cell string) bool {
	return strings.Trim(cell, "-: ") == ""
}
