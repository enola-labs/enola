package tsextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

type fileRefs struct {
	kinds   *tsutil.KindTable
	src     []byte
	ctx     *extractCtx
	aliases map[string]tsAlias
	fileDir string

	internal    map[string]string // local name -> canonical target (internal modules only)
	namespaces  map[string]string // `import * as ns` local -> module dir
	reexports   []string          // canonical targets re-exported via `export { x } from './y'`
	defaultRefs []string          // default-export targets of default-imported modules

	targets []string
	seen    map[string]bool
}

// collectTSFileRefs performs a whole-file reference pass for the dead-code detector.
//
// The per-function call walk (collectCallsWithMetrics) only records call_expression
// edges inside function bodies, so it misses the ways a React/CommonJS codebase
// actually uses a symbol: rendering a component in JSX (<Foo/>), passing an imported
// identifier as a value (route configs — `{ component: Foo }`), namespace member
// access (`ns.foo`), and require()-bound names. Many of these live at module scope
// (top-level route arrays), which has no enclosing symbol fact to hang an edge on.
//
// We fold every such reference into a single KindFileRef fact — the reference-only
// carrier the dead-code detector already consumes for top-level references — so
// genuinely-used code is not mis-reported as dead. References are matched downstream
// by short name, so binding a local import name to "<moduleDir>.<name>" is enough
// even when the canonical declaration lives behind a folder index (the last segment
// still matches). This only ever ADDS references, so it can hide a real orphan but
// never invent a false one — the detector's deliberate conservative bias.
//
// kind selects the emitted fact kind: facts.KindFileRef for the production
// file-scope pass, facts.KindTestRef when the caller is ExtractTestRefs walking a
// test/spec file. The walk and resolvers are identical either way — a reference
// from a test is spelled exactly as the same reference from production code.
func (e *TSExtractor) collectTSFileRefs(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx, aliases map[string]tsAlias, kind string) []facts.Fact {
	r := &fileRefs{
		kinds:      kinds,
		src:        ctx.src,
		ctx:        ctx,
		aliases:    aliases,
		fileDir:    factpath.Dir(ctx.relFile),
		internal:   make(map[string]string),
		namespaces: make(map[string]string),
		seen:       make(map[string]bool),
	}
	// Pass 1: parse the bindings — static imports, `export … from` re-exports, and
	// require()/dynamic-import assignments — into name → target maps.
	for i := range root.ChildCount() {
		r.bindStatement(root.Child(i))
	}
	// Pass 2: walk the whole tree collecting references — to the imported/required
	// bindings above and, in call positions, to same-module declarations.
	r.walk(root)
	for _, t := range r.reexports {
		r.add(t)
	}
	for _, t := range r.defaultRefs {
		r.add(t)
	}
	return r.fact(kind)
}

func (r *fileRefs) fact(kind string) []facts.Fact {
	if len(r.targets) == 0 {
		return nil
	}
	rels := make([]facts.Relation, 0, len(r.targets))
	for _, t := range r.targets {
		rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: t})
	}
	return []facts.Fact{{
		Kind:      kind,
		Name:      r.ctx.relFile,
		File:      r.ctx.relFile,
		Line:      1,
		Props:     map[string]any{"language": "typescript"},
		Relations: rels,
	}}
}

func (r *fileRefs) add(t string) {
	if t != "" && !r.seen[t] {
		r.seen[t] = true
		r.targets = append(r.targets, t)
	}
}

func (r *fileRefs) bind(local, moduleDir, exportName string) {
	if local != "" {
		r.internal[local] = moduleDir + "." + exportName
	}
}

