package mdintent

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// Document mode: a markdown file that declares no intent is still a source.
// The file is a symbol named by its path, each heading is a symbol named the
// way a fragment names it, and every relative link that resolves on disk is a
// names relation from the enclosing section (or the document) to the file it
// points at. Nothing is guessed from prose: a link that does not resolve is
// counted under its cause and derives nothing.

const (
	SymbolDocument = "document"
	SymbolSection  = "section"

	linksEdgeType = "file_ref"
	linksFactName = "mdintent:links"
)

var (
	headingLine = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)
	inlineLink  = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	codeSpan    = regexp.MustCompile("`([^`\\s]+)`")
	scheme      = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
	slugDrop    = regexp.MustCompile(`[^a-z0-9 _-]`)
)

type linkCount struct {
	resolved   int
	unresolved map[string]int
}

func (c *linkCount) miss(cause string) {
	if c.unresolved == nil {
		c.unresolved = map[string]int{}
	}
	c.unresolved[cause]++
}

type section struct {
	fact  *facts.Fact
	links map[string]bool
}

func documentFacts(scope *inScope, relFile string, src []byte, count *linkCount) []facts.Fact {
	slashed := filepath.ToSlash(relFile)
	doc := &section{fact: &facts.Fact{
		Kind: facts.KindSymbol,
		Name: slashed,
		File: slashed,
		Line: 1,
		Props: map[string]any{
			"language":    "markdown",
			"symbol_kind": SymbolDocument,
			"exported":    true,
		},
	}, links: map[string]bool{}}
	if dir := factpath.Dir(slashed); dir != "." && dir != "" {
		doc.fact.Relations = append(doc.fact.Relations, facts.Relation{Kind: facts.RelDeclares, Target: dir})
	}

	sections := []*section{}
	slugs := map[string]int{}
	current := doc
	title := ""
	inFence := false
	fence := ""
	lines := strings.Split(string(src), "\n")
	body := stripFrontmatter(lines)
	for i, line := range lines {
		if i < body {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if marker := fenceMarker(trimmed); marker != "" {
			switch {
			case !inFence:
				inFence, fence = true, marker
			case strings.HasPrefix(marker, fence[:1]):
				inFence, fence = false, ""
			}
			continue
		}
		if inFence {
			continue
		}
		if m := headingLine.FindStringSubmatch(line); m != nil {
			heading := strings.TrimSpace(m[2])
			if title == "" && len(m[1]) == 1 {
				title = heading
			}
			slug := uniqueSlug(slugOf(heading), slugs)
			sec := &section{fact: &facts.Fact{
				Kind: facts.KindSymbol,
				Name: slashed + "#" + slug,
				File: slashed,
				Line: i + 1,
				Props: map[string]any{
					"language":    "markdown",
					"symbol_kind": SymbolSection,
					"title":       heading,
					"level":       len(m[1]),
					"exported":    true,
				},
			}, links: map[string]bool{}}
			sections = append(sections, sec)
			doc.fact.Relations = append(doc.fact.Relations, facts.Relation{Kind: facts.RelDeclares, Target: sec.fact.Name})
			current = sec
			continue
		}
		for _, target := range linkTargets(line) {
			if resolved, cause := resolveLink(scope, relFile, target); resolved != "" {
				if !current.links[resolved] {
					current.links[resolved] = true
					current.fact.Relations = append(current.fact.Relations, facts.Relation{Kind: facts.RelNames, Target: resolved})
				}
				count.resolved++
			} else if cause != "" {
				count.miss(cause)
			}
		}
	}
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(relFile), ".md")
	}
	doc.fact.SetProp("title", title)
	doc.fact.SetProp("sections", len(sections))

	out := []facts.Fact{*doc.fact}
	for _, sec := range sections {
		out = append(out, *sec.fact)
	}
	return out
}

// stripFrontmatter returns the index of the first body line, skipping a
// leading YAML block so its keys are not read as prose.
func stripFrontmatter(lines []string) int {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return 0
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return i + 1
		}
	}
	return 0
}

