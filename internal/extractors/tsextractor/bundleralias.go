package tsextractor

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
)

// How an alias came to be known, recorded on the import it resolves as resolved_via.
const (
	aliasViaTSConfig = "tsconfig-paths"
	aliasViaVite     = "vite-alias"
	aliasViaNuxt     = "nuxt-alias"
)

// Why an import that matches an alias is not resolved, recorded as unresolved_reason.
const (
	aliasUnresolvedCustomResolver = "custom-resolver"
	aliasUnresolvedDynamic        = "dynamic-alias"
)

var (
	viteConfigNames = []string{"vite.config.ts", "vite.config.mts", "vite.config.js", "vite.config.mjs", "vite.config.cts", "vite.config.cjs"}
	nuxtConfigNames = []string{"nuxt.config.ts", "nuxt.config.js", "nuxt.config.mjs"}

	nuxtSrcDirRe      = regexp.MustCompile(`(?m)^\s*srcDir:\s*['"]([^'"]+)['"]`)
	nuxtCompat4Re     = regexp.MustCompile(`compatibilityVersion:\s*4`)
	jsRegexGroupRefRe = regexp.MustCompile(`\$(\d)`)
)

// bundlerAliasesAtDir returns the aliases the bundler configured in dir applies:
// Vite's resolve.alias, and Nuxt's built-in and configured aliases. The bundler is
// what resolves the import when the code runs, so these win over tsconfig paths.
// Replacements are repository-relative, unlike tsconfig ones, which walkTSAliasRoots
// qualifies with the root's directory.
func bundlerAliasesAtDir(repoPath, dir string) map[string]tsAlias {
	rel, err := filepath.Rel(repoPath, dir)
	if err != nil {
		return nil
	}
	rel = factpath.Slash(rel)
	if rel == "." {
		rel = ""
	}
	out := map[string]tsAlias{}
	for _, name := range nuxtConfigNames {
		if src, err := os.ReadFile(filepath.Join(dir, name)); err == nil { //factpath:host
			addNuxtBuiltinAliases(out, dir, rel, src)
			readConfigAliases(out, src, rel, aliasViaNuxt, true)
			break
		}
	}
	for _, name := range viteConfigNames {
		if src, err := os.ReadFile(filepath.Join(dir, name)); err == nil { //factpath:host
			readConfigAliases(out, src, rel, aliasViaVite, false)
			break
		}
	}
	return out
}

// addNuxtBuiltinAliases adds what Nuxt aliases without configuration: ~ and @ for
// the source directory, ~~ and @@ for the root, #shared for <root>/shared. Nuxt 4
// moved the source directory to app/.
func addNuxtBuiltinAliases(out map[string]tsAlias, dir, rel string, src []byte) {
	srcDir := rel
	if m := nuxtSrcDirRe.FindSubmatch(src); m != nil {
		srcDir = joinRel(rel, string(m[1]))
	} else if st, err := os.Stat(filepath.Join(dir, "app")); err == nil && st.IsDir() && nuxtMajorAtLeast4(dir, src) { //factpath:host
		srcDir = joinRel(rel, "app")
	}
	for _, key := range []string{"~", "@"} {
		addStringAlias(out, key, tsAlias{replacement: srcDir, via: aliasViaNuxt})
	}
	for _, key := range []string{"~~", "@@"} {
		addStringAlias(out, key, tsAlias{replacement: rel, via: aliasViaNuxt})
	}
	addStringAlias(out, "#shared", tsAlias{replacement: joinRel(rel, "shared"), via: aliasViaNuxt})
}

func nuxtMajorAtLeast4(dir string, config []byte) bool {
	if nuxtCompat4Re.Match(config) {
		return true
	}
	raw, err := os.ReadFile(filepath.Join(dir, "package.json")) //factpath:host
	if err != nil {
		return false
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return false
	}
	v := pkg.Dependencies["nuxt"]
	if v == "" {
		v = pkg.DevDependencies["nuxt"]
	}
	v = strings.TrimLeft(v, "^~>=v ")
	return v != "" && v[0] >= '4' && v[0] <= '9'
}

func joinRel(rel, p string) string {
	return factpath.Clean(path.Join(rel, p))
}

// addStringAlias adds a string `find` the way Vite matches one: the specifier
// itself, or the specifier followed by a slash.
func addStringAlias(out map[string]tsAlias, find string, a tsAlias) {
	exact := a
	exact.exact = true
	out[find] = exact
	prefix := a
	if prefix.unresolved == "" && a.replacement != "" {
		// "" is the repository root, where `~~/config` is plain `config`.
		prefix.replacement = strings.TrimSuffix(a.replacement, "/") + "/"
	}
	out[strings.TrimSuffix(find, "/")+"/"] = prefix
}

