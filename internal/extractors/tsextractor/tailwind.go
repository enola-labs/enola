package tsextractor

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// Tailwind design tokens: the colors, spacing, radii and the rest a project's
// Tailwind theme declares, and the components whose class strings use them. A
// token is a constant symbol (dead-code leaves data members out by default) named
// tailwind/<section>.<token>; a class naming it (`bg-app-bg-accent`) is a names
// edge from the component, or the file, to the token. Only tokens the repository
// declares are matched: Tailwind's built-in palette (`bg-red-500`) is not, so a
// class matches only a name somebody here chose.
const (
	propToken         = "token"
	propTokenSection  = "token_section"
	propTokenSource   = "token_source"
	tokenSourceTW     = "tailwind"
	tailwindTokenRoot = "tailwind/"
)

// tailwindSections maps a theme section to the utility prefixes that take its
// tokens. `text` serves both colors and fontSize; a class is matched against every
// section its prefix serves.
var tailwindSections = map[string][]string{
	"colors": {"bg", "text", "border", "border-x", "border-y", "border-t", "border-r", "border-b", "border-l",
		"ring", "ring-offset", "outline", "fill", "stroke", "from", "via", "to", "divide", "placeholder",
		"caret", "accent", "decoration", "shadow"},
	"backgroundColor": {"bg"}, "textColor": {"text"}, "borderColor": {"border"},
	"spacing": {"p", "px", "py", "pt", "pr", "pb", "pl", "m", "mx", "my", "mt", "mr", "mb", "ml", "gap", "gap-x",
		"gap-y", "space-x", "space-y", "w", "h", "min-w", "min-h", "max-w", "max-h", "inset", "top", "right",
		"bottom", "left", "size", "translate-x", "translate-y"},
	"fontSize":           {"text"},
	"fontFamily":         {"font"},
	"fontWeight":         {"font"},
	"borderRadius":       {"rounded", "rounded-t", "rounded-r", "rounded-b", "rounded-l", "rounded-tl", "rounded-tr", "rounded-br", "rounded-bl"},
	"boxShadow":          {"shadow"},
	"zIndex":             {"z"},
	"width":              {"w"},
	"height":             {"h"},
	"maxWidth":           {"max-w"},
	"lineHeight":         {"leading"},
	"letterSpacing":      {"tracking"},
	"backgroundImage":    {"bg"},
	"animation":          {"animate"},
	"transitionProperty": {"transition"},
}

// tailwindV4Namespaces maps a v4 @theme variable namespace to its v3 section.
var tailwindV4Namespaces = map[string]string{
	"color": "colors", "spacing": "spacing", "font": "fontFamily", "text": "fontSize",
	"radius": "borderRadius", "shadow": "boxShadow", "leading": "lineHeight", "tracking": "letterSpacing",
}

var (
	tailwindConfigRe = regexp.MustCompile(`(^|/)tailwind\.config\.(js|cjs|mjs|ts|cts|mts)$`)
	themeBlockRe     = regexp.MustCompile(`@theme(?:\s+[a-z]+)?\s*\{([^}]*)\}`)
	themeVarRe       = regexp.MustCompile(`--([a-z]+)-([A-Za-z0-9_-]+)\s*:\s*([^;]+);`)
	classStringRe    = regexp.MustCompile("\"([^\"\\n]*)\"|'([^'\\n]*)'|`([^`]*)`")
)

type tailwindToken struct {
	section, name, value, file string
	line                       int
	// prefix is set, alone, on the entry recording a config's `prefix: "tw-"`.
	prefix string
}

// tailwindTokens reads the tokens a repository's Tailwind themes declare: every
// tailwind.config.*, every preset or plugin module (a file importing from
// "tailwindcss"), and every @theme block in CSS.
func tailwindTokens(repoPath string, files []string) []tailwindToken {
	var out []tailwindToken
	for _, f := range files {
		rel := filepath.ToSlash(f)
		if facts.IsTestPath(rel) {
			continue
		}
		switch ext := path.Ext(rel); {
		case ext == ".css" || ext == ".scss" || ext == ".pcss":
			src, err := os.ReadFile(filepath.Join(repoPath, f)) //factpath:host
			if err != nil || !strings.Contains(string(src), "@theme") {
				continue
			}
			out = append(out, tailwindV4Tokens(string(src), rel)...)
		case tailwindConfigRe.MatchString(rel):
			out = append(out, readTailwindTheme(repoPath, rel)...)
		case ext == ".ts" || ext == ".js" || ext == ".cjs" || ext == ".mjs":
			src, err := os.ReadFile(filepath.Join(repoPath, f)) //factpath:host
			if err != nil || !importsTailwind(string(src)) {
				continue
			}
			out = append(out, readTailwindTheme(repoPath, rel)...)
		}
	}
	return out
}