func fenceMarker(trimmed string) string {
	for _, m := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, m) {
			return m
		}
	}
	return ""
}

// linkTargets reads the relative targets a line carries: inline links, and
// code spans that spell a path (a slash is what separates a path from an
// identifier; `foo.bar` is a method, `app/models/foo.rb` is a file).
func linkTargets(line string) []string {
	var out []string
	for _, m := range inlineLink.FindAllStringSubmatch(line, -1) {
		out = append(out, m[1])
	}
	for _, m := range codeSpan.FindAllStringSubmatch(line, -1) {
		if strings.Contains(m[1], "/") && !strings.ContainsAny(m[1], "*{}$<>") {
			out = append(out, m[1])
		}
	}
	return out
}

// resolveLink returns the repository-relative path a target resolves to, or
// the cause it does not: external for a URL, outside-repo for a path that
// escapes the root, missing for one that is not on disk. An anchor-only
// target is neither and returns nothing.
// inScope is the set a link may resolve against: every file the walker kept, plus
// the directories those files live in.
//
// It is not the filesystem, and that distinction is the whole point. Resolving
// against disk made a snapshot depend on WHETHER A PREVIOUS SNAPSHOT EXISTS: this
// repository's own documentation names paths under its output directory, so
// `.enola/extractor_cache.json` was missing on a cold run, present on the next, and
// `.enola/previous` appeared on the one after that — three runs, three different
// fact streams, on the one repository in the corpus whose docs describe enola. The
// ignore globs already say what is source and what is output; the walker has
// applied them, and this resolves against what the walker kept.
type inScope struct {
	paths map[string]bool
}

func newInScope(files []string) *inScope {
	s := &inScope{paths: make(map[string]bool, len(files)*2)}
	for _, f := range files {
		clean := factpath.Clean(filepath.ToSlash(f))
		s.paths[clean] = true
		// A link may name a directory — `internal/cachecov`, `docs/` — and a
		// directory is not a walked file. Every ancestor of a kept file is in
		// scope by construction, and one holding nothing but ignored files is not.
		for dir := factpath.Dir(clean); dir != "" && dir != "."; dir = factpath.Dir(dir) {
			if s.paths[dir] {
				break
			}
			s.paths[dir] = true
		}
	}
	return s
}

func (s *inScope) has(path string) bool { return s.paths[path] }

func resolveLink(scope *inScope, relFile, target string) (string, string) {
	if target == "" || strings.HasPrefix(target, "#") {
		return "", ""
	}
	if scheme.MatchString(target) || strings.HasPrefix(target, "//") {
		return "", "external"
	}
	if i := strings.IndexAny(target, "#?"); i >= 0 {
		target = target[:i]
	}
	if target == "" {
		return "", ""
	}
	candidates := []string{}
	if strings.HasPrefix(target, "/") {
		candidates = append(candidates, strings.TrimPrefix(target, "/"))
	} else {
		candidates = append(candidates, factpath.Dir(filepath.ToSlash(relFile))+"/"+target, target)
	}
	for _, candidate := range candidates {
		clean := factpath.Clean(strings.TrimPrefix(candidate, "/"))
		if clean == ".." || strings.HasPrefix(clean, "../") {
			continue
		}
		if scope.has(clean) {
			if clean == "." {
				return "", ""
			}
			return clean, ""
		}
	}
	first := factpath.Clean(strings.TrimPrefix(candidates[0], "/"))
	if first == ".." || strings.HasPrefix(first, "../") {
		return "", "outside-repo"
	}
	return "", "missing"
}

func slugOf(heading string) string {
	s := strings.ToLower(heading)
	s = slugDrop.ReplaceAllString(s, "")
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "-")
	if s == "" {
		return "section"
	}
	return s
}

func uniqueSlug(slug string, seen map[string]int) string {
	n := seen[slug]
	seen[slug] = n + 1
	if n == 0 {
		return slug
	}
	return slug + "-" + itoa(n)
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