// resolveModule resolves an import specifier to its owning module directory and,
// when the backing file is known, that file's path (for computing the module's
// default-export name). ok is false for external modules. It prefers the exact
// file/folder-index from the known-files set — so a folder module resolves to the
// folder itself, not its parent — falling back to filepath.Dir when the target
// isn't indexed (still lets short-name matching link named imports).
func (r *fileRefs) resolveModule(node *sitter.Node) (moduleDir, indexPath string, ok bool) {
	if node == nil {
		return "", "", false
	}
	importPath := strings.Trim(nodeText(node, r.src), `"'`)
	resolved, isExternal := resolveImportPath(importPath, r.fileDir, r.aliases)
	if isExternal {
		return "", "", false
	}
	if idx, dir, found := resolveModuleFile(resolved, r.ctx.knownFiles); found {
		return dir, idx, true
	}
	return factpath.Dir(resolved), "", true
}

func (r *fileRefs) bindStatement(child *sitter.Node) {
	switch kindOf(r.kinds, child) {
	case "import_statement":
		r.bindImport(child)
	case "export_statement":
		r.bindReexport(child)
	case "lexical_declaration", "variable_declaration":
		r.bindRequires(child)
	}
}

func (r *fileRefs) bindImport(child *sitter.Node) {
	moduleDir, indexPath, ok := r.resolveModule(findChildByKind(r.kinds, child, "string"))
	if !ok {
		return
	}
	clause := findChildByKind(r.kinds, child, "import_clause")
	if clause == nil {
		return
	}
	for j := range clause.ChildCount() {
		c := clause.Child(j)
		switch kindOf(r.kinds, c) {
		case "identifier": // default import: `import Foo from './x'`
			local := nodeText(c, r.src)
			r.bind(local, moduleDir, local)
			// A default import IS a use of the module's default export, whose
			// symbol is named by fileSymbolName (an anonymous
			// `export default connect(...)(X)` in a folder index becomes
			// "<Folder>Index" — unmatchable by the local name). Record it so the
			// wrapper symbol is not falsely reported dead.
			if indexPath != "" {
				r.defaultRefs = append(r.defaultRefs, moduleDir+"."+fileSymbolName(indexPath))
			}
		case "namespace_import": // `import * as ns from './x'`
			if id := findChildByKind(r.kinds, c, "identifier"); id != nil {
				r.namespaces[nodeText(id, r.src)] = moduleDir
			}
		case "named_imports":
			r.bindNamedImports(c, moduleDir)
		}
	}
}

func (r *fileRefs) bindNamedImports(named *sitter.Node, moduleDir string) {
	for k := range named.ChildCount() {
		spec := named.Child(k)
		if kindOf(r.kinds, spec) != "import_specifier" {
			continue
		}
		nameNode := spec.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		exportName := nodeText(nameNode, r.src)
		local := exportName
		if a := spec.ChildByFieldName("alias"); a != nil {
			local = nodeText(a, r.src)
		}
		r.bind(local, moduleDir, exportName)
	}
}

// `export { a, default as b } from './y'` re-exports y's symbols; record a
// reference to each so a symbol consumed only through a barrel is not
// mis-reported as dead (matched by short name downstream).
func (r *fileRefs) bindReexport(child *sitter.Node) {
	moduleDir, indexPath, ok := r.resolveModule(child.ChildByFieldName("source"))
	if !ok {
		return
	}
	clause := findChildByKind(r.kinds, child, "export_clause")
	if clause == nil {
		return
	}
	for k := range clause.ChildCount() {
		spec := clause.Child(k)
		if kindOf(r.kinds, spec) != "export_specifier" {
			continue
		}
		nameNode := spec.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		// `export { default as X } from './y'` re-exports y's default; the
		// literal name "default" matches no symbol, so resolve it to y's
		// default-export name (fileSymbolName) instead.
		if name := nodeText(nameNode, r.src); name == "default" && indexPath != "" {
			r.reexports = append(r.reexports, moduleDir+"."+fileSymbolName(indexPath))
		} else {
			r.reexports = append(r.reexports, moduleDir+"."+name)
		}
	}
}

