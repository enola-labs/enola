package dotnetextractor

// F# — the third language on the platform.
//
// Indentation-scoped rather than delimiter-scoped, which is what makes this
// different from the VB scanner beside it: F# closes a module or type by
// dedenting, so the walker carries a scope stack keyed on column rather than
// matching an `End` keyword.
//
// F# is also the one .NET language with genuine FREE FUNCTIONS. A module-level
// `let` is not a method on anything, which matters downstream: find_orphans rates
// a `function` finding high-confidence because plain calls are reliably tracked,
// while it rates C#'s methods low. F# is therefore the only .NET language whose
// orphan output is a cleanup list rather than a set of leads.
//
// Measured before this existed: dotnet/fsharp parsed 111 files of 10,519 — the
// 5,473 `.fs` sources contributed nothing at all.

import (
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

func isFSharpFile(relFile string) bool {
	switch strings.ToLower(filepath.Ext(relFile)) {
	case ".fs", ".fsi", ".fsx":
		return true
	}
	return false
}

// ── Line preparation ────────────────────────────────────────────────────────

// fsLine is one significant line with its indentation and 1-based number.
type fsLine struct {
	text   string
	indent int
	line   int
}

// fsPrepare strips comments and blank lines, keeping indentation.
//
// `//` inside a string is not a comment, and F# block comments `(* … *)` nest.
func fsPrepare(src string) []fsLine {
	raw := strings.Split(src, "\n")
	out := make([]fsLine, 0, len(raw))
	blockDepth := 0

	for i, line := range raw {
		var b strings.Builder
		inStr := false
		for j := 0; j < len(line); j++ {
			if blockDepth > 0 {
				if j+1 < len(line) && line[j] == '*' && line[j+1] == ')' {
					blockDepth--
					j++
				} else if j+1 < len(line) && line[j] == '(' && line[j+1] == '*' {
					blockDepth++
					j++
				}
				continue
			}
			switch {
			case line[j] == '"':
				inStr = !inStr
				b.WriteByte(line[j])
			case !inStr && j+1 < len(line) && line[j] == '/' && line[j+1] == '/':
				j = len(line)
			case !inStr && j+1 < len(line) && line[j] == '(' && line[j+1] == '*':
				blockDepth++
				j++
			default:
				b.WriteByte(line[j])
			}
		}
		text := b.String()
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			continue
		}
		indent := 0
		for indent < len(text) && (text[indent] == ' ' || text[indent] == '\t') {
			indent++
		}
		out = append(out, fsLine{text: trimmed, indent: indent, line: i + 1})
	}
	return out
}

// ── Declaration patterns ────────────────────────────────────────────────────