func tailwindV4Tokens(src, rel string) []tailwindToken {
	var out []tailwindToken
	for _, block := range themeBlockRe.FindAllStringSubmatchIndex(src, -1) {
		body := src[block[2]:block[3]]
		for _, m := range themeVarRe.FindAllStringSubmatchIndex(body, -1) {
			section := tailwindV4Namespaces[body[m[2]:m[3]]]
			if section == "" {
				continue
			}
			line := 1 + strings.Count(src[:block[2]+m[0]], "\n")
			out = append(out, tailwindToken{section: section, name: body[m[4]:m[5]], value: strings.TrimSpace(body[m[6]:m[7]]), file: rel, line: line})
		}
	}
	return out
}

// themeReader evaluates the object literals a theme is written as, following
// spreads and identifiers into the files they are imported from (relative imports,
// a few hops), the way a preset spreads `theme.extend` from its own module.
type themeReader struct {
	repoPath string
	kinds    *tsutil.KindTable
	parsed   map[string]*themeFile
}

type themeFile struct {
	rel     string
	src     []byte
	root    *sitter.Node
	tree    *sitter.Tree
	imports map[string]string // local binding -> repo-relative file stem
	consts  map[string]*sitter.Node
}

func readTailwindTheme(repoPath, rel string) []tailwindToken {
	r := &themeReader{repoPath: repoPath, kinds: tsKindsFor(false), parsed: map[string]*themeFile{}}
	defer r.close()
	tf := r.file(rel)
	if tf == nil {
		return nil
	}
	var out []tailwindToken
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if kindOf(r.kinds, n) == "pair" {
			switch key := strings.Trim(nodeText(n.ChildByFieldName("key"), tf.src), `"'`); {
			case key == "theme":
				out = append(out, r.theme(tf, n.ChildByFieldName("value"), 0)...)
				return
			case key == "prefix" && kindOf(r.kinds, n.ChildByFieldName("value")) == "string":
				out = append(out, tailwindToken{prefix: strings.Trim(nodeText(n.ChildByFieldName("value"), tf.src), `"'`)})
				return
			}
		}
		for i := range n.NamedChildCount() {
			walk(n.NamedChild(i))
		}
	}
	walk(tf.root)
	// A theme module that is spread into a preset elsewhere (`export const theme = {
	// extend: {...} }`) is read where it is declared too.
	for name, v := range tf.consts {
		if strings.EqualFold(name, "theme") {
			out = append(out, r.theme(tf, v, 0)...)
		}
	}
	return out
}

func (r *themeReader) close() {
	for _, f := range r.parsed {
		if f != nil && f.tree != nil {
			f.tree.Close()
		}
	}
}

func (r *themeReader) file(rel string) *themeFile {
	if tf, ok := r.parsed[rel]; ok {
		return tf
	}
	r.parsed[rel] = nil
	src, err := os.ReadFile(filepath.Join(r.repoPath, rel)) //factpath:host
	if err != nil {
		return nil
	}
	parser := sitter.NewParser()
	defer parser.Close()
	if parser.SetLanguage(sitter.NewLanguage(typescript.LanguageTypescript())) != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	tf := &themeFile{rel: rel, src: src, root: tree.RootNode(), tree: tree, imports: map[string]string{}, consts: map[string]*sitter.Node{}}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		switch kindOf(r.kinds, n) {
		case "import_statement":
			if s := findChildByKind(r.kinds, n, "string"); s != nil {
				spec := strings.Trim(nodeText(s, src), `"'`)
				if strings.HasPrefix(spec, ".") {
					target := factpath.Clean(path.Join(path.Dir(rel), spec))
					for _, id := range regexp.MustCompile(`[A-Za-z_$][\w$]*`).FindAllString(nodeText(n, src)[:strings.LastIndex(nodeText(n, src), "from")], -1) {
						if id != "import" && id != "type" && id != "as" {
							tf.imports[id] = target
						}
					}
				}
			}
			return
		case "variable_declarator":
			if name, val := n.ChildByFieldName("name"), n.ChildByFieldName("value"); name != nil && val != nil {
				tf.consts[nodeText(name, src)] = val
			}
		}
		for i := range n.NamedChildCount() {
			walk(n.NamedChild(i))
		}
	}
	walk(tf.root)
	r.parsed[rel] = tf
	return tf
}

