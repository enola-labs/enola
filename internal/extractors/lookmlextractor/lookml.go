// Package lookmlextractor reads LookML: the views a Looker project defines, the
// tables they read, and the explores that put views in front of users.
//
//	view: orders {
//	  sql_table_name: analytics.marts.fct_orders ;;
//	}
//	explore: orders {
//	  join: users { … }
//	}
//
// A view becomes a symbol "lookml.<view>" that depends on the table it reads,
// named by the table's last segment (`fct_orders`): the name the warehouse, and
// the dbt model that builds the table, give it. A derived table depends on the
// tables its SQL selects from, and a view on the views it extends. An explore
// becomes a symbol "lookml.<model>.<explore>" depending on its base view and
// every view it joins. Views and explores are framework roots: Looker serves
// them, and nothing in the repository calls them.
package lookmlextractor

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/facts"
)

// Extractor reads LookML files.
type Extractor struct{}

// New creates the extractor.
func New() *Extractor { return &Extractor{} }

// Name returns the extractor name.
func (e *Extractor) Name() string { return "lookml" }

func isLookML(rel string) bool {
	l := strings.ToLower(rel)
	return strings.HasSuffix(l, ".lkml") || strings.HasSuffix(l, ".lookml")
}

// OwnsFile scopes caching to LookML files.
func (e *Extractor) OwnsFile(relFile string) bool { return isLookML(relFile) }

// Detect reports a LookML file anywhere in the repository.
func (e *Extractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector.
func (e *Extractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if isLookML(rel) {
			return true, nil
		}
	}
	return false, nil
}

// block is one top-level `view:` or `explore:` declaration, or a `join:` inside an
// explore, with the parameters read off its body.
type block struct {
	kind, name string
	line       int
	params     map[string][]string // parameter -> values (sql parameters keep their text)
	children   []*block            // an explore's joins
}

// Extract emits the views and explores of every LookML file.
func (e *Extractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var out []facts.Fact
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if !isLookML(rel) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		model := ""
		if base := path.Base(rel); strings.HasSuffix(base, ".model.lkml") {
			model = strings.TrimSuffix(base, ".model.lkml")
		}
		for _, b := range parse(string(src)) {
			switch b.kind {
			case "view":
				out = append(out, viewFact(rel, b))
			case "explore":
				owner := model
				if owner == "" {
					owner = strings.TrimSuffix(path.Base(rel), path.Ext(path.Base(rel)))
					owner = strings.TrimSuffix(owner, ".explore")
				}
				out = append(out, exploreFact(rel, owner, b))
			}
		}
	}
	return out, nil
}

func symbol(name, file string, line int, kind string) facts.Fact {
	return facts.Fact{
		Kind: facts.KindSymbol, Name: name, File: file, Line: line,
		Props: map[string]any{
			"symbol_kind":           facts.SymbolType,
			"language":              "lookml",
			"lookml_kind":           kind,
			facts.PropFrameworkRoot: "looker",
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: path.Dir(file)}},
	}
}

func dependOn(f *facts.Fact, seen map[string]bool, target string) {
	if target == "" || target == f.Name || seen[target] {
		return
	}
	seen[target] = true
	f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: target})
}

func viewFact(rel string, b *block) facts.Fact {
	f := symbol("lookml."+b.name, rel, b.line, "view")
	seen := map[string]bool{}
	for _, t := range b.params["sql_table_name"] {
		dependOn(&f, seen, tableName(t))
	}
	for _, sql := range b.params["sql"] { // a derived table's query
		for _, t := range sqlTables(sql) {
			dependOn(&f, seen, t)
		}
	}
	for _, list := range b.params["extends"] {
		for _, v := range listItems(list) {
			dependOn(&f, seen, "lookml."+v)
		}
	}
	return f
}

func exploreFact(rel, model string, b *block) facts.Fact {
	f := symbol("lookml."+model+"."+b.name, rel, b.line, "explore")
	f.Props["looker_model"] = model
	seen := map[string]bool{}
	base := b.name
	if v := first(b.params["view_name"]); v != "" {
		base = v
	}
	if v := first(b.params["from"]); v != "" {
		base = v
	}
	dependOn(&f, seen, "lookml."+base)
	for _, j := range b.children {
		view := j.name
		if v := first(j.params["from"]); v != "" {
			view = v
		}
		if v := first(j.params["view_name"]); v != "" {
			view = v
		}
		dependOn(&f, seen, "lookml."+view)
	}
	return f
}

func first(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	return strings.Trim(strings.TrimSpace(vs[0]), `"`)
}

// tableName returns the last segment of a qualified table name:
// `project.dataset.fct_orders` → `fct_orders`. A templated name (`${…}`,
// `{% … %}`) names no table.
func tableName(s string) string {
	s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "`\""))
	if s == "" || strings.ContainsAny(s, "${}% ") {
		return ""
	}
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return strings.Trim(s, "`\"")
}