var (
	fsNamespace = regexp.MustCompile(`^namespace\s+(?:rec\s+)?([\w.]+)`)
	fsModule    = regexp.MustCompile(`^(?:\[<[^>]*>\]\s*)?(?:public\s+|private\s+|internal\s+)?module\s+(?:rec\s+)?([\w.]+)\s*(=)?`)
	fsOpen      = regexp.MustCompile(`^open\s+(?:type\s+)?([\w.]+)`)

	fsType = regexp.MustCompile(`^(?:\[<[^>]*>\]\s*)?(?:and\s+|type\s+)(?:\[<[^>]*>\]\s*)?` +
		`(?:private\s+|internal\s+|public\s+)?([A-Za-z_]\w*)`)

	fsLet = regexp.MustCompile(`^let\s+(?:mutable\s+)?(?:rec\s+)?(?:private\s+|internal\s+|public\s+)?` +
		`(?:inline\s+)?\(?([a-zA-Z_]\w*)\)?`)
	fsMember = regexp.MustCompile(`^(?:static\s+)?(?:abstract\s+)?(?:override\s+)?(?:default\s+)?` +
		`member\s+(?:private\s+|internal\s+|public\s+)?(?:val\s+)?(?:[a-zA-Z_]\w*\.)?([a-zA-Z_]\w*)`)
	fsInterfaceImpl = regexp.MustCompile(`^interface\s+([\w.]+)`)
	fsInherit       = regexp.MustCompile(`^inherit\s+([\w.]+)`)

	// Reference positions. F# applies functions without parentheses, so a bare
	// identifier is ambiguous with a parameter name; only these four shapes are
	// harvested, which is why coverage is partial by design.
	fsQualified = regexp.MustCompile(`\b([A-Z]\w*)\.([a-zA-Z_]\w*)`)
	fsDotMember = regexp.MustCompile(`\.([a-zA-Z_]\w*)`)
	fsCall      = regexp.MustCompile(`\b([a-zA-Z_]\w*)\s*\(`)
	fsPipe      = regexp.MustCompile(`\|>\s*([a-zA-Z_][\w.]*)`)
	// F# applies functions WITHOUT parentheses, and in almost any position — as an
	// argument (`List.map decode xs`), after `then`/`else`/`->`, at the head of a
	// line. Anchoring on assignment and pipe alone left 2,579 of dotnet/fsharp's
	// 12,430 functions with no inbound edge, and find_orphans rates a `function`
	// finding HIGH confidence, so those would have shipped as actionable dead code
	// on a maintained compiler.
	//
	// So every non-keyword identifier in a body is harvested, exactly as the Razor
	// scanner does. bindMemberCall then drops any name the repository does not
	// declare as a function or method, which is what keeps this from inventing
	// edges. The residual cost is over-vouching — crediting a symbol that merely
	// shares a name — and that direction loses a dead-code LEAD rather than
	// publishing a false accusation.
	fsApply = regexp.MustCompile(`\b([a-z_]\w*)\b`)

	fsDecision = regexp.MustCompile(`(^|\s)(if|elif|match|when|while|for)\b|\|\s*[A-Z_]`)
	fsLoop     = regexp.MustCompile(`(^|\s)(for|while)\b`)
)

var fsKeywords = map[string]bool{
	"let": true, "in": true, "and": true, "or": true, "not": true, "if": true,
	"then": true, "else": true, "elif": true, "match": true, "with": true,
	"when": true, "fun": true, "function": true, "type": true, "member": true,
	"module": true, "namespace": true, "open": true, "new": true, "for": true,
	"while": true, "do": true, "done": true, "to": true, "downto": true,
	"try": true, "finally": true, "rec": true, "mutable": true, "static": true,
	"abstract": true, "override": true, "default": true, "inherit": true,
	"interface": true, "end": true, "val": true, "use": true, "yield": true,
	"return": true, "async": true, "task": true, "lazy": true, "assert": true,
	"begin": true, "class": true, "struct": true, "inline": true, "of": true,
	"private": true, "public": true, "internal": true, "upcast": true,
	"downcast": true, "true": true, "false": true, "null": true, "unit": true,
	"string": true, "int": true, "bool": true, "float": true, "double": true,
	"obj": true, "list": true, "seq": true, "option": true, "array": true,
	"failwith": true, "ignore": true, "printfn": true, "sprintf": true,
	"raise": true, "some": true, "none": true, "id": true, "fst": true, "snd": true,
}

func fsIsKeyword(s string) bool { return fsKeywords[strings.ToLower(s)] }

// ── Walker ──────────────────────────────────────────────────────────────────

type fsScope struct {
	kind   string // "module" | "type"
	name   string
	indent int
}

type fsWalker struct {
	dir     string
	relFile string
	ns      string
	scopes  []fsScope
	out     []facts.Fact
	member  int // index into out, or -1
	// Column of the member being walked. A `let` indented deeper than this is a
	// LOCAL binding inside the body, not a new module-level declaration; without
	// the distinction every local shadowed the member and its references were
	// attributed to the local instead.
	memberIndent int
}

func scanFSharp(src, relFile string) []facts.Fact {
	rel := filepath.ToSlash(relFile)
	w := &fsWalker{dir: path.Dir(rel), relFile: rel, member: -1}
	for _, ln := range fsPrepare(src) {
		w.line(ln)
	}
	return w.out
}

// ownerPath is the dotted scope chain a declaration sits in.
func (w *fsWalker) ownerPath() string {
	parts := make([]string, 0, len(w.scopes))
	for _, s := range w.scopes {
		parts = append(parts, s.name)
	}
	return strings.Join(parts, ".")
}

func (w *fsWalker) qualify(name string) string {
	if o := w.ownerPath(); o != "" {
		return w.dir + "." + o + "." + name
	}
	return w.dir + "." + name
}