// object resolves n to an object literal and the file it is written in: directly,
// through a module constant, an import, a member path (`theme.extend`) or
// `satisfies`/`as` wrappers.
func (r *themeReader) object(tf *themeFile, n *sitter.Node, hops int) (*themeFile, *sitter.Node) {
	if n == nil || hops > 4 {
		return nil, nil
	}
	switch kindOf(r.kinds, n) {
	case "object":
		return tf, n
	case "satisfies_expression", "as_expression", "parenthesized_expression":
		return r.object(tf, n.NamedChild(0), hops)
	case "identifier":
		name := nodeText(n, tf.src)
		if v, ok := tf.consts[name]; ok {
			return r.object(tf, v, hops+1)
		}
		if stem, ok := tf.imports[name]; ok {
			for _, ext := range []string{".ts", ".js", ".mjs", ".cjs", "/index.ts", "/index.js"} {
				if other := r.file(stem + ext); other != nil {
					if v, ok := other.consts[name]; ok {
						return r.object(other, v, hops+1)
					}
				}
			}
		}
	case "member_expression":
		of, obj := r.object(tf, n.ChildByFieldName("object"), hops+1)
		if obj == nil {
			return nil, nil
		}
		key := nodeText(n.ChildByFieldName("property"), tf.src)
		for _, p := range r.pairs(of, obj, hops+1) {
			if p.key == key {
				return r.object(p.file, p.value, hops+1)
			}
		}
	}
	return nil, nil
}

type themePair struct {
	key   string
	value *sitter.Node
	file  *themeFile
}

// pairs lists an object's key/value pairs, spreads expanded.
func (r *themeReader) pairs(tf *themeFile, obj *sitter.Node, hops int) []themePair {
	var out []themePair
	for i := range obj.NamedChildCount() {
		c := obj.NamedChild(i)
		switch kindOf(r.kinds, c) {
		case "pair":
			out = append(out, themePair{key: strings.Trim(nodeText(c.ChildByFieldName("key"), tf.src), `"'`), value: c.ChildByFieldName("value"), file: tf})
		case "spread_element":
			if of, o := r.object(tf, c.NamedChild(0), hops+1); o != nil {
				out = append(out, r.pairs(of, o, hops+1)...)
			}
		}
	}
	return out
}

// theme reads a theme object: its sections, and those of its extend.
func (r *themeReader) theme(tf *themeFile, n *sitter.Node, hops int) []tailwindToken {
	of, obj := r.object(tf, n, hops)
	if obj == nil {
		return nil
	}
	var out []tailwindToken
	for _, p := range r.pairs(of, obj, hops) {
		if p.key == "extend" {
			out = append(out, r.theme(p.file, p.value, hops)...)
			continue
		}
		if _, ok := tailwindSections[p.key]; ok {
			out = append(out, r.section(p.file, p.key, p.value, "", hops)...)
		}
	}
	return out
}

// section flattens a theme section's nested keys the way Tailwind names them:
// app.bg.accent is app-bg-accent, and DEFAULT is the name of its parent.
func (r *themeReader) section(tf *themeFile, section string, n *sitter.Node, prefix string, hops int) []tailwindToken {
	of, obj := r.object(tf, n, hops)
	if obj == nil {
		return nil
	}
	var out []tailwindToken
	for _, p := range r.pairs(of, obj, hops) {
		name := p.key
		if name == "DEFAULT" {
			name = prefix
		} else if prefix != "" {
			name = prefix + "-" + name
		}
		if name == "" {
			continue
		}
		switch kindOf(r.kinds, p.value) {
		case "string", "template_string", "number", "array", "call_expression":
			out = append(out, tailwindToken{section: section, name: name, value: strings.Trim(nodeText(p.value, p.file.src), "\"'`"),
				file: p.file.rel, line: int(p.value.StartPosition().Row) + 1})
		default:
			// Nesting is not a hop: only following a reference is.
			out = append(out, r.section(p.file, section, p.value, name, hops)...)
		}
	}
	return out
}