// sqlTableRef is a table a FROM or JOIN names with at least a dataset qualifier.
var sqlTableRef = regexp.MustCompile("(?i)\\b(?:from|join)\\s+`?([A-Za-z0-9_\\-]+(?:\\.[A-Za-z0-9_\\-]+){1,2})`?")

func sqlTables(sql string) []string {
	var out []string
	for _, m := range sqlTableRef.FindAllStringSubmatch(sql, -1) {
		if t := tableName(m[1]); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func listItems(s string) []string {
	s = strings.Trim(strings.TrimSpace(s), "[]")
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.Trim(strings.TrimSpace(p), `"`); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parse reads the top-level view and explore blocks of a LookML file. LookML is
// `key: value` pairs and `key: name { … }` blocks; a SQL value runs to `;;` and
// may hold braces (`${view.field}`), so it is read as a unit; `#` starts a comment
// outside strings and SQL.
func parse(src string) []*block {
	p := &parser{src: src, line: 1}
	return p.blocks(0, nil)
}

type parser struct {
	src  string
	pos  int
	line int
}

func (p *parser) peek() byte {
	if p.pos < len(p.src) {
		return p.src[p.pos]
	}
	return 0
}

func (p *parser) advance() {
	if p.src[p.pos] == '\n' {
		p.line++
	}
	p.pos++
}

func (p *parser) skipSpace() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case '#':
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
		case ' ', '\t', '\r', '\n', ',':
			p.advance()
		default:
			return
		}
	}
}

func isIdent(c byte) bool {
	return c == '_' || c == '-' || c == '.' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func (p *parser) ident() string {
	start := p.pos
	for p.pos < len(p.src) && isIdent(p.src[p.pos]) {
		p.pos++
	}
	return p.src[start:p.pos]
}

// value reads a scalar value to the end of its line, a quoted string, a [list], or
// SQL up to `;;`.
func (p *parser) value(key string) string {
	p.skipInline()
	if strings.HasPrefix(key, "sql") || key == "html" || key == "expression" {
		end := strings.Index(p.src[p.pos:], ";;")
		if end < 0 {
			end = len(p.src) - p.pos
		}
		v := p.src[p.pos : p.pos+end]
		for i := 0; i < end+2 && p.pos < len(p.src); i++ {
			p.advance()
		}
		return v
	}
	switch p.peek() {
	case '"':
		p.advance()
		start := p.pos
		for p.pos < len(p.src) && p.src[p.pos] != '"' {
			if p.src[p.pos] == '\\' {
				p.advance()
			}
			p.advance()
		}
		v := p.src[start:p.pos]
		if p.pos < len(p.src) {
			p.advance()
		}
		return v
	case '[':
		start := p.pos
		depth := 0
		for p.pos < len(p.src) {
			c := p.src[p.pos]
			p.advance()
			if c == '[' {
				depth++
			} else if c == ']' {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		return p.src[start:p.pos]
	}
	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] != '\n' && p.src[p.pos] != '}' && p.src[p.pos] != '#' {
		p.pos++
	}
	return strings.TrimSpace(p.src[start:p.pos])
}

func (p *parser) skipInline() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

// blocks reads `key: …` entries until the closing brace of the enclosing block
// (or the end of the file at depth 0). Top-level views and explores, and the joins
// of an explore, are returned; every other block is read only to be skipped, its
// sql parameters folded into the enclosing view (a derived_table's query).
func (p *parser) blocks(depth int, into *block) []*block {
	var out []*block
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			return out
		}
		if p.peek() == '}' {
			p.advance()
			return out
		}
		if !isIdent(p.peek()) {
			p.advance() // stray punctuation: move on rather than stall
			continue
		}
		line := p.line
		key := p.ident()
		p.skipInline()
		if p.peek() != ':' {
			continue
		}
		p.advance()
		p.skipInline()
		// `key: name {` or `key: {`
		name := ""
		if isIdent(p.peek()) {
			save, saveLine := p.pos, p.line
			name = p.ident()
			p.skipSpace()
			if p.peek() != '{' {
				p.pos, p.line = save, saveLine
				name = ""
			}
		} else {
			save, saveLine := p.pos, p.line
			p.skipSpace()
			if p.peek() != '{' {
				p.pos, p.line = save, saveLine
			}
		}
		if p.peek() == '{' {
			p.advance()
			b := &block{kind: key, name: name, line: line, params: map[string][]string{}}
			p.blocks(depth+1, b)
			switch {
			case depth == 0 && (key == "view" || key == "explore") && name != "":
				out = append(out, b)
			case key == "join" && into != nil && name != "":
				into.children = append(into.children, b)
			case into != nil:
				// derived_table and the like: their SQL is the view's.
				for k, v := range b.params {
					if strings.HasPrefix(k, "sql") {
						into.params["sql"] = append(into.params["sql"], v...)
					}
				}
			}
			continue
		}
		v := p.value(key)
		if into != nil {
			into.params[key] = append(into.params[key], v)
		}
	}
}
