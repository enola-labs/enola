package rubyextractor

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
	sitter "github.com/tree-sitter/go-tree-sitter"
	ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
)

// Grape is the second HTTP framework a Rails codebase serves from, and the extractor
// read none of it. GitLab's entire v4 REST API — 1,033 files, ~1,530 verb sites nested
// under 382 `resource` and 318 `namespace` blocks — was invisible, mounted into Rails
// through a single `mount ::API::API => '/'` line that resolved to nothing.
//
// Grape differs from the Rails router in three ways that matter here:
//
//  1. There is no route FILE. Routes are declared in the body of a class, and the
//     classes are spread across the tree like any other source.
//  2. A Grape API is identified by inheritance, and almost never directly: GitLab has
//     exactly one class inheriting `Grape::API::Instance` (API::Base) and a thousand
//     inheriting that. So the set of Grape classes is a transitive closure over
//     superclasses, which cannot be decided one file at a time.
//  3. Composition is by `mount`, class to class, and the mount site's namespace nesting
//     contributes to the path. So a route's real URL depends on where its class was
//     mounted, which is in a different file.
//
// The closure in (2) is computed from the class facts the main AST pass already emits —
// every class symbol carries a `superclass` prop — so identifying the Grape classes
// costs no extra I/O. Only the files that survive that filter are parsed again for
// their route bodies.

// grapeBaseRoots are the constants Grape itself defines. Everything else is a Grape API
// only by inheriting, transitively, from one of these.
var grapeBaseRoots = []string{"Grape::API", "Grape::API::Instance"}

// grapeClass is one Grape API class: what it serves, what it mounts, and the prefix it
// imposes on both.
type grapeClass struct {
	name    string // qualified, e.g. "API::Lint"
	file    string
	line    int
	prefix  string // from class-level `prefix`/`version`, e.g. "/api/v4"
	mounts  []grapeMount
	routes  []grapeRoute
	visited bool // guards the prefix walk against a mount cycle
}

type grapeMount struct {
	constant string // the mounted class, e.g. "API::Projects"
	path     string // the namespace path at the mount site, "" at class top level
	line     int
}

type grapeRoute struct {
	method string
	path   string
	line   int
}

// extractGrapeRoutes finds every Grape API class reachable in the repository and emits
// its routes at the URL they are actually served at.
//
// classFacts is the class symbols from the main AST pass; it is read for the
// name/superclass/file triples only. The returned facts are appended to the snapshot
// like any other route.
func extractGrapeRoutes(ctx context.Context, repoPath string, classFacts []facts.Fact) []facts.Fact {
	grapeFiles := grapeAPIFiles(classFacts)
	if len(grapeFiles) == 0 {
		return nil
	}

	// Parse only the confirmed Grape files, in parallel, in file order.
	perFile := parallel.MapFiles(ctx, grapeFiles, func(relFile string) []grapeClass {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[ruby-extractor] error reading grape file %s: %v", relFile, err)
			return nil
		}
		return parseGrapeFile(src, relFile)
	})

	byName := map[string]*grapeClass{}
	var order []string
	for _, classes := range perFile {
		for i := range classes {
			c := classes[i]
			if _, seen := byName[c.name]; seen {
				continue
			}
			byName[c.name] = &c
			order = append(order, c.name)
		}
	}
	if len(byName) == 0 {
		return nil
	}

	// A class that something else mounts is not a root; its prefix comes from its
	// mount site. Everything else is a root and starts at its own prefix.
	mounted := map[string]bool{}
	for _, name := range order {
		for _, m := range byName[name].mounts {
			mounted[normalizeConstant(m.constant)] = true
		}
	}

	var out []facts.Fact
	var emit func(c *grapeClass, mountPoint string)
	emit = func(c *grapeClass, mountPoint string) {
		if c == nil || c.visited {
			return
		}
		c.visited = true
		base := mountPoint + c.prefix
		for _, r := range c.routes {
			path := base + r.path
			if path == "" {
				path = "/"
			}
			out = append(out, facts.Fact{
				Kind: facts.KindRoute,
				Name: path,
				File: c.file,
				Line: r.line,
				Props: map[string]any{
					"method":    r.method,
					"framework": "grape",
					"language":  "ruby",
					"handler":   c.name,
				},
				Relations: []facts.Relation{
					{Kind: facts.RelDeclares, Target: factpath.Dir(c.file)},
					{Kind: facts.RelHandledBy, Target: c.name},
				},
			})
		}
		for _, m := range c.mounts {
			emit(byName[normalizeConstant(m.constant)], base+m.path)
		}
		c.visited = false
	}

	sort.Strings(order)
	for _, name := range order {
		if !mounted[name] {
			emit(byName[name], "")
		}
	}
	return out
}