// bindTailwind adds a token symbol per declared token and a names edge from each
// production file's component (a .vue component, or the one component a file
// declares) or its file_ref to every token its class strings use.
func bindTailwind(all []facts.Fact, tokens []tailwindToken, repoPath string, files, tsFiles []string, sources map[string][]byte) []facts.Fact {
	if len(tokens) == 0 {
		return all
	}
	usesTailwind := tailwindProjects(repoPath, files)
	tokenFiles := map[string]bool{}
	for _, t := range tokens {
		if t.file != "" {
			tokenFiles[t.file] = true
		}
	}
	bySection := map[string]map[string]string{} // section -> token -> node name
	seen := map[string]bool{}
	var prefixes []string
	for _, t := range tokens {
		if t.prefix != "" {
			prefixes = append(prefixes, t.prefix)
			continue
		}
		node := tailwindTokenRoot + t.section + "." + t.name
		if bySection[t.section] == nil {
			bySection[t.section] = map[string]string{}
		}
		bySection[t.section][t.name] = node
		key := node + "\x00" + t.file
		if seen[key] {
			continue
		}
		seen[key] = true
		all = append(all, facts.Fact{Kind: facts.KindSymbol, Name: node, File: t.file, Line: t.line, Props: map[string]any{
			"symbol_kind": facts.SymbolConstant, "language": "typescript", propToken: t.name,
			propTokenSection: t.section, propTokenSource: tokenSourceTW, "value": t.value,
		}, Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: factpath.Dir(t.file)}}})
	}
	byPrefix := map[string][]string{} // utility prefix -> sections
	for section, prefixes := range tailwindSections {
		if bySection[section] == nil {
			continue
		}
		for _, p := range prefixes {
			byPrefix[p] = append(byPrefix[p], section)
		}
	}
	components := map[string][]int{}
	fileRefs := map[string]int{}
	templates := map[string]int{} // an Angular component's templateUrl -> the component
	for i, f := range all {
		switch {
		case f.Kind == facts.KindFileRef:
			fileRefs[f.File] = i
		case f.Kind == facts.KindSymbol && f.Props["web_component"] == "component":
			components[f.File] = append(components[f.File], i)
		}
		if url, _ := f.Props["angular_template_url"].(string); url != "" && f.Kind == facts.KindSymbol {
			templates[url] = i
		}
	}
	// HTML is read only from disk: an Angular template is where most of an Angular
	// application's classes are, and it belongs to the component that names it.
	scan := append([]string(nil), tsFiles...)
	html := map[string][]byte{}
	for _, f := range files {
		if f = filepath.ToSlash(f); isAngularTemplateFile(f) && !facts.IsTestPath(f) && usesTailwind(f) {
			if src, err := os.ReadFile(filepath.Join(repoPath, f)); err == nil { //factpath:host
				html[f] = src
				scan = append(scan, f)
			}
		}
	}
	for _, file := range scan {
		// A theme is no user of its own tokens, and code outside a Tailwind project
		// (the framework's own compiler, a codemod's data) names classes it does not
		// style anything with.
		if facts.IsTestPath(file) || tokenFiles[file] || tailwindConfigRe.MatchString(file) || !usesTailwind(file) {
			continue
		}
		src, ok := html[file]
		if !ok {
			src = sources[file]
		}
		if len(src) == 0 {
			continue
		}
		text := string(src)
		if !isAngularTemplateFile(file) {
			text = string(blankTSComments(src, file))
		}
		used := tailwindClassTokens(text, prefixes, byPrefix, bySection)
		if len(used) == 0 {
			continue
		}
		var idx int
		if c, ok := templates[file]; ok {
			idx = c
		} else if c := components[file]; len(c) == 1 {
			idx = c[0]
		} else if i, ok := fileRefs[file]; ok {
			idx = i
		} else {
			all = append(all, facts.Fact{Kind: facts.KindFileRef, Name: file, File: file, Line: 1, Props: map[string]any{"language": "typescript"}})
			idx = len(all) - 1
			fileRefs[file] = idx
		}
		for _, node := range used {
			if !all[idx].HasRelation(facts.RelNames, node) {
				all[idx].Relations = append(all[idx].Relations, facts.Relation{Kind: facts.RelNames, Target: node})
			}
		}
	}
	return all
}

