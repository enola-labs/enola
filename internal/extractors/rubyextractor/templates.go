package rubyextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
	ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
)

// refsFromRuby parses Ruby source and returns a single reference-only fact of the
// given kind (facts.KindTestRef / facts.KindFileRef) carrying the production
// symbols the source references. It emits NO symbol facts (so the source never
// becomes a dead-code candidate) and returns nil when nothing is referenced.
// Shared by the test-file and view-template reference passes.
func refsFromRuby(src []byte, relFile, kind string) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(ruby.Language())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()

	w := &rubyWalker{src: src, relFile: relFile, dir: factpath.Dir(relFile), fileRefIdx: -1}
	seen := make(map[string]bool)
	var rels []facts.Relation
	add := func(target string) {
		if target == "" || seen[target] {
			return
		}
		seen[target] = true
		rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: target})
	}
	w.walkTestRefs(tree.RootNode(), add)
	if len(rels) == 0 {
		return nil
	}
	return []facts.Fact{{
		Kind:      kind,
		Name:      relFile,
		File:      relFile,
		Props:     map[string]any{"language": "ruby"},
		Relations: rels,
	}}
}

// isTemplateFile reports whether a path is a Rails view template that embeds Ruby
// (ERB, Slim, or HAML — covering compound suffixes like .html.erb, .js.erb).
func isTemplateFile(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".erb") ||
		strings.HasSuffix(lower, ".slim") ||
		strings.HasSuffix(lower, ".haml")
}

// isJbuilderFile reports whether a path is a Jbuilder view (.jbuilder, usually
// .json.jbuilder). A Jbuilder template is plain Ruby — the json builder DSL —
// so it goes through the same reference-only pass as the embedded-Ruby
// templates, with the whole file as the Ruby region.
func isJbuilderFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".jbuilder")
}

// extractTemplateRefs pulls the embedded Ruby out of a view template, parses it,
// and returns a KindFileRef fact carrying the call references it makes — so
// helpers and class methods invoked only from views are not mis-reported as dead.
// The reference-only walk emits no symbols, so templates never become candidates.
func extractTemplateRefs(src []byte, relFile string) []facts.Fact {
	embedded := extractEmbeddedRuby(src, relFile)
	if len(embedded) == 0 {
		return nil
	}
	return refsFromRuby(embedded, relFile, facts.KindFileRef)
}

// extractEmbeddedRuby returns the Ruby regions of a template, joined by newlines.
// ERB delimits Ruby with <% %> / <%= %>; Slim and HAML are indentation languages
// where Ruby follows a leading =/-/~ marker on a line; a Jbuilder view is Ruby
// from the first byte and passes through whole. Interpolation (#{...}) is
// captured for all kinds. Only Ruby regions are returned — never HTML/text — so
// the downstream parse sees real code (tree-sitter tolerates imperfect fragments).
func extractEmbeddedRuby(src []byte, relFile string) []byte {
	if isJbuilderFile(relFile) {
		return src
	}
	if strings.HasSuffix(strings.ToLower(relFile), ".erb") {
		return extractERBRuby(string(src))
	}
	return extractIndentRuby(string(src)) // slim / haml
}

// extractERBRuby collects the Ruby inside <% … %> / <%= … %> / <%- … -%> tags,
// skipping <%# … %> comments.
func extractERBRuby(s string) []byte {
	var out strings.Builder
	for i := 0; i < len(s); {
		open := strings.Index(s[i:], "<%")
		if open < 0 {
			break
		}
		i += open + 2
		comment := i < len(s) && s[i] == '#'
		// Strip a leading output/trim marker (=, -, ==).
		for i < len(s) && (s[i] == '=' || s[i] == '-') {
			i++
		}
		closeIdx := strings.Index(s[i:], "%>")
		if closeIdx < 0 {
			break
		}
		frag := s[i : i+closeIdx]
		i += closeIdx + 2
		if comment {
			continue
		}
		out.WriteString(strings.TrimRight(frag, "-")) // drop trailing - from -%>
		out.WriteByte('\n')
	}
	return []byte(out.String())
}

// extractIndentRuby collects Ruby from Slim/HAML lines: the text after a leading
// control/output marker (=, ==, -, ~), plus any #{…} interpolations on any line.
// Comment lines (-#, /) are skipped. Tag-attached output (`p= foo`) and filter
// blocks are best-effort — missing them only under-captures (never a false edge).
func extractIndentRuby(s string) []byte {
	var out strings.Builder
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimLeft(raw, " \t")
		if strings.HasPrefix(line, "-#") || strings.HasPrefix(line, "/") {
			continue // comment
		}
		if line != "" && (line[0] == '=' || line[0] == '-' || line[0] == '~') {
			r := strings.TrimLeft(line[1:], "=") // handle == / ~=
			out.WriteString(strings.TrimLeft(r, " \t"))
			out.WriteByte('\n')
		}
		for _, expr := range interpolations(raw) {
			out.WriteString(expr)
			out.WriteByte('\n')
		}
	}
	return []byte(out.String())
}

// interpolations returns the Ruby expressions inside #{…} spans of a line,
// handling nested braces.
func interpolations(line string) []string {
	var out []string
	for i := 0; i+1 < len(line); i++ {
		if line[i] != '#' || line[i+1] != '{' {
			continue
		}
		depth, j := 1, i+2
		for j < len(line) && depth > 0 {
			switch line[j] {
			case '{':
				depth++
			case '}':
				depth--
			}
			j++
		}
		if depth == 0 {
			out = append(out, line[i+2:j-1])
			i = j - 1
		}
	}
	return out
}