// CommonJS: `const x = require('./y')` / `const { a } = require('./y')`.
func (r *fileRefs) bindRequires(child *sitter.Node) {
	for j := range child.ChildCount() {
		d := child.Child(j)
		if kindOf(r.kinds, d) != "variable_declarator" {
			continue
		}
		val := d.ChildByFieldName("value")
		if val == nil || kindOf(r.kinds, val) != "call_expression" {
			continue
		}
		fn := val.ChildByFieldName("function")
		if fn == nil || nodeText(fn, r.src) != "require" {
			continue
		}
		moduleDir, _, ok := r.resolveModule(findChildByKind(r.kinds, val.ChildByFieldName("arguments"), "string"))
		if !ok {
			continue
		}
		nameNode := d.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		switch kindOf(r.kinds, nameNode) {
		case "identifier":
			local := nodeText(nameNode, r.src)
			r.bind(local, moduleDir, local)
		case "object_pattern":
			for k := range nameNode.ChildCount() {
				if p := nameNode.Child(k); kindOf(r.kinds, p) == "shorthand_property_identifier_pattern" {
					nm := nodeText(p, r.src)
					r.bind(nm, moduleDir, nm)
				}
			}
		}
	}
}

func (r *fileRefs) walk(n *sitter.Node) {
	if n == nil {
		return
	}
	switch kindOf(r.kinds, n) {
	case "import_statement":
		return // binding sites, not uses
	case "identifier", "type_identifier":
		// type_identifier covers an imported type/interface used only as an
		// annotation (`repo: Repo`), which is otherwise never an edge.
		if t, ok := r.internal[nodeText(n, r.src)]; ok {
			r.add(t)
		}
		return
	case "member_expression":
		if r.memberRef(n) {
			return
		}
	case "jsx_opening_element", "jsx_self_closing_element":
		if nameNode := n.ChildByFieldName("name"); nameNode != nil {
			r.add(resolveJSXTag(r.kinds, nameNode, r.src, r.ctx.dir, r.internal, r.namespaces))
		}
	case "call_expression":
		r.callRefs(n)
	}
	for i := range n.ChildCount() {
		r.walk(n.Child(i))
	}
}

// memberRef records `ns.foo` and `Foo.bar` on an imported name, and reports whether
// the member expression is fully handled (a namespace member).
func (r *fileRefs) memberRef(n *sitter.Node) bool {
	obj := n.ChildByFieldName("object")
	prop := n.ChildByFieldName("property")
	if obj == nil || kindOf(r.kinds, obj) != "identifier" {
		return false
	}
	name := nodeText(obj, r.src)
	if dir, ok := r.namespaces[name]; ok && prop != nil {
		r.add(dir + "." + nodeText(prop, r.src)) // ns.foo -> <dir>.foo
		return true
	}
	if t, ok := r.internal[name]; ok {
		r.add(t) // Foo.bar on imported Foo marks Foo used
	}
	return false
}

// A bare callee and identifier arguments are USE positions (never
// declarations), so it is safe to resolve them same-module as well as via
// imports. This catches module-scope calls the per-function walk cannot see
// (`startSession()` at file top level) and functions passed as values
// (`connect(mapStateToProps, actions)`, HOC/callback wiring) — otherwise a
// symbol used only that way is falsely reported dead.
func (r *fileRefs) callRefs(n *sitter.Node) {
	if fn := n.ChildByFieldName("function"); fn != nil && kindOf(r.kinds, fn) == "identifier" {
		r.add(resolveLocalOrImport(nodeText(fn, r.src), r.ctx.dir, r.internal))
	}
	args := n.ChildByFieldName("arguments")
	if args == nil {
		return
	}
	for i := range args.ChildCount() {
		if a := args.Child(i); kindOf(r.kinds, a) == "identifier" {
			r.add(resolveLocalOrImport(nodeText(a, r.src), r.ctx.dir, r.internal))
		}
	}
}