// tailwindClassTokens returns the token nodes the class names in a file's string
// literals use, sorted.
func tailwindClassTokens(src string, prefixes []string, byPrefix map[string][]string, bySection map[string]map[string]string) []string {
	used := map[string]bool{}
	for _, m := range classStringRe.FindAllStringSubmatch(src, -1) {
		s := m[1] + m[2] + m[3]
		if !strings.Contains(s, "-") {
			continue
		}
		for _, cls := range strings.Fields(s) {
			if node := tailwindClassToken(cls, prefixes, byPrefix, bySection); node != "" {
				used[node] = true
			}
		}
	}
	out := make([]string, 0, len(used))
	for n := range used {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// tailwindClassToken matches one class against the declared tokens: variants
// (`hover:`, `md:`, `data-[x]:`) and `!` / `-` are stripped, then the longest
// utility prefix is tried with the remainder as the token, an opacity modifier
// (`/50`) dropped.
func tailwindClassToken(cls string, prefixes []string, byPrefix map[string][]string, bySection map[string]map[string]string) string {
	depth := 0
	cut := -1
	for i, ch := range cls {
		switch ch {
		case '[':
			depth++
		case ']':
			depth--
		case ':':
			if depth == 0 {
				cut = i
			}
		}
	}
	cls = strings.TrimPrefix(cls[cut+1:], "!")
	if strings.HasPrefix(cls, "--") {
		return "" // a CSS custom property (`--font-sans`), not a class
	}
	cls = strings.TrimPrefix(cls, "-") // a negative utility, -m-4
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(cls, p) {
			cls = strings.TrimPrefix(strings.TrimPrefix(cls, p), "-")
			break
		}
	}
	if i := strings.IndexByte(cls, '/'); i > 0 {
		cls = cls[:i]
	}
	best, bestNode := 0, ""
	for i := len(cls) - 1; i > 0; i-- {
		if cls[i] != '-' {
			continue
		}
		prefix, rest := cls[:i], cls[i+1:]
		for _, section := range byPrefix[prefix] {
			if node := bySection[section][rest]; node != "" && len(prefix) > best {
				best, bestNode = len(prefix), node
			}
		}
	}
	return bestNode
}

// tailwindProjects reports whether a file belongs to a project that styles with
// Tailwind: walking up from the file, a tailwind.config.* before the nearest
// package.json, or that package.json depending on tailwindcss. The walk stops at the
// package boundary, so a monorepo root depending on Tailwind does not claim every
// package under it.
func tailwindProjects(repoPath string, files []string) func(string) bool {
	configs := map[string]bool{}
	manifests := map[string]bool{}
	for _, f := range files {
		f = filepath.ToSlash(f)
		switch {
		case tailwindConfigRe.MatchString(f):
			configs[factpath.Dir(f)] = true
		case path.Base(f) == "package.json":
			manifests[factpath.Dir(f)] = true
		}
	}
	cache := map[string]bool{}
	var dirUses func(dir string) bool
	dirUses = func(dir string) bool {
		if v, ok := cache[dir]; ok {
			return v
		}
		var v bool
		switch {
		case configs[dir]:
			v = true
		case manifests[dir]:
			v = manifestDependsOn(filepath.Join(repoPath, filepath.FromSlash(dir), "package.json"), "tailwindcss")
		case dir == "." || dir == "":
			v = false
		default:
			v = dirUses(factpath.Dir(dir))
		}
		cache[dir] = v
		return v
	}
	return func(file string) bool { return dirUses(factpath.Dir(filepath.ToSlash(file))) }
}

func manifestDependsOn(manifest, dep string) bool {
	raw, err := os.ReadFile(manifest) //factpath:host
	if err != nil {
		return false
	}
	var pkg struct {
		Dependencies, DevDependencies, PeerDependencies map[string]string
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return false
	}
	_, a := pkg.Dependencies[dep]
	_, b := pkg.DevDependencies[dep]
	_, c := pkg.PeerDependencies[dep]
	return a || b || c
}

// importsTailwind reports whether a module imports from tailwindcss: a preset, a
// plugin, or a config typed against it.
func importsTailwind(src string) bool {
	return strings.Contains(src, `"tailwindcss`) || strings.Contains(src, `'tailwindcss`)
}