// readConfigAliases reads every `alias` a config declares where the bundler applies
// it to application code: under `resolve` (Vite, or Nuxt's `vite:` block), and for
// Nuxt at the top level of defineNuxtConfig. `nitro.alias` and `test.alias` apply to
// other code and are left out.
func readConfigAliases(out map[string]tsAlias, src []byte, rel, via string, topLevel bool) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(typescript.LanguageTypescript())); err != nil {
		return
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()
	r := &configReader{kinds: tsKindsFor(false), src: src, rel: rel, via: via}
	r.readConsts(tree.RootNode())
	order := 0
	var walk func(n *sitter.Node, parentKey string, depth int)
	walk = func(n *sitter.Node, parentKey string, depth int) {
		if n == nil {
			return
		}
		if kindOf(r.kinds, n) == "pair" {
			key := r.keyOf(n)
			val := n.ChildByFieldName("value")
			if key == "alias" && (parentKey == "resolve" || (topLevel && parentKey == "" && depth <= 1)) {
				r.readAliasValue(out, val, &order)
				return
			}
			walk(val, key, depth+1)
			return
		}
		for i := range n.ChildCount() {
			walk(n.Child(i), parentKey, depth)
		}
	}
	walk(tree.RootNode(), "", 0)
}

type configReader struct {
	kinds  *tsutil.KindTable
	src    []byte
	rel    string // the config's directory, repository-relative
	via    string
	consts map[string]*sitter.Node // module-scope const name -> its value
}

// readConsts indexes the config's module-scope `const x = ...` bindings, so a path
// built once (const root = path.resolve(__dirname, 'src')) evaluates where it is used.
func (r *configReader) readConsts(root *sitter.Node) {
	r.consts = map[string]*sitter.Node{}
	for i := range root.NamedChildCount() {
		stmt := root.NamedChild(i)
		if kindOf(r.kinds, stmt) == "export_statement" {
			stmt = stmt.ChildByFieldName("declaration")
		}
		if stmt == nil || kindOf(r.kinds, stmt) != "lexical_declaration" || !strings.HasPrefix(nodeText(stmt, r.src), "const") {
			continue
		}
		for j := range stmt.NamedChildCount() {
			d := stmt.NamedChild(j)
			name, val := d.ChildByFieldName("name"), d.ChildByFieldName("value")
			if kindOf(r.kinds, d) == "variable_declarator" && name != nil && val != nil && kindOf(r.kinds, name) == "identifier" {
				r.consts[nodeText(name, r.src)] = val
			}
		}
	}
}

func (r *configReader) keyOf(pair *sitter.Node) string {
	k := pair.ChildByFieldName("key")
	if k == nil {
		return ""
	}
	return strings.Trim(nodeText(k, r.src), `"'`+"`")
}

func (r *configReader) readAliasValue(out map[string]tsAlias, val *sitter.Node, order *int) {
	switch kindOf(r.kinds, val) {
	case "object":
		for i := range val.NamedChildCount() {
			pair := val.NamedChild(i)
			if kindOf(r.kinds, pair) != "pair" {
				continue
			}
			addStringAlias(out, r.keyOf(pair), r.target(pair.ChildByFieldName("value")))
		}
	case "array":
		for i := range val.NamedChildCount() {
			entry := val.NamedChild(i)
			if kindOf(r.kinds, entry) != "object" {
				continue
			}
			var find, replacement *sitter.Node
			custom := false
			for j := range entry.NamedChildCount() {
				p := entry.NamedChild(j)
				if kindOf(r.kinds, p) != "pair" && kindOf(r.kinds, p) != "method_definition" {
					continue
				}
				switch name := r.keyOf(p); {
				case name == "find":
					find = p.ChildByFieldName("value")
				case name == "replacement":
					replacement = p.ChildByFieldName("value")
				case name == "customResolver" || kindOf(r.kinds, p) == "method_definition" && strings.HasPrefix(nodeText(p, r.src), "customResolver"):
					custom = true
				}
			}
			if find == nil {
				continue
			}
			a := r.target(replacement)
			if custom {
				a = tsAlias{unresolved: aliasUnresolvedCustomResolver, via: r.via}
			}
			*order++
			a.order = *order
			switch kindOf(r.kinds, find) {
			case "string":
				addStringAlias(out, strings.Trim(nodeText(find, r.src), `"'`), a)
			case "regex":
				r.addRegexAlias(out, find, a)
			}
		}
	}
}

// addRegexAlias keeps a regex find whose pattern Go's regexp reads the same way;
// $1 in the replacement becomes the first group.
func (r *configReader) addRegexAlias(out map[string]tsAlias, find *sitter.Node, a tsAlias) {
	pat := find.ChildByFieldName("pattern")
	if pat == nil {
		return
	}
	re, err := regexp.Compile(nodeText(pat, r.src))
	if err != nil {
		return
	}
	a.re = re
	a.replacement = jsRegexGroupRefRe.ReplaceAllString(a.replacement, "$${$1}")
	out["re:"+re.String()] = a
}

