// Package regoextractor reads Open Policy Agent policies (.rego).
//
//	package orders.sandbox
//	import data.shared
//	allow if { verified := shared.token.verified; has_permission(…) }
//	has_permission(perms, principals) if { … }
//
// Each rule becomes a symbol "<package>.<rule>", declared once however many
// definitions it has, with a depends_on edge to every rule its bodies reference:
// a rule of its own package by name, another package's through data.<package>
// or an imported alias. A reference names the longest declared package its path
// starts with, and the rule after it when that package defines one. Rules are
// framework roots: OPA evaluates them by path, from outside the code. Test files
// (*_test.rego) are not read.
package regoextractor

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/facts"
)

// Extractor reads Rego policies.
type Extractor struct{}

// New creates the extractor.
func New() *Extractor { return &Extractor{} }

// Name returns the extractor name.
func (e *Extractor) Name() string { return "rego" }

func isRego(rel string) bool { return strings.EqualFold(path.Ext(rel), ".rego") }

func isRegoTest(rel string) bool { return strings.HasSuffix(strings.ToLower(rel), "_test.rego") }

// OwnsFile scopes caching to policy files.
func (e *Extractor) OwnsFile(relFile string) bool { return isRego(relFile) }

// Detect reports a policy file anywhere in the repository.
func (e *Extractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector.
func (e *Extractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if isRego(rel) && !isRegoTest(rel) {
			return true, nil
		}
	}
	return false, nil
}

var (
	pkgLine    = regexp.MustCompile(`^package\s+([A-Za-z_][\w.]*)`)
	importLine = regexp.MustCompile(`^import\s+data\.([A-Za-z_][\w.]*)(?:\s+as\s+([A-Za-z_]\w*))?`)
	// A rule head starts at the first column: `name`, `default name`, followed
	// by arguments, a key, an assignment, `if`, `contains` or a body.
	ruleHead = regexp.MustCompile(`^(?:default\s+)?([A-Za-z_]\w*)\s*(?:\(|\[|:=|=|\bif\b|\bcontains\b|\{|$)`)
	dataRef  = regexp.MustCompile(`\bdata\.([A-Za-z_][\w]*(?:\.[A-Za-z_][\w]*)*)`)
	identRef = regexp.MustCompile(`\b([A-Za-z_]\w*)((?:\.[A-Za-z_]\w*)*)`)
)

var regoKeywords = map[string]bool{
	"package": true, "import": true, "default": true, "else": true, "if": true, "in": true,
	"some": true, "every": true, "not": true, "with": true, "as": true, "contains": true,
	"true": true, "false": true, "null": true,
}

type rule struct {
	name, file string
	line       int
	body       strings.Builder
}

type policy struct {
	pkg     string
	file    string
	imports map[string]string // alias -> package path
	rules   []*rule
}

// Extract emits a symbol per rule.
func (e *Extractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var policies []*policy
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !isRego(rel) || isRegoTest(rel) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		if p := parse(string(src), rel); p != nil {
			policies = append(policies, p)
		}
	}

	// Rules by package, across files: a package may span several.
	rulesOf := map[string]map[string]bool{}
	for _, p := range policies {
		if rulesOf[p.pkg] == nil {
			rulesOf[p.pkg] = map[string]bool{}
		}
		for _, r := range p.rules {
			rulesOf[p.pkg][r.name] = true
		}
	}
	// target resolves a dotted path to the rule it names, or the package.
	target := func(dotted string) string {
		segs := strings.Split(dotted, ".")
		for i := len(segs); i > 0; i-- {
			pkg := strings.Join(segs[:i], ".")
			if _, ok := rulesOf[pkg]; !ok {
				continue
			}
			if i < len(segs) && rulesOf[pkg][segs[i]] {
				return pkg + "." + segs[i]
			}
			return ""
		}
		return ""
	}

	var out []facts.Fact
	declared := map[string]bool{}
	for _, p := range policies {
		for _, r := range p.rules {
			name := p.pkg + "." + r.name
			if declared[name] {
				continue // a later definition of the same rule
			}
			declared[name] = true
			f := facts.Fact{
				Kind: facts.KindSymbol, Name: name, File: r.file, Line: r.line,
				Props: map[string]any{
					"symbol_kind":           facts.SymbolFunc,
					"language":              "rego",
					"rego_package":          p.pkg,
					facts.PropFrameworkRoot: "opa",
				},
				Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: path.Dir(r.file)}},
			}
			seen := map[string]bool{name: true}
			add := func(t string) {
				if t != "" && !seen[t] {
					seen[t] = true
					f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: t})
				}
			}
			// Every definition of the rule in this package contributes its body.
			for _, q := range policies {
				if q.pkg != p.pkg {
					continue
				}
				for _, other := range q.rules {
					if other.name != r.name {
						continue
					}
					body := other.body.String()
					for _, m := range dataRef.FindAllStringSubmatch(body, -1) {
						add(target(m[1]))
					}
					for _, m := range identRef.FindAllStringSubmatch(body, -1) {
						head := m[1]
						if regoKeywords[head] || head == "data" || head == "input" {
							continue
						}
						if imp, ok := q.imports[head]; ok {
							add(target(imp + m[2]))
						} else if rulesOf[p.pkg][head] && head != r.name {
							add(p.pkg + "." + head)
						}
					}
				}
			}
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// parse reads a policy's package, imports and rules. A rule's body is every line
// from its head to the next first-column line.
func parse(src, rel string) *policy {
	p := &policy{file: rel, imports: map[string]string{}}
	var cur *rule
	for i, line := range strings.Split(src, "\n") {
		if j := strings.Index(line, "#"); j >= 0 && !strings.Contains(line[:j], `"`) {
			line = line[:j]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		firstColumn := line[0] != ' ' && line[0] != '\t' && line[0] != '}' && line[0] != ']' && line[0] != ')'
		if firstColumn {
			cur = nil
			switch {
			case pkgLine.MatchString(line):
				p.pkg = pkgLine.FindStringSubmatch(line)[1]
				continue
			case strings.HasPrefix(line, "import"):
				if m := importLine.FindStringSubmatch(line); m != nil {
					alias := m[2]
					if alias == "" {
						alias = m[1][strings.LastIndex(m[1], ".")+1:]
					}
					p.imports[alias] = m[1]
				}
				continue
			}
			if m := ruleHead.FindStringSubmatch(line); m != nil && !regoKeywords[m[1]] {
				cur = &rule{name: m[1], file: rel, line: i + 1}
				p.rules = append(p.rules, cur)
				rest := line[strings.Index(line, m[1])+len(m[1]):]
				cur.body.WriteString(rest + "\n")
				continue
			}
		}
		if cur != nil {
			cur.body.WriteString(line + "\n")
		}
	}
	if p.pkg == "" {
		return nil
	}
	return p
}