// grapeAPIFiles returns the files declaring a Grape API class, computed as the
// transitive closure of `superclass` over the class facts. Sorted, so a run is
// reproducible.
func grapeAPIFiles(classFacts []facts.Fact) []string {
	// children maps a superclass name to the classes that inherit it. Superclasses are
	// compared after stripping a leading "::" so `::API::Base` and `API::Base` unify.
	children := map[string][]facts.Fact{}
	for _, f := range classFacts {
		if f.Kind != facts.KindSymbol || f.Props == nil {
			continue
		}
		if f.PropAny("symbol_kind") != facts.SymbolClass {
			continue
		}
		super, _ := f.PropAny("superclass").(string)
		if super == "" {
			continue
		}
		children[normalizeConstant(super)] = append(children[normalizeConstant(super)], f)
	}

	seen := map[string]bool{}
	files := map[string]bool{}
	queue := append([]string{}, grapeBaseRoots...)
	for len(queue) > 0 {
		base := queue[0]
		queue = queue[1:]
		if seen[base] {
			continue
		}
		seen[base] = true
		for _, f := range children[base] {
			files[f.File] = true
			// Both the qualified name and its last segment: a class written
			// `class Base < Grape::API::Instance` inside `module API` is referenced by
			// subclasses as `::API::Base`, but a sibling in the same module may write
			// just `Base`.
			queue = append(queue, f.Name)
			if i := strings.LastIndex(f.Name, "::"); i >= 0 {
				queue = append(queue, f.Name[i+2:])
			}
		}
	}

	out := make([]string, 0, len(files))
	for f := range files {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// --- per-file Grape parse ---

// parseGrapeFile walks a file's Grape API classes, collecting their routes, mounts and
// class-level prefix.
func parseGrapeFile(src []byte, relFile string) []grapeClass {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(ruby.Language())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	g := &grapeWalker{src: src, relFile: relFile}
	g.walkTop(tree.RootNode(), nil)
	return g.classes
}

type grapeWalker struct {
	src     []byte
	relFile string
	classes []grapeClass
}

// walkTop descends through module/class nesting looking for class bodies, tracking the
// module path so a class's name comes out qualified.
func (g *grapeWalker) walkTop(node *sitter.Node, nesting []string) {
	if node == nil {
		return
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		c := node.Child(i)
		switch kindOf(c) {
		case "module":
			name := rubyText(c.ChildByFieldName("name"), g.src)
			g.walkTop(c.ChildByFieldName("body"), append(append([]string{}, nesting...), name))
		case "class":
			name := rubyText(c.ChildByFieldName("name"), g.src)
			if name == "" {
				continue
			}
			qualified := strings.Join(append(append([]string{}, nesting...), name), "::")
			cls := grapeClass{name: qualified, file: g.relFile, line: line(c)}
			body := c.ChildByFieldName("body")
			g.walkBody(body, "", &cls)
			if len(cls.routes) > 0 || len(cls.mounts) > 0 || cls.prefix != "" {
				g.classes = append(g.classes, cls)
			}
			// A Grape API may nest further classes; keep descending.
			g.walkTop(body, append(append([]string{}, nesting...), name))
		default:
			// `class << self`, `if`, `begin`: descend without changing the nesting.
			g.walkTop(c, nesting)
		}
	}
}

// walkBody walks the statements of a Grape class body (or of a nested
// namespace/resource block) at URL prefix `at`.
func (g *grapeWalker) walkBody(node *sitter.Node, at string, cls *grapeClass) {
	if node == nil {
		return
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		c := node.Child(i)
		if kindOf(c) == "call" {
			g.handleGrapeCall(c, at, cls)
		}
	}
}

func (g *grapeWalker) handleGrapeCall(call *sitter.Node, at string, cls *grapeClass) {
	method := rubyText(call.ChildByFieldName("method"), g.src)
	args := call.ChildByFieldName("arguments")
	body := blockBody(call)

	switch method {
	case "get", "post", "put", "patch", "delete", "head", "options":
		// The path argument is optional: `get do ... end` serves the enclosing
		// namespace path itself.
		path := grapeSegment(firstPositionalPath(args, g.src))
		cls.routes = append(cls.routes, grapeRoute{
			method: strings.ToUpper(method),
			path:   at + path,
			line:   line(call),
		})

	case "namespace", "resource", "resources", "group", "segment":
		// All five are the same construct in Grape — a path segment with a block.
		// A namespace with no name (`namespace do`) adds nothing to the path.
		seg := grapeSegment(firstPositionalPath(args, g.src))
		if body != nil {
			g.walkBody(body, at+seg, cls)
		}

	case "route_param":
		// `route_param :id do ... end` is a path parameter segment.
		name := firstSymbolArg(args, g.src)
		if name == "" {
			name = firstStringArg(args, g.src)
		}
		if name != "" && body != nil {
			g.walkBody(body, at+"/:"+name, cls)
		}

	case "prefix":
		// Class-level; applies to everything the class serves and mounts.
		cls.prefix += grapeSegment(firstPositionalPath(args, g.src))

	case "version":
		// `version 'v4', using: :path` prefixes the path; `using: :header` and
		// `using: :param` do not touch the URL, so they contribute nothing.
		if pairSymbol(args, "using", g.src) != "path" {
			// The default IS :path when `using:` is absent, which is the common form.
			if findPairValue(args, "using", g.src) != nil {
				if body != nil {
					g.walkBody(body, at, cls)
				}
				return
			}
		}
		seg := grapeSegment(firstPositionalPath(args, g.src))
		if body != nil {
			// The block form scopes the version to its contents.
			g.walkBody(body, at+seg, cls)
			return
		}
		cls.prefix += seg

	case "mount":
		// `mount ::API::Projects` — the mounted class serves below the current path.
		// Grape also accepts `mount Foo => '/bar'`, which overrides the path.
		constant, mountAt := parseMount(args, g.src)
		if constant == "" {
			return
		}
		path := at
		if mountAt != "" && mountAt != "/" {
			path = at + grapeSegment(mountAt)
		}
		cls.mounts = append(cls.mounts, grapeMount{constant: constant, path: path, line: line(call)})

	case "params", "desc", "helpers", "before", "after", "rescue_from", "format",
		"content_type", "use", "include", "extend", "insert_before", "require":
		// Metadata and middleware: no path contribution, and their blocks contain no
		// routes. Skipping them explicitly keeps a `params do ... end` block from being
		// descended into by the catch-all below.

	default:
		// An unrecognized DSL call that carries a block may still wrap routes
		// (`authenticate do`, `if Gitlab.ee?`), so descend without changing the path.
		if body != nil {
			g.walkBody(body, at, cls)
		}
	}
}

// grapeSegment normalizes a Grape path fragment into a leading-slash segment. Grape
// accepts ':id', '/:id' and :id interchangeably, and a bare verb with no path
// contributes nothing.
func grapeSegment(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimSuffix(p, "/")
}