// inType reports whether the innermost scope is a type, where a `let` is private
// state rather than a function.
func (w *fsWalker) inType() bool {
	return len(w.scopes) > 0 && w.scopes[len(w.scopes)-1].kind == "type"
}

func (w *fsWalker) popTo(indent int) {
	for len(w.scopes) > 0 && indent <= w.scopes[len(w.scopes)-1].indent {
		w.scopes = w.scopes[:len(w.scopes)-1]
		w.member = -1
	}
	if w.member >= 0 && indent <= w.memberIndent {
		w.member = -1
	}
}

func (w *fsWalker) cur() *facts.Fact {
	if w.member >= 0 {
		return &w.out[w.member]
	}
	if len(w.scopes) == 0 {
		return nil
	}
	want := w.dir + "." + w.ownerPath()
	for i := len(w.out) - 1; i >= 0; i-- {
		if w.out[i].Kind == facts.KindSymbol && w.out[i].Name == want {
			return &w.out[i]
		}
	}
	return nil
}

func (w *fsWalker) addRel(kind, target string) {
	f := w.cur()
	if f == nil || target == "" || target == f.Name || fsIsKeyword(target) {
		return
	}
	for _, r := range f.Relations {
		if r.Kind == kind && r.Target == target {
			return
		}
	}
	f.Relations = append(f.Relations, facts.Relation{Kind: kind, Target: target})
}

func (w *fsWalker) line(ln fsLine) {
	t := ln.text
	w.popTo(ln.indent)

	if m := fsNamespace.FindStringSubmatch(t); m != nil {
		w.ns = m[1]
		w.scopes = nil
		return
	}
	if m := fsOpen.FindStringSubmatch(t); m != nil {
		w.emitOpen(m[1], ln.line)
		return
	}
	if m := fsModule.FindStringSubmatch(t); m != nil {
		name := m[1]
		// `module A.B` with no `=` at the top of a file declares the file's module
		// rather than opening a nested scope; either way the chain is the same.
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		w.emitContainer("module", name, t, ln.line)
		w.scopes = append(w.scopes, fsScope{kind: "module", name: name, indent: ln.indent})
		w.member = -1
		return
	}
	if m := fsType.FindStringSubmatch(t); m != nil && !fsIsKeyword(m[1]) &&
		(strings.HasPrefix(t, "type ") || strings.HasPrefix(t, "and ") || strings.HasPrefix(t, "[<")) {
		w.emitContainer("type", m[1], t, ln.line)
		w.scopes = append(w.scopes, fsScope{kind: "type", name: m[1], indent: ln.indent})
		w.member = -1
		w.bodyRefs(t)
		return
	}
	if m := fsInterfaceImpl.FindStringSubmatch(t); m != nil {
		w.addRel(facts.RelImplements, shortType(m[1]))
		return
	}
	if m := fsInherit.FindStringSubmatch(t); m != nil {
		w.addRel(facts.RelImplements, shortType(m[1]))
		return
	}
	// Inside a member body every declaration is local; only references matter.
	if w.member >= 0 && ln.indent > w.memberIndent {
		w.bodyRefs(t)
		return
	}
	if m := fsMember.FindStringSubmatch(t); m != nil && !fsIsKeyword(m[1]) {
		w.openMember(facts.SymbolMethod, m[1], t, ln.line, ln.indent)
		w.bodyRefs(t)
		return
	}
	if m := fsLet.FindStringSubmatch(t); m != nil && !fsIsKeyword(m[1]) {
		// A `let` inside a TYPE is private state, not a member — the same rule the
		// C# and VB walkers apply to private fields.
		if w.inType() {
			w.bodyRefs(t)
			return
		}
		kind := fsBindingKind(t, m[1])
		w.openMember(kind, m[1], t, ln.line, ln.indent)
		w.bodyRefs(t)
		return
	}

	w.bodyRefs(t)
}

// NO ROUTE EXTRACTION, deliberately. Giraffe's routing DSL (`route`, `routef`,
// `subRoute`, `choose`) is the obvious next thing to read, and the corpus cannot
// validate it: giraffe-fsharp/Giraffe DEFINES the DSL and every use of it lives in
// tests/, which the ignore globs exclude for the same reason csharp-sdk's 31
// test-only endpoints were excluded. Shipping an unexercised route parser would
// add a claim no benchmark run can check.