// target evaluates an alias replacement to a repository path, a package name, or
// nothing it can state.
func (r *configReader) target(n *sitter.Node) tsAlias {
	if n == nil {
		return tsAlias{unresolved: aliasUnresolvedDynamic, via: r.via}
	}
	if v, ok := r.pathOf(n); ok {
		return tsAlias{replacement: v, via: r.via}
	}
	if s, ok := r.literal(n); ok {
		switch {
		case strings.HasPrefix(s, "/"):
			return tsAlias{replacement: joinRel(r.rel, s), via: r.via}
		case strings.HasPrefix(s, "."):
			return tsAlias{replacement: joinRel(r.rel, s), via: r.via}
		default:
			return tsAlias{replacement: s, via: r.via, external: true}
		}
	}
	if pkg, ok := r.requireResolve(n); ok {
		return tsAlias{replacement: pkg, via: r.via, external: true}
	}
	return tsAlias{unresolved: aliasUnresolvedDynamic, via: r.via}
}

func (r *configReader) literal(n *sitter.Node) (string, bool) {
	switch kindOf(r.kinds, n) {
	case "string":
		return strings.Trim(nodeText(n, r.src), `"'`), true
	case "template_string":
		t := nodeText(n, r.src)
		if !strings.Contains(t, "${") {
			return strings.Trim(t, "`"), true
		}
	}
	return "", false
}

// pathOf evaluates the path expressions configs use: __dirname, import.meta.dirname,
// process.cwd(), path.resolve/join(...), fileURLToPath(new URL('./x', import.meta.url)),
// and a template string built from them. Every result stays inside the repository.
func (r *configReader) pathOf(n *sitter.Node) (string, bool) {
	text := nodeText(n, r.src)
	switch kindOf(r.kinds, n) {
	case "identifier":
		if text == "__dirname" {
			return r.rel, true
		}
		if v, ok := r.consts[text]; ok {
			// Unbound while it is read, so a const defined through itself ends.
			delete(r.consts, text)
			p, ok := r.pathOf(v)
			r.consts[text] = v
			return p, ok
		}
	case "member_expression":
		if text == "import.meta.dirname" {
			return r.rel, true
		}
		if prop := n.ChildByFieldName("property"); prop != nil && nodeText(prop, r.src) == "pathname" {
			return r.pathOf(n.ChildByFieldName("object"))
		}
	case "new_expression":
		if c := n.ChildByFieldName("constructor"); c != nil && nodeText(c, r.src) == "URL" {
			args := namedArgs(r.kinds, n.ChildByFieldName("arguments"))
			if len(args) == 2 && nodeText(args[1], r.src) == "import.meta.url" {
				if s, ok := r.literal(args[0]); ok {
					return r.inRepo(path.Join(r.rel, s))
				}
			}
		}
	case "template_string":
		var b strings.Builder
		for i := range n.NamedChildCount() {
			c := n.NamedChild(i)
			switch kindOf(r.kinds, c) {
			case "template_substitution":
				v, ok := r.pathOf(c.NamedChild(0))
				if !ok {
					return "", false
				}
				b.WriteString(v)
			case "string_fragment":
				b.WriteString(nodeText(c, r.src))
			}
		}
		if b.Len() == 0 {
			return "", false
		}
		return b.String(), true
	case "call_expression":
		fn := n.ChildByFieldName("function")
		name := nodeText(fn, r.src)
		if i := strings.LastIndexByte(name, '.'); i >= 0 {
			name = name[i+1:]
		}
		args := namedArgs(r.kinds, n.ChildByFieldName("arguments"))
		switch name {
		case "cwd":
			if nodeText(fn, r.src) == "process.cwd" {
				return r.rel, true
			}
		case "fileURLToPath":
			if len(args) == 1 {
				return r.pathOf(args[0])
			}
		case "resolve", "join":
			if nodeText(fn, r.src) == "require.resolve" {
				return "", false
			}
			acc, trailing := r.rel, false
			for _, a := range args {
				if v, ok := r.pathOf(a); ok {
					acc, trailing = v, false
					continue
				}
				s, ok := r.literal(a)
				if !ok || strings.HasPrefix(s, "/") {
					return "", false
				}
				acc, trailing = path.Join(acc, s), strings.HasSuffix(s, "/")
			}
			p, ok := r.inRepo(acc)
			// Node's path.join keeps a trailing slash and path.resolve drops it; a
			// regex alias appends its capture straight after the replacement.
			if ok && trailing && name == "join" && p != "" {
				p += "/"
			}
			return p, ok
		}
	}
	return "", false
}

func (r *configReader) requireResolve(n *sitter.Node) (string, bool) {
	if kindOf(r.kinds, n) != "call_expression" || nodeText(n.ChildByFieldName("function"), r.src) != "require.resolve" {
		return "", false
	}
	args := namedArgs(r.kinds, n.ChildByFieldName("arguments"))
	if len(args) != 1 {
		return "", false
	}
	s, ok := r.literal(args[0])
	if !ok || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "/") {
		return "", false
	}
	return s, true
}

func (r *configReader) inRepo(p string) (string, bool) {
	p = factpath.Clean(p)
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	if p == "." {
		p = ""
	}
	return p, true
}

func namedArgs(kinds *tsutil.KindTable, args *sitter.Node) []*sitter.Node {
	if args == nil {
		return nil
	}
	out := make([]*sitter.Node, 0, args.NamedChildCount())
	for i := range args.NamedChildCount() {
		if c := args.NamedChild(i); kindOf(kinds, c) != "comment" {
			out = append(out, c)
		}
	}
	return out
}