func (w *fsWalker) emitOpen(pathName string, line int) {
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindDependency,
		Name: w.dir + " -> " + pathName,
		File: w.relFile,
		Line: line,
		Props: map[string]any{
			"language": "fsharp",
			"import":   pathName,
			"source":   "external", // refined by classifyUsing once the repo is indexed
		},
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: pathName}},
	})
}

func (w *fsWalker) emitContainer(kind, name, decl string, line int) {
	symKind := facts.SymbolClass
	if kind == "module" {
		symKind = facts.SymbolClass // an F# module compiles to a static class
	}
	props := map[string]any{
		"language":    "fsharp",
		"symbol_kind": symKind,
		"exported":    !strings.Contains(decl, "private"),
	}
	if kind == "module" {
		props["fsharp_module"] = true
	}
	if w.ns != "" {
		props["namespace"] = w.ns
	}
	full := w.qualify(name)
	if w.ns != "" {
		props["fqn"] = w.ns + "." + name
	}
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      full,
		File:      w.relFile,
		Line:      line,
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})
}

func (w *fsWalker) openMember(kind, name, decl string, line, indent int) {
	props := map[string]any{
		"language":    "fsharp",
		"symbol_kind": kind,
		"exported":    !strings.Contains(decl, "private"),
		"cyclomatic":  1,
	}
	if o := w.ownerPath(); o != "" {
		props["receiver"] = w.dir + "." + o
	}
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      w.qualify(name),
		File:      w.relFile,
		Line:      line,
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})
	w.member = len(w.out) - 1
	w.memberIndent = indent
}

// fsBindingKind decides whether a `let` declares a function or a value, by what
// stands between the name and the `=`. Parameters make it a function; nothing, or
// a lone type annotation, makes it a value. Testing for "(" instead would call
// `let private RouteKey = "..."` a function, because the MODIFIERS sit before the
// name and look like parameters to a naive scan.
func fsBindingKind(t, name string) string {
	i := strings.Index(t, name)
	if i < 0 {
		return facts.SymbolVariable
	}
	rest := t[i+len(name):]
	if eq := strings.Index(rest, "="); eq >= 0 {
		rest = rest[:eq]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" || strings.HasPrefix(rest, ":") {
		return facts.SymbolVariable
	}
	return facts.SymbolFunc
}

func (w *fsWalker) bodyRefs(t string) {
	if w.cur() == nil {
		return
	}
	if w.member >= 0 {
		mp := w.out[w.member].Props
		if n := len(fsDecision.FindAllString(t, -1)); n > 0 {
			if c, ok := mp["cyclomatic"].(int); ok {
				mp["cyclomatic"] = c + n
			}
		}
		if fsLoop.MatchString(t) {
			c, _ := mp["loop_count"].(int)
			mp["loop_count"] = c + 1
			if d, _ := mp["loop_depth"].(int); d < 1 {
				mp["loop_depth"] = 1
			}
		}
	}

	for _, m := range fsQualified.FindAllStringSubmatch(t, -1) {
		w.addRel(facts.RelCalls, m[1])
		w.addRel(facts.RelCalls, m[2])
		if ioStaticTypes[m[1]] || ioMethods[m[2]] {
			w.markIO()
		}
	}
	for _, m := range fsDotMember.FindAllStringSubmatch(t, -1) {
		w.addRel(facts.RelCalls, m[1])
		if ioMethods[m[1]] {
			w.markIO()
		}
	}
	for _, m := range fsCall.FindAllStringSubmatch(t, -1) {
		w.addRel(facts.RelCalls, m[1])
		if ioMethods[m[1]] {
			w.markIO()
		}
	}
	for _, m := range fsPipe.FindAllStringSubmatch(t, -1) {
		w.addRel(facts.RelCalls, shortType(m[1]))
	}
	for _, m := range fsApply.FindAllStringSubmatch(t, -1) {
		w.addRel(facts.RelCalls, m[1])
	}
}

func (w *fsWalker) markIO() {
	if w.member >= 0 {
		w.out[w.member].SetProp("io_direct", true)
	}
}
