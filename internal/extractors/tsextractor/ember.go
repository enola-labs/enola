package tsextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/litfold"
)

// Ember/Glimmer support — the third framework dialect the TypeScript extractor
// handles beside Vue and Svelte.
//
// Ember is the inverse slicing problem: where a Vue SFC is markup with embedded
// <script> blocks, a Glimmer template-tag file (.gts/.gjs) is TypeScript/JavaScript
// with embedded <template> blocks. Rather than adjusting line offsets the way the
// Vue path must, the template spans are blanked in place — every byte except
// newlines replaced with spaces — so the remainder parses with the standard grammar
// and every fact's Line is true to the original file by construction.
//
// Three resolution regimes, in decreasing order of certainty:
//
//   - .gts/.gjs templates are strict-mode: anything a template renders must be in
//     scope, which for components/helpers means imported. Template identifier
//     tokens are matched against the file's own import bindings; a token that is
//     not an import is a local (an @arg, a block param, `this.*`) and produces no
//     edge.
//   - `@service` fields name a service by convention. The extractor records the
//     names; the ember-resolver binder resolves them against the store, where the
//     actual declared class symbol is known.
//   - .hbs invocations are bare strings with no import to anchor on. The extractor
//     records them on a file_ref carrier; the binder resolves via Ember's default
//     resolver layout (components/, helpers/) and skips anything ambiguous —
//     a missing edge beats a wrong one.

// EmberFramework is the framework prop value stamped on Ember facts.
const EmberFramework = "ember"

// emberTemplateOpen/Close delimit a Glimmer template block. Glimmer requires the
// lowercase form; anything else is ordinary markup in a string and is not sliced.
const (
	emberTemplateOpen  = "<template>"
	emberTemplateClose = "</template>"
)

func isEmberTemplateTagFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".gts" || ext == ".gjs"
}

func isHbsFile(path string) bool {
	return strings.ToLower(filepath.Ext(path)) == ".hbs"
}

// detectEmber reports whether this repository contains an Ember application.
//
// It used to check only the TypeScript root and the repository root, which
// misses the shape this estate actually has: an Ember application nested inside
// a backend repository. One monolith's `ember_app/package.json` declares
// ember-source and the repository root's does not — and because the root has a
// package.json at all, findTSRoot returns the root and the fallback branch
// never fires. The flag stayed false, and everything gated on it produced
// nothing: 510 router declarations, 373 ember-data models and 1,363 classic
// templates, all silently absent while 1,866 components extracted fine, because
// a .gts file importing @glimmer/component needs no flag to be recognisable.
//
// The nested search is bounded and stops at the directories a JavaScript
// project never keeps source in. A repository is Ember if any package.json it
// contains says so.
func detectEmber(repoPath string) bool {
	tsRoot, _ := findTSRoot(repoPath)
	if hasPkgDependency(tsRoot, "ember-source") ||
		(tsRoot != repoPath && hasPkgDependency(repoPath, "ember-source")) {
		return true
	}
	return nestedPkgDeclares(repoPath, "ember-source", emberSearchDepth)
}

// emberSearchDepth is how deep a nested application may sit. Two levels covers
// `ember_app/` and `frontend/app/`; deeper than that and the directory is not
// the repository's frontend, it is a fixture or a vendored copy.
const emberSearchDepth = 2

// nestedPkgDeclares reports whether any package.json within depth levels of root
// declares the dependency.
func nestedPkgDeclares(root, dependency string, depth int) bool {
	if depth <= 0 {
		return false
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() || skipForPkgSearch(entry.Name()) {
			continue
		}
		child := filepath.Join(root, entry.Name())
		if hasPkgDependency(child, dependency) {
			return true
		}
		if nestedPkgDeclares(child, dependency, depth-1) {
			return true
		}
	}
	return false
}

// skipForPkgSearch names the directories a JavaScript project never keeps its
// own manifest in, so the walk does not wander into dependencies or fixtures.
func skipForPkgSearch(name string) bool {
	switch name {
	case "node_modules", "vendor", "tmp", "dist", "build", "coverage", "spec", "test", "tests", "fixtures":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// emberTemplateSegment is one <template> block sliced out of a .gts/.gjs file.
// Start is the byte offset of its opening tag in the original source, used to
// associate the segment with the class or binding that owns it.
type emberTemplateSegment struct {
	Content string
	Start   int
}

// blankEmberTemplates replaces every <template>…</template> span (tags included)
// so the remainder parses with the standard grammar, preserving newlines so
// tree-sitter positions match the original file exactly. The replacement depends
// on syntactic position, because the RFC allows a template in two kinds of place:
// as a statement (top-level standalone, or inside a class body), where blank
// space is valid, and as an EXPRESSION (`const Greet = <template>…`,
// `export default <template>…`), where blank space would leave a dangling `=` —
// there the span becomes a backtick template literal of the same length, which
// is a well-formed expression that may span lines. An unclosed <template> is
// left untouched: half a blank would corrupt the parse worse than an unparsed
// template block.
func blankEmberTemplates(src []byte) ([]byte, []emberTemplateSegment) {
	var segments []emberTemplateSegment
	out := make([]byte, len(src))
	copy(out, src)
	pos := 0
	for {
		idx := strings.Index(string(out[pos:]), emberTemplateOpen)
		if idx < 0 {
			break
		}
		start := pos + idx
		closeIdx := strings.Index(string(out[start:]), emberTemplateClose)
		if closeIdx < 0 {
			break
		}
		end := start + closeIdx + len(emberTemplateClose)
		inner := string(src[start+len(emberTemplateOpen) : start+closeIdx])
		segments = append(segments, emberTemplateSegment{Content: inner, Start: start})
		for i := start; i < end; i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
		if emberExpressionPosition(out, start) {
			out[start] = '`'
			out[end-1] = '`'
		}
		pos = end
	}
	return out, segments
}

// emberExpressionPosition reports whether the template at start sits where the
// grammar needs an expression: after an assignment, an opening delimiter, a
// `return`, an `export default`, or an arrow. Everything else — a top-level
// standalone template, a class-body template — is a statement position where
// blank space parses.
func emberExpressionPosition(src []byte, start int) bool {
	i := start - 1
	for i >= 0 && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
		i--
	}
	if i < 0 {
		return false
	}
	switch src[i] {
	case '=', '(', ',', ':', '[', '?':
		return true
	case '>':
		return i > 0 && src[i-1] == '='
	}
	wordEnd := i + 1
	for i >= 0 && ((src[i] >= 'a' && src[i] <= 'z') || (src[i] >= 'A' && src[i] <= 'Z')) {
		i--
	}
	switch string(src[i+1 : wordEnd]) {
	case "return", "default":
		return true
	}
	return false
}

// emberImportBindings maps each locally-bound import name to its resolution:
// internal imports to a canonical symbol name ("<moduleDir>.<localName>"),
// external imports to their module specifier. Default and named imports both
// bind — Ember components are overwhelmingly default exports, which is exactly
// the case buildImportSymbols skips for the general call-resolution path.
//
// modules is the third reading of the same statement, and the one a consumer
// outside Ember asks for: the module a name came from, whichever kind of import
// bound it — the specifier for a package, the repo-relative path for a file.
// That is the string this file's own dependency fact already carries as its
// imports target, so a name's origin and the module the file declares it imports
// are the same value rather than two spellings of it.
type emberImportBindings struct {
	internal map[string]string
	external map[string]string
	modules  map[string]string
}

func buildEmberImportBindings(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string, aliases map[string]tsAlias) emberImportBindings {
	b := emberImportBindings{
		internal: make(map[string]string),
		external: make(map[string]string),
		modules:  make(map[string]string),
	}
	fileDir := factpath.Dir(relFile)
	for i := range root.ChildCount() {
		child := root.Child(i)
		if kindOf(kinds, child) != "import_statement" {
			continue
		}
		source := findChildByKind(kinds, child, "string")
		if source == nil {
			continue
		}
		importPath := strings.Trim(nodeText(source, src), `"'`)
		resolved, isExternal := resolveImportPath(importPath, fileDir, aliases)
		moduleDir := factpath.Dir(resolved)

		clause := findChildByKind(kinds, child, "import_clause")
		if clause == nil {
			continue
		}
		bind := func(local string) {
			if local == "" {
				return
			}
			b.modules[local] = resolved
			if isExternal {
				b.external[local] = importPath
			} else {
				b.internal[local] = moduleDir + "." + local
			}
		}
		for j := range clause.ChildCount() {
			c := clause.Child(j)
			switch kindOf(kinds, c) {
			case "identifier":
				bind(nodeText(c, src))
			case "named_imports":
				for k := range c.ChildCount() {
					spec := c.Child(k)
					if kindOf(kinds, spec) != "import_specifier" {
						continue
					}
					nameNode := spec.ChildByFieldName("name")
					if nameNode == nil {
						continue
					}
					local := nodeText(nameNode, src)
					if a := spec.ChildByFieldName("alias"); a != nil {
						local = nodeText(a, src)
					}
					bind(local)
				}
			}
		}
	}
	return b
}

// emberTemplateRefs returns the canonical targets of every identifier token in the
// template segments that matches an internal import binding, sorted and deduplicated.
// The strict-mode invariant does the filtering: locals, @args, block params and
// built-in keywords are simply never in the binding map.
func emberTemplateRefs(segments []emberTemplateSegment, bindings emberImportBindings) []string {
	seen := make(map[string]bool)
	for _, seg := range segments {
		for _, tok := range identTokens(seg.Content) {
			if target, ok := bindings.internal[tok]; ok {
				seen[target] = true
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	targets := make([]string, 0, len(seen))
	for t := range seen {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	return targets
}

// identTokens splits text into identifier-shaped tokens (letters, digits, _, $).
func identTokens(text string) []string {
	var tokens []string
	start := -1
	isIdent := func(r byte) bool {
		return r == '_' || r == '$' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
	}
	for i := 0; i <= len(text); i++ {
		if i < len(text) && isIdent(text[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			tokens = append(tokens, text[start:i])
			start = -1
		}
	}
	return tokens
}

// dasherize converts CamelCase/PascalCase to kebab-case ("AcmeApollo" →
// "acme-apollo"), matching Ember's name convention for services and models.
func dasherize(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// emberModuleBindings are the external modules whose default exports mark a class
// as an Ember construct when extended.
var (
	emberComponentModules = map[string]bool{"@glimmer/component": true, "@ember/component": true}
	emberModelModules     = map[string]bool{"@ember-data/model": true, "ember-data": true}
	emberServiceModules   = map[string]bool{"@ember/service": true}
)

// emberClassInfo describes one top-level class declaration and the Ember role its
// heritage and decorators imply. start/end delimit the class node's byte span, so
// a template segment can be attributed to the class that embeds it.
type emberClassInfo struct {
	name           string
	line           int
	start, end     int
	isDefault      bool
	isComponent    bool
	isModel        bool
	isService      bool
	services       []string
	relationships  []string
	attrTransforms []string
	codeLinks      []string
}

// emberBindingInfo describes a top-level `const Name = <template>…` binding — the
// RFC's named-component form. start/end delimit the declarator's value span.
type emberBindingInfo struct {
	name       string
	start, end int
}

// collectEmberClasses walks the top-level declarations (unwrapping export
// statements) and classifies each class by what its superclass's local name was
// imported from, plus the service names its @service-decorated fields inject and
// the ember-data relationships its @belongsTo/@hasMany fields declare.
func collectEmberClasses(kinds *tsutil.KindTable, root *sitter.Node, src []byte, bindings emberImportBindings, folds *litfold.Assignments) []emberClassInfo {
	serviceDecorators := emberServiceDecoratorNames(bindings)
	relationshipDecorators := emberRelationshipDecoratorNames(bindings)
	attrNames := make(map[string]bool)
	for local, mod := range bindings.external {
		if emberModelModules[mod] && local == "attr" {
			attrNames[local] = true
		}
	}
	var classes []emberClassInfo
	defaultName := emberDefaultExportName(kinds, root, src)
	for i := range root.ChildCount() {
		node := root.Child(i)
		isDefault := false
		if kindOf(kinds, node) == "export_statement" {
			isDefault = hasChildKind(kinds, node, "default")
			if decl := firstDeclChild(kinds, node); decl != nil {
				node = decl
			} else if c := findChildByKind(kinds, node, "class"); c != nil {
				node = c
			}
		}
		switch kindOf(kinds, node) {
		case "class_declaration", "abstract_class_declaration", "class":
		default:
			continue
		}
		info := emberClassInfo{
			line:      int(node.StartPosition().Row) + 1,
			start:     int(node.StartByte()),
			end:       int(node.EndByte()),
			isDefault: isDefault,
		}
		if name := findChildByKind(kinds, node, "type_identifier"); name != nil {
			info.name = nodeText(name, src)
		}
		if super := tsSuperclassName(kinds, node, src); super != "" {
			if mod, ok := bindings.external[super]; ok {
				info.isComponent = emberComponentModules[mod]
				info.isModel = emberModelModules[mod]
				info.isService = emberServiceModules[mod]
			}
		}
		if body := findChildByKind(kinds, node, "class_body"); body != nil {
			info.services = emberInjectedServices(kinds, body, src, serviceDecorators)
			info.services = mergeSorted(info.services, emberLookupServices(kinds, body, src, folds))
			info.codeLinks = emberTransitionLinks(kinds, body, src, folds)
			if info.isModel {
				info.relationships = emberModelRelationships(kinds, body, src, relationshipDecorators)
				info.attrTransforms = emberModelAttrTransforms(kinds, body, src, attrNames)
			}
		}
		if info.name != "" && info.name == defaultName {
			info.isDefault = true
		}
		classes = append(classes, info)
	}
	return classes
}

// emberTransitionLinks collects the literal route names a class navigates to in
// CODE — `this.router.transitionTo('jobs.job', …)` and `replaceWith` — the
// programmatic counterpart of a template's `<LinkTo @route=…>`. Only a literal
// first argument that looks like a route name (no leading slash: a URL form is
// a path, not a name) produces a candidate; a computed name produces nothing.
func emberTransitionLinks(kinds *tsutil.KindTable, classBody *sitter.Node, src []byte, folds *litfold.Assignments) []string {
	seen := make(map[string]bool)
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if kindOf(kinds, n) == "call_expression" {
			if fn := n.ChildByFieldName("function"); fn != nil && kindOf(kinds, fn) == "member_expression" {
				if prop := fn.ChildByFieldName("property"); prop != nil {
					switch nodeText(prop, src) {
					case "transitionTo", "replaceWith":
						if args := n.ChildByFieldName("arguments"); args != nil {
							name := ""
							if s := findChildByKind(kinds, args, "string"); s != nil {
								name = strings.Trim(nodeText(s, src), `"'`)
							} else if id := findChildByKind(kinds, args, "identifier"); id != nil {
								name, _ = folds.Resolve(nodeText(id, src))
							}
							if name != "" && !strings.HasPrefix(name, "/") &&
								!strings.ContainsAny(name, " {}") {
								seen[name] = true
							}
						}
					}
				}
			}
		}
		for i := range n.ChildCount() {
			walk(n.Child(i))
		}
	}
	walk(classBody)
	if len(seen) == 0 {
		return nil
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// emberLookupServices collects the literal service names a class resolves
// through the container — `owner.lookup('service:current')` — the string-form
// counterpart of an @service field. Only the `service:` type is read: it is
// effectively all real-world usage, and each container type would need its own
// resolution rule.
func emberLookupServices(kinds *tsutil.KindTable, classBody *sitter.Node, src []byte, folds *litfold.Assignments) []string {
	seen := make(map[string]bool)
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if kindOf(kinds, n) == "call_expression" {
			if fn := n.ChildByFieldName("function"); fn != nil && kindOf(kinds, fn) == "member_expression" {
				if prop := fn.ChildByFieldName("property"); prop != nil && nodeText(prop, src) == "lookup" {
					if args := n.ChildByFieldName("arguments"); args != nil {
						arg := ""
						if s := findChildByKind(kinds, args, "string"); s != nil {
							arg = strings.Trim(nodeText(s, src), `"'`)
						} else if id := findChildByKind(kinds, args, "identifier"); id != nil {
							arg, _ = folds.Resolve(nodeText(id, src))
						}
						if name, ok := strings.CutPrefix(arg, "service:"); ok && name != "" {
							seen[name] = true
						}
					}
				}
			}
		}
		for i := range n.ChildCount() {
			walk(n.Child(i))
		}
	}
	walk(classBody)
	if len(seen) == 0 {
		return nil
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func mergeSorted(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		seen[s] = true
	}
	merged := make([]string, 0, len(seen))
	for s := range seen {
		merged = append(merged, s)
	}
	sort.Strings(merged)
	return merged
}

// EmberYieldHashProp carries a component's literal yield-hash entries
// ("Key=name"); EmberContextualProp the "<component>#<Key>" consumption pairs;
// EmberAttrTransformsProp a model's @attr type names; the dynamic props count
// irreducibly runtime resolution sites with capped samples — visibility, never
// speculation.
const (
	EmberYieldHashProp      = "ember_yield_hash"
	EmberContextualProp     = "ember_contextual"
	EmberAttrTransformsProp = "ember_attr_transforms"
	EmberDynamicCountProp   = "ember_dynamic_count"
	EmberDynamicSamplesProp = "ember_dynamic_samples"
)

func propStringsLocal(v any) []string {
	switch vv := v.(type) {
	case []string:
		return vv
	case []any:
		out := make([]string, 0, len(vv))
		for _, x := range vv {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// EmberDataRoleProp classifies a class under adapters/, serializers/ or
// transforms/ — the rest of ember-data's per-model quartet. The value is the
// directory role; the file base names the model it serves, which the binder
// links (the reserved `application` base is the app-wide fallback and names no
// model).
const EmberDataRoleProp = "ember_data_role"

var emberDataRoleDirs = map[string]string{
	"adapters":    "adapter",
	"serializers": "serializer",
	"transforms":  "transform",
}

// emberDataRoleForFile returns the data-layer role the app tree assigns relFile,
// or "".
func emberDataRoleForFile(relFile string) string {
	slashed := filepath.ToSlash(relFile)
	for dir, role := range emberDataRoleDirs {
		if strings.HasPrefix(slashed, "app/"+dir+"/") || strings.Contains(slashed, "/app/"+dir+"/") {
			return role
		}
	}
	return ""
}

// emberDefaultExportName returns the identifier of a separate
// `export default Name;` statement, or "".
func emberDefaultExportName(kinds *tsutil.KindTable, root *sitter.Node, src []byte) string {
	for i := range root.ChildCount() {
		node := root.Child(i)
		if kindOf(kinds, node) != "export_statement" || !hasChildKind(kinds, node, "default") {
			continue
		}
		if decl := firstDeclChild(kinds, node); decl != nil {
			switch kindOf(kinds, decl) {
			case "function_declaration", "generator_function_declaration":
				if id := findChildByKind(kinds, decl, "identifier"); id != nil {
					return nodeText(id, src)
				}
			}
			continue
		}
		if id := findChildByKind(kinds, node, "identifier"); id != nil {
			return nodeText(id, src)
		}
	}
	return ""
}

// collectEmberTemplateBindings walks top-level variable declarations (unwrapping
// export statements) and returns each declarator's name and value span, so a
// segment blanked into the declarator's backtick literal can classify the
// binding as a component.
func collectEmberTemplateBindings(kinds *tsutil.KindTable, root *sitter.Node, src []byte) []emberBindingInfo {
	var bindings []emberBindingInfo
	for i := range root.ChildCount() {
		node := root.Child(i)
		if kindOf(kinds, node) == "export_statement" {
			if decl := firstDeclChild(kinds, node); decl != nil {
				node = decl
			}
		}
		if kindOf(kinds, node) != "lexical_declaration" && kindOf(kinds, node) != "variable_declaration" {
			continue
		}
		for j := range node.ChildCount() {
			d := node.Child(j)
			if kindOf(kinds, d) != "variable_declarator" {
				continue
			}
			name := findChildByKind(kinds, d, "identifier")
			val := d.ChildByFieldName("value")
			if name == nil || val == nil {
				continue
			}
			bindings = append(bindings, emberBindingInfo{
				name:  nodeText(name, src),
				start: int(val.StartByte()),
				end:   int(val.EndByte()),
			})
		}
	}
	return bindings
}

// emberServiceDecoratorNames returns the local names bound to the service
// decorator export of @ember/service (`service`, and the legacy `inject`).
func emberServiceDecoratorNames(bindings emberImportBindings) map[string]bool {
	names := make(map[string]bool)
	for local, mod := range bindings.external {
		if emberServiceModules[mod] {
			names[local] = true
		}
	}
	return names
}

// emberRelationshipDecoratorNames maps the local names bound to ember-data's
// belongsTo/hasMany exports to a relationship kind. An aliased import loses the
// export name in the binding map and is not recognized — aliasing these
// decorators is vanishingly rare, and a missed relationship is recoverable.
func emberRelationshipDecoratorNames(bindings emberImportBindings) map[string]string {
	names := make(map[string]string)
	for local, mod := range bindings.external {
		if !emberModelModules[mod] {
			continue
		}
		switch local {
		case "belongsTo":
			names[local] = "belongs_to"
		case "hasMany":
			names[local] = "has_many"
		}
	}
	return names
}

// emberModelRelationships reads @belongsTo/@hasMany fields off a model class
// body. The explicit string argument names the related model; a bare @belongsTo
// falls back to the dasherized field name (the decorator's own default), while a
// bare @hasMany is skipped — recovering the singular model name from a plural
// field requires an inflector, and a guessed edge is worse than a missing one.
// Entries are "belongs_to:<name>" / "has_many:<name>", sorted.
func emberModelRelationships(kinds *tsutil.KindTable, classBody *sitter.Node, src []byte, decorators map[string]string) []string {
	if len(decorators) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	for i := range classBody.ChildCount() {
		member := classBody.Child(i)
		if kindOf(kinds, member) != "public_field_definition" && kindOf(kinds, member) != "field_definition" {
			continue
		}
		for j := range member.ChildCount() {
			dec := member.Child(j)
			if kindOf(kinds, dec) != "decorator" {
				continue
			}
			name, arg := emberDecoratorNameArg(kinds, dec, src)
			kind, ok := decorators[name]
			if !ok {
				continue
			}
			if arg == "" {
				if kind != "belongs_to" {
					continue
				}
				field := emberFieldName(kinds, member, src)
				if field == "" {
					continue
				}
				arg = dasherize(field)
			}
			seen[kind+":"+arg] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	rels := make([]string, 0, len(seen))
	for r := range seen {
		rels = append(rels, r)
	}
	sort.Strings(rels)
	return rels
}

// emberInjectedServices reads @service-decorated class fields. `@service store`
// injects the service named by the field (camelCase dasherized); `@service('a/b')`
// injects the named path. Names are sorted for deterministic output.
func emberInjectedServices(kinds *tsutil.KindTable, classBody *sitter.Node, src []byte, decorators map[string]bool) []string {
	if len(decorators) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	for i := range classBody.ChildCount() {
		member := classBody.Child(i)
		if kindOf(kinds, member) != "public_field_definition" && kindOf(kinds, member) != "field_definition" {
			continue
		}
		for j := range member.ChildCount() {
			dec := member.Child(j)
			if kindOf(kinds, dec) != "decorator" {
				continue
			}
			name, arg := emberDecoratorNameArg(kinds, dec, src)
			if !decorators[name] {
				continue
			}
			if arg == "" {
				if field := emberFieldName(kinds, member, src); field != "" {
					arg = dasherize(field)
				}
			}
			if arg != "" {
				seen[arg] = true
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func emberDecoratorNameArg(kinds *tsutil.KindTable, dec *sitter.Node, src []byte) (name, arg string) {
	for i := range dec.ChildCount() {
		c := dec.Child(i)
		switch kindOf(kinds, c) {
		case "identifier":
			return nodeText(c, src), ""
		case "call_expression":
			fn := c.ChildByFieldName("function")
			if fn == nil || kindOf(kinds, fn) != "identifier" {
				return "", ""
			}
			if args := c.ChildByFieldName("arguments"); args != nil {
				if s := findChildByKind(kinds, args, "string"); s != nil {
					return nodeText(fn, src), strings.Trim(nodeText(s, src), `"'`)
				}
			}
			return nodeText(fn, src), ""
		}
	}
	return "", ""
}

func emberFieldName(kinds *tsutil.KindTable, member *sitter.Node, src []byte) string {
	if n := member.ChildByFieldName("name"); n != nil {
		return nodeText(n, src)
	}
	if n := findChildByKind(kinds, member, "property_identifier"); n != nil {
		return nodeText(n, src)
	}
	return ""
}

// EmberServicesProp carries the sorted service names a class injects, and
// EmberRelationshipsProp the sorted "belongs_to:<name>" / "has_many:<name>"
// entries of an ember-data model; the ember-resolver binder resolves both
// against the store.
const (
	EmberServicesProp      = "ember_injected_services"
	EmberRelationshipsProp = "ember_relationships"
)

// EmberDefaultExportProp marks the symbol a resolver name means when a module
// exports several — Ember resolution is default-export resolution, so the
// binder prefers the symbol carrying it.
const EmberDefaultExportProp = "ember_default_export"

// emberEnrich applies Ember classification to the already-extracted declaration
// facts of one file. Each template segment attaches to the declaration that owns
// it — the class whose body embeds it (which is thereby a component, whatever
// its superclass), or the `const Name = <template>…` binding it initializes —
// and a segment owned by neither is the file's standalone default component,
// synthesized the way a script-less Vue SFC is. Service classes, @service
// injections and ember-data models gain their props and companion facts.
func emberEnrich(kinds *tsutil.KindTable, result []facts.Fact, root *sitter.Node, src []byte, relFile string,
	aliases map[string]tsAlias, segments []emberTemplateSegment) []facts.Fact {

	dir := factpath.Dir(relFile)
	base := strings.TrimSuffix(filepath.Base(relFile), filepath.Ext(relFile))
	bindings := buildEmberImportBindings(kinds, root, src, relFile, aliases)
	folds := emberBuildFoldMap(kinds, root, src)
	classes := collectEmberClasses(kinds, root, src, bindings, folds)
	templateBindings := collectEmberTemplateBindings(kinds, root, src)
	frameworkRegistered := emberFrameworkRegisteredFile(relFile)

	// A template may also render a component declared in its own file (a named
	// sibling binding or class) — those locals are statically known, so they join
	// the resolution set. Imports win on a name collision.
	for _, cls := range classes {
		if cls.name != "" {
			if _, taken := bindings.internal[cls.name]; !taken {
				bindings.internal[cls.name] = dir + "." + cls.name
			}
		}
	}
	for _, tb := range templateBindings {
		if _, taken := bindings.internal[tb.name]; !taken {
			bindings.internal[tb.name] = dir + "." + tb.name
		}
	}

	refsFor := func(owns func(emberTemplateSegment) bool) []string {
		var owned []emberTemplateSegment
		for _, seg := range segments {
			if owns(seg) {
				owned = append(owned, seg)
			}
		}
		return emberTemplateRefs(owned, bindings)
	}
	segmentText := func(start, end int) string {
		var b strings.Builder
		for _, seg := range segments {
			if seg.Start >= start && seg.Start < end {
				b.WriteString(seg.Content)
				b.WriteByte(0)
			}
		}
		return b.String()
	}
	applySegmentProps := func(f *facts.Fact, start, end int) {
		text := segmentText(start, end)
		if text == "" {
			return
		}
		if typed := scanTypedLiteralInvocations(text, folds); len(typed) > 0 {
			f.SetProp(EmberInvocationsProp, mergeSorted(propStringsLocal(f.PropAny(EmberInvocationsProp)), typed))
		}
		if yh := scanEmberYieldHash(text); len(yh) > 0 {
			resolved := make([]string, 0, len(yh))
			for _, e := range yh {
				if k, v, ok := strings.Cut(e, "=?"); ok {
					if target, bound := bindings.internal[v]; bound {
						resolved = append(resolved, k+"=@"+target)
					}
					continue
				}
				resolved = append(resolved, e)
			}
			if len(resolved) > 0 {
				sort.Strings(resolved)
				f.SetProp(EmberYieldHashProp, resolved)
			}
		}
		if ctx := scanEmberContextualUses(text); len(ctx) > 0 {
			resolved := make([]string, 0, len(ctx))
			for _, pair := range ctx {
				comp, key, _ := strings.Cut(pair, "#")
				if target, ok := bindings.internal[comp]; ok {
					resolved = append(resolved, target+"#"+key)
				} else {
					resolved = append(resolved, pair)
				}
			}
			sort.Strings(resolved)
			f.SetProp(EmberContextualProp, resolved)
		}
		if n, samples := countDynamicInvocations(text, folds); n > 0 {
			f.SetProp(EmberDynamicCountProp, n)
			f.SetProp(EmberDynamicSamplesProp, samples)
		}
	}

	linksFor := func(start, end int) []string {
		seen := make(map[string]bool)
		for _, seg := range segments {
			if seg.Start < start || seg.Start >= end {
				continue
			}
			for _, l := range scanEmberRouteLinks(seg.Content) {
				seen[l] = true
			}
		}
		if len(seen) == 0 {
			return nil
		}
		links := make([]string, 0, len(seen))
		for l := range seen {
			links = append(links, l)
		}
		sort.Strings(links)
		return links
	}
	attachCalls := func(f *facts.Fact, targets []string) {
		for _, t := range targets {
			if t != f.Name && !f.HasRelation(facts.RelCalls, t) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: t})
			}
		}
	}
	markComponent := func(f *facts.Fact) {
		f.SetProp("web_component", "component")
		f.SetProp("framework", EmberFramework)
	}
	claimed := make(map[int]bool)

	if defaultName := emberDefaultExportName(kinds, root, src); defaultName != "" {
		factName := dir + "." + defaultName
		for i := range result {
			if result[i].Kind == facts.KindSymbol && result[i].Name == factName {
				result[i].SetProp(EmberDefaultExportProp, true)
				break
			}
		}
	}

	if frameworkRegistered {
		for i := range result {
			if result[i].Kind == facts.KindSymbol && result[i].PropAny("receiver") == nil {
				result[i].SetProp("framework_registered", true)
			}
		}
	}

	for ci := range classes {
		cls := &classes[ci]
		if cls.name == "" {
			continue
		}
		var classRefs []string
		hasTemplate := false
		classRefs = refsFor(func(seg emberTemplateSegment) bool {
			owns := seg.Start >= cls.start && seg.Start < cls.end
			if owns {
				hasTemplate = true
				claimed[seg.Start] = true
			}
			return owns
		})
		factName := dir + "." + cls.name
		for i := range result {
			if result[i].Kind != facts.KindSymbol || result[i].Name != factName {
				continue
			}
			if cls.isComponent || hasTemplate {
				markComponent(&result[i])
				attachCalls(&result[i], classRefs)
				applySegmentProps(&result[i], cls.start, cls.end)
			}

			merged := make(map[string]bool)
			for _, l := range linksFor(cls.start, cls.end) {
				merged[l] = true
			}
			for _, l := range cls.codeLinks {
				merged[l] = true
			}
			if len(merged) > 0 {
				links := make([]string, 0, len(merged))
				for l := range merged {
					links = append(links, l)
				}
				sort.Strings(links)
				result[i].SetProp(EmberRouteLinksProp, links)
			}
			if cls.isDefault {
				result[i].SetProp(EmberDefaultExportProp, true)
			}
			if cls.isService {
				result[i].SetProp("ember_service", base)
			}
			if role := emberDataRoleForFile(relFile); role != "" {
				result[i].SetProp(EmberDataRoleProp, role)
			}
			if len(cls.services) > 0 {
				result[i].SetProp(EmberServicesProp, cls.services)
			}
			break
		}
		if cls.isModel {
			props := map[string]any{
				"storage_kind": "model",
				"framework":    "ember-data",
				"table":        base,
				"language":     "typescript",
			}
			if len(cls.relationships) > 0 {
				props[EmberRelationshipsProp] = cls.relationships
			}
			if len(cls.attrTransforms) > 0 {
				props[EmberAttrTransformsProp] = cls.attrTransforms
			}
			result = append(result, facts.Fact{
				Kind:      facts.KindStorage,
				Name:      factName,
				File:      relFile,
				Line:      cls.line,
				Props:     props,
				Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
			})
		}
	}

	for _, tb := range templateBindings {
		refs := refsFor(func(seg emberTemplateSegment) bool {
			owns := seg.Start >= tb.start && seg.Start < tb.end
			if owns {
				claimed[seg.Start] = true
			}
			return owns
		})
		if refs == nil && !anySegmentIn(segments, tb.start, tb.end) {
			continue
		}
		factName := dir + "." + tb.name
		for i := range result {
			if result[i].Kind != facts.KindSymbol || result[i].Name != factName {
				continue
			}
			markComponent(&result[i])
			attachCalls(&result[i], refs)
			if links := linksFor(tb.start, tb.end); len(links) > 0 {
				result[i].SetProp(EmberRouteLinksProp, links)
			}
			applySegmentProps(&result[i], tb.start, tb.end)
			break
		}
	}

	if isEmberTemplateTagFile(relFile) {
		refs := refsFor(func(seg emberTemplateSegment) bool { return !claimed[seg.Start] })
		unclaimed := false
		for _, seg := range segments {
			if !claimed[seg.Start] {
				unclaimed = true
			}
		}
		if unclaimed {
			rels := []facts.Relation{{Kind: facts.RelDeclares, Target: dir}}
			for _, t := range refs {
				rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: t})
			}
			var unclaimedText strings.Builder
			for _, seg := range segments {
				if !claimed[seg.Start] {
					unclaimedText.WriteString(seg.Content)
					unclaimedText.WriteByte(0)
				}
			}
			props := map[string]any{
				"symbol_kind":          facts.SymbolFunc,
				"exported":             true,
				"language":             "typescript",
				"web_component":        "component",
				"framework":            EmberFramework,
				EmberDefaultExportProp: true,
			}
			linkSeen := make(map[string]bool)
			for _, seg := range segments {
				if claimed[seg.Start] {
					continue
				}
				for _, l := range scanEmberRouteLinks(seg.Content) {
					linkSeen[l] = true
				}
			}
			if len(linkSeen) > 0 {
				links := make([]string, 0, len(linkSeen))
				for l := range linkSeen {
					links = append(links, l)
				}
				sort.Strings(links)
				props[EmberRouteLinksProp] = links
			}
			text := unclaimedText.String()
			if typed := scanTypedLiteralInvocations(text, folds); len(typed) > 0 {
				props[EmberInvocationsProp] = typed
			}
			if yh := scanEmberYieldHash(text); len(yh) > 0 {
				resolvedYH := make([]string, 0, len(yh))
				for _, e := range yh {
					if k, v, ok := strings.Cut(e, "=?"); ok {
						if target, bound := bindings.internal[v]; bound {
							resolvedYH = append(resolvedYH, k+"=@"+target)
						}
						continue
					}
					resolvedYH = append(resolvedYH, e)
				}
				if len(resolvedYH) > 0 {
					sort.Strings(resolvedYH)
					props[EmberYieldHashProp] = resolvedYH
				}
			}
			if ctx := scanEmberContextualUses(text); len(ctx) > 0 {
				props[EmberContextualProp] = ctx
			}
			if n, samples := countDynamicInvocations(text, folds); n > 0 {
				props[EmberDynamicCountProp] = n
				props[EmberDynamicSamplesProp] = samples
			}
			result = append(result, facts.Fact{
				Kind:      facts.KindSymbol,
				Name:      dir + "." + fileSymbolName(relFile),
				File:      relFile,
				Line:      1,
				Props:     props,
				Relations: rels,
			})
		}
	}

	return result
}

func anySegmentIn(segments []emberTemplateSegment, start, end int) bool {
	for _, seg := range segments {
		if seg.Start >= start && seg.Start < end {
			return true
		}
	}
	return false
}

// EmberTemplateProp marks the file_ref carrier an .hbs template emits;
// EmberInvocationsProp holds the sorted invocation names found in it, and
// EmberOwnerFileProp the co-located class file when one exists. The
// ember-resolver binder consumes all three.
const (
	EmberTemplateProp    = "ember_template"
	EmberInvocationsProp = "ember_invocations"
	EmberOwnerFileProp   = "ember_owner_file"
	EmberRouteLinksProp  = "ember_route_links"
)

// scanEmberRouteLinks collects the route names a template links to —
// `<LinkTo @route="jobs.job">`, `{{link-to route="…"}}` — sorted and deduped.
// The name is matched against router-map facts by the binder, giving the
// navigation graph the same treatment invocations get.
func scanEmberRouteLinks(text string) []string {
	seen := make(map[string]bool)
	for _, marker := range []string{`@route="`, `@route='`, `route="`, `route='`} {
		pos := 0
		for {
			idx := strings.Index(text[pos:], marker)
			if idx < 0 {
				break
			}
			start := pos + idx + len(marker)
			quote := marker[len(marker)-1]
			end := strings.IndexByte(text[start:], quote)
			if end < 0 {
				break
			}
			name := text[start : start+end]
			if name != "" && !strings.ContainsAny(name, "{} \t\n") {
				seen[name] = true
			}
			pos = start + end
		}
	}
	if len(seen) == 0 {
		return nil
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// emberHbsKeywords are Glimmer's built-in helpers and keywords: never component
// invocations, so never candidates for resolution.
var emberHbsKeywords = map[string]bool{
	"if": true, "unless": true, "each": true, "each-in": true, "let": true,
	"yield": true, "outlet": true, "component": true, "helper": true,
	"modifier": true, "action": true, "fn": true, "on": true, "get": true,
	"concat": true, "array": true, "hash": true, "debugger": true, "log": true,
	"mount": true, "input": true, "textarea": true, "link-to": true,
	"query-params": true, "unbound": true, "mut": true, "in-element": true,
	"has-block": true, "has-block-params": true, "else": true,
}

// extractEmberHbs models one classic .hbs template. The invocation names it
// records are candidates; resolution happens in the ember-resolver binder where
// the whole store is visible. A component template with no co-located class is a
// template-only component and synthesizes its component symbol here, since a
// binder may never add facts.
func (e *TSExtractor) extractEmberHbs(src []byte, relFile string, knownFiles map[string]bool) []facts.Fact {
	dir := factpath.Dir(relFile)
	text := string(src)
	invocations := mergeSorted(scanHbsInvocations(text), scanTypedLiteralInvocations(text, nil))

	slashed := filepath.ToSlash(relFile)
	ownerFile := ""
	ownerBases := []string{strings.TrimSuffix(slashed, filepath.Ext(slashed))}
	// The classic pre-Octane split keeps a component's template under
	// app/templates/components/ with its class under app/components/; a pods
	// component keeps them as siblings named template/component. Both are
	// additional owner candidates, not a mode.
	if idx := strings.Index(slashed, "app/templates/components/"); idx >= 0 {
		ownerBases = append(ownerBases,
			slashed[:idx]+"app/components/"+strings.TrimSuffix(slashed[idx+len("app/templates/components/"):], filepath.Ext(slashed)))
	}
	if filepath.Base(slashed) == "template.hbs" {
		ownerBases = append(ownerBases, factpath.Join(factpath.Dir(slashed), "component"))
	}
	for _, ob := range ownerBases {
		for _, ext := range []string{".ts", ".js", ".gts", ".gjs"} {
			if knownFiles[ob+ext] {
				ownerFile = ob + ext
				break
			}
		}
		if ownerFile != "" {
			break
		}
	}

	var result []facts.Fact
	isPodsComponentTemplate := filepath.Base(slashed) == "template.hbs" &&
		(strings.HasPrefix(slashed, "app/pods/") || strings.Contains(slashed, "/app/pods/"))
	isComponentTemplate := strings.HasPrefix(slashed, "app/components/") ||
		strings.Contains(slashed, "/app/components/") ||
		strings.HasPrefix(slashed, "app/templates/components/") ||
		strings.Contains(slashed, "/app/templates/components/") ||
		isPodsComponentTemplate
	if ownerFile == "" && isComponentTemplate {
		synthName := fileSymbolName(relFile)
		if isPodsComponentTemplate {
			synthName = toPascal(filepath.Base(dir))
		}
		result = append(result, facts.Fact{
			Kind: facts.KindSymbol,
			Name: dir + "." + synthName,
			File: relFile,
			Line: 1,
			Props: map[string]any{
				"symbol_kind":          facts.SymbolFunc,
				"exported":             true,
				"language":             "handlebars",
				"web_component":        "component",
				"framework":            EmberFramework,
				EmberDefaultExportProp: true,
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
		})
	}

	props := map[string]any{
		"language":        "handlebars",
		EmberTemplateProp: true,
	}
	if len(invocations) > 0 {
		props[EmberInvocationsProp] = invocations
	}
	if links := scanEmberRouteLinks(text); len(links) > 0 {
		props[EmberRouteLinksProp] = links
	}
	if yh := scanEmberYieldHash(text); len(yh) > 0 {
		kept := make([]string, 0, len(yh))
		for _, e := range yh {
			if !strings.Contains(e, "=?") {
				kept = append(kept, e)
			}
		}
		if len(kept) > 0 {
			props[EmberYieldHashProp] = kept
		}
	}
	if ctx := scanEmberContextualUses(text); len(ctx) > 0 {
		props[EmberContextualProp] = ctx
	}
	if n, samples := countDynamicInvocations(text, nil); n > 0 {
		props[EmberDynamicCountProp] = n
		props[EmberDynamicSamplesProp] = samples
	}
	if ownerFile != "" {
		props[EmberOwnerFileProp] = ownerFile
	}
	result = append(result, facts.Fact{
		Kind:  facts.KindFileRef,
		Name:  relFile,
		File:  relFile,
		Line:  1,
		Props: props,
	})
	return result
}

// scanHbsInvocations collects component/helper invocation names from template
// text: angle-bracket tags starting with an uppercase letter (`<CoreModal`,
// `<Ui::Button`) and mustache/subexpression heads in kebab-case with at least one
// hyphen (`{{format-date …}}`, `(errors-for …)`). The hyphen requirement on curly
// names is the determinism line: a bare `{{title}}` is indistinguishable from a
// property, while a hyphenated name cannot be one.
func scanHbsInvocations(text string) []string {
	seen := make(map[string]bool)
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '<':
			j := i + 1
			if j < len(text) && text[j] >= 'A' && text[j] <= 'Z' {
				k := j
				for k < len(text) && (isHbsNameByte(text[k]) || text[k] == ':') {
					k++
				}
				seen[strings.TrimRight(text[j:k], ":")] = true
				i = k
			}
		case '{', '(':
			var j int
			if text[i] == '{' {
				if i+1 >= len(text) || text[i+1] != '{' {
					continue
				}
				j = i + 2
			} else {
				j = i + 1
			}
			for j < len(text) && (text[j] == '#' || text[j] == ' ') {
				j++
			}
			k := j
			for k < len(text) && isHbsNameByte(text[k]) {
				k++
			}
			name := text[j:k]
			if strings.Contains(name, "-") && !emberHbsKeywords[name] &&
				name != "" && name[0] >= 'a' && name[0] <= 'z' {
				seen[name] = true
			}
			i = k
		}
	}
	if len(seen) == 0 {
		return nil
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func isHbsNameByte(b byte) bool {
	return b == '-' || b == '_' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// isEmberRouterFile reports whether relFile is the app's router map.
func isEmberRouterFile(relFile string) bool {
	base := filepath.Base(relFile)
	return base == "router.ts" || base == "router.js"
}

// extractEmberRoutes walks Router.map's `this.route(name, {path}, fn)` DSL and
// emits one client-side page route per declaration, with parent paths composed
// the way Ember's router composes them. These are UI routes, not HTTP contracts —
// the same modelling Nuxt pages and SvelteKit routes already get.
func extractEmberRoutes(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string) []facts.Fact {
	var result []facts.Fact
	var walk func(n *sitter.Node, prefix, namePrefix string)
	walk = func(n *sitter.Node, prefix, namePrefix string) {
		if n == nil {
			return
		}
		if kindOf(kinds, n) == "call_expression" {
			if fn := n.ChildByFieldName("function"); fn != nil &&
				kindOf(kinds, fn) == "member_expression" && nodeText(fn, src) == "this.mount" {
				name, path, _, _ := emberRouteArgs(kinds, n, src)
				if name != "" {
					result = append(result, facts.Fact{
						Kind: facts.KindRoute,
						Name: joinEmberPath(prefix, path),
						File: relFile,
						Line: int(n.StartPosition().Row) + 1,
						Props: map[string]any{
							"method":       "GET",
							"type":         "engine_mount",
							"router":       "map",
							"language":     "typescript",
							"framework":    EmberFramework,
							"ember_engine": name,
						},
					})
					return
				}
			}
			if fn := n.ChildByFieldName("function"); fn != nil &&
				kindOf(kinds, fn) == "member_expression" && nodeText(fn, src) == "this.route" {
				name, path, resetNamespace, callback := emberRouteArgs(kinds, n, src)
				if name != "" {
					full := joinEmberPath(prefix, path)
					routeName := name
					if namePrefix != "" && !resetNamespace {
						routeName = namePrefix + "." + name
					}
					result = append(result, facts.Fact{
						Kind: facts.KindRoute,
						Name: full,
						File: relFile,
						Line: int(n.StartPosition().Row) + 1,
						Props: map[string]any{
							"method":           "GET",
							"type":             "page",
							"router":           "map",
							"language":         "typescript",
							"framework":        EmberFramework,
							"ember_route_name": routeName,
						},
					})
					if callback != nil {
						walk(callback, full, routeName)
						return
					}
					return
				}
			}
		}
		for i := range n.ChildCount() {
			walk(n.Child(i), prefix, namePrefix)
		}
	}
	walk(root, "", "")
	return result
}

// emberRouteArgs pulls (name, path, resetNamespace, nested-callback) out of one
// this.route call. path defaults to the route name when no {path: …} option is
// given, matching the router's own default; resetNamespace restarts the route
// NAME at this segment while the URL path keeps nesting — exactly the router's
// semantics.
func emberRouteArgs(kinds *tsutil.KindTable, call *sitter.Node, src []byte) (name, path string, resetNamespace bool, callback *sitter.Node) {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return "", "", false, nil
	}
	for i := range args.ChildCount() {
		a := args.Child(i)
		switch kindOf(kinds, a) {
		case "string":
			if name == "" {
				name = strings.Trim(nodeText(a, src), `"'`)
			}
		case "object":
			for j := range a.ChildCount() {
				pair := a.Child(j)
				if kindOf(kinds, pair) != "pair" {
					continue
				}
				key := pair.ChildByFieldName("key")
				val := pair.ChildByFieldName("value")
				if key == nil || val == nil {
					continue
				}
				switch nodeText(key, src) {
				case "path":
					if kindOf(kinds, val) == "string" {
						path = strings.Trim(nodeText(val, src), `"'`)
					}
				case "resetNamespace":
					resetNamespace = nodeText(val, src) == "true"
				}
			}
		case "function_expression", "arrow_function":
			callback = a
		}
	}
	if path == "" {
		path = name
	}
	return name, path, resetNamespace, callback
}

func joinEmberPath(prefix, path string) string {
	path = strings.TrimPrefix(path, "/")
	prefix = strings.TrimSuffix(prefix, "/")
	if path == "" {
		if prefix == "" {
			return "/"
		}
		return prefix
	}
	if prefix == "" {
		return "/" + path
	}
	return prefix + "/" + path
}

// emberFrameworkRegisteredDirs are the role directories whose classes Ember's
// container instantiates by name — nothing in-repo imports them, so without the
// stamp the dead-code detector flags live classes. The list is the resolver's
// own layout, not a heuristic; the stamp is additive metadata (fabricating
// inbound edges would pollute impact analysis).
var emberFrameworkRegisteredDirs = map[string]bool{
	"adapters": true, "serializers": true, "transforms": true,
	"initializers": true, "instance-initializers": true,
	"routes": true, "controllers": true,
}

// emberFrameworkRegisteredFile reports whether relFile sits in a
// container-resolved role directory under the app tree.
func emberFrameworkRegisteredFile(relFile string) bool {
	slashed := filepath.ToSlash(relFile)
	for dir := range emberFrameworkRegisteredDirs {
		if strings.HasPrefix(slashed, "app/"+dir+"/") || strings.Contains(slashed, "/app/"+dir+"/") {
			return true
		}
	}
	return false
}

// emberBuildFoldMap collects file-local single-assignment string constants
// (`const NAME = 'literal'`) so scanners can fold an identifier argument the
// file states outright. The single-assignment discipline — a reassigned or
// non-string binding folds nothing — is litfold's, the shared definition of
// derivable; this function owns only the AST walk that feeds it.
func emberBuildFoldMap(kinds *tsutil.KindTable, root *sitter.Node, src []byte) *litfold.Assignments {
	folds := litfold.NewAssignments()
	for i := range root.ChildCount() {
		node := root.Child(i)
		if kindOf(kinds, node) == "export_statement" {
			if decl := firstDeclChild(kinds, node); decl != nil {
				node = decl
			}
		}
		if kindOf(kinds, node) != "lexical_declaration" || !strings.HasPrefix(nodeText(node, src), "const") {
			continue
		}
		for j := range node.ChildCount() {
			d := node.Child(j)
			if kindOf(kinds, d) != "variable_declarator" {
				continue
			}
			name := findChildByKind(kinds, d, "identifier")
			val := d.ChildByFieldName("value")
			if name == nil || val == nil {
				continue
			}
			if kindOf(kinds, val) == "string" {
				folds.Add(nodeText(name, src), strings.Trim(nodeText(val, src), `"'`))
			} else {
				folds.Add(nodeText(name, src), "")
			}
		}
	}
	return folds
}

// emberModelAttrTransforms reads @attr('type') fields off a model class body;
// the type string names a transform the way relationships name models. A bare
// @attr has no transform and draws nothing.
func emberModelAttrTransforms(kinds *tsutil.KindTable, classBody *sitter.Node, src []byte, attrNames map[string]bool) []string {
	if len(attrNames) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	for i := range classBody.ChildCount() {
		member := classBody.Child(i)
		if kindOf(kinds, member) != "public_field_definition" && kindOf(kinds, member) != "field_definition" {
			continue
		}
		for j := range member.ChildCount() {
			dec := member.Child(j)
			if kindOf(kinds, dec) != "decorator" {
				continue
			}
			name, arg := emberDecoratorNameArg(kinds, dec, src)
			if attrNames[name] && arg != "" {
				seen[arg] = true
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	types := make([]string, 0, len(seen))
	for t := range seen {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

// scanTypedLiteralInvocations collects `{{component "x"}}` / `(helper "y")` /
// `(modifier "z")` forms — the dynamic helpers with a literal (or foldable)
// name argument, exactly as deterministic as a direct invocation. Entries are
// "component:x"-style, the explicit type replacing the hyphen requirement.
func scanTypedLiteralInvocations(text string, folds *litfold.Assignments) []string {
	seen := make(map[string]bool)
	for _, kind := range []string{"component", "helper", "modifier"} {
		for _, opener := range []string{"{{" + kind + " ", "(" + kind + " "} {
			pos := 0
			for {
				idx := strings.Index(text[pos:], opener)
				if idx < 0 {
					break
				}
				start := pos + idx + len(opener)
				for start < len(text) && text[start] == ' ' {
					start++
				}
				if start < len(text) {
					if text[start] == '"' || text[start] == '\'' {
						quote := text[start]
						if end := strings.IndexByte(text[start+1:], quote); end >= 0 {
							name := text[start+1 : start+1+end]
							if name != "" && !strings.ContainsAny(name, " {}") {
								seen[kind+":"+name] = true
							}
						}
					} else if folds != nil {
						k := start
						for k < len(text) && isHbsNameByte(text[k]) {
							k++
						}
						if lit, ok := folds.Resolve(text[start:k]); ok {
							seen[kind+":"+lit] = true
						}
					}
				}
				pos = start
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// countDynamicInvocations counts the same helper forms whose argument is
// neither literal nor foldable — the irreducibly runtime sites. Visibility,
// never speculation: the count and capped samples are all that is asserted.
func countDynamicInvocations(text string, folds *litfold.Assignments) (int, []string) {
	count := 0
	var samples []string
	for _, kind := range []string{"component", "helper", "modifier"} {
		for _, opener := range []string{"{{" + kind + " ", "(" + kind + " "} {
			pos := 0
			for {
				idx := strings.Index(text[pos:], opener)
				if idx < 0 {
					break
				}
				start := pos + idx + len(opener)
				for start < len(text) && text[start] == ' ' {
					start++
				}
				if start < len(text) && text[start] != '"' && text[start] != '\'' {
					k := start
					for k < len(text) && isHbsNameByte(text[k]) {
						k++
					}
					expr := text[start:k]
					if _, folded := folds.Resolve(expr); !folded && expr != "" {
						count++
						if len(samples) < 3 {
							samples = append(samples, kind+" "+expr)
						}
					}
				}
				pos = start
			}
		}
	}
	sort.Strings(samples)
	return count, samples
}

// scanEmberYieldHash reads `{{yield (hash Key=(component "name") …)}}` forms,
// returning the sorted "Key=name" entries — the yielding half of a contextual
// component. Only literal component names qualify.
func scanEmberYieldHash(text string) []string {
	seen := make(map[string]bool)
	pos := 0
	for {
		idx := strings.Index(text[pos:], "{{yield")
		if idx < 0 {
			break
		}
		hashIdx := strings.Index(text[pos+idx:], "(hash")
		if hashIdx < 0 {
			pos += idx + 7
			continue
		}
		region := text[pos+idx+hashIdx:]
		depth := 0
		bounded := false
		for i := 0; i < len(region); i++ {
			if region[i] == '(' {
				depth++
			} else if region[i] == ')' {
				depth--
				if depth == 0 {
					region = region[:i]
					bounded = true
					break
				}
			}
		}
		if !bounded {
			// An unbalanced hash means a malformed template; scanning the whole
			// remaining file for it would be quadratic on large templates.
			pos += idx + hashIdx + 5
			continue
		}
		// Strict-mode templates pass imported components directly:
		// `(hash Header=ModalHeader)`. A bare-identifier value is recorded
		// with a "?" marker; the caller resolves it against the file's import
		// bindings, and an unbindable identifier drops.
		identPos := 0
		for identPos < len(region) {
			eq := strings.IndexByte(region[identPos:], '=')
			if eq < 0 {
				break
			}
			at := identPos + eq
			keyEnd := at
			keyStart := keyEnd
			for keyStart > 0 && isHbsNameByte(region[keyStart-1]) {
				keyStart--
			}
			key := region[keyStart:keyEnd]
			vStart := at + 1
			if vStart < len(region) && region[vStart] != '(' &&
				key != "" && key[0] >= 'A' && key[0] <= 'Z' {
				vEnd := vStart
				for vEnd < len(region) && isHbsNameByte(region[vEnd]) {
					vEnd++
				}
				val := region[vStart:vEnd]
				if val != "" && val[0] >= 'A' && val[0] <= 'Z' {
					seen[key+"=?"+val] = true
				}
			}
			identPos = at + 1
		}
		scanPos := 0
		for {
			cIdx := strings.Index(region[scanPos:], "=(component")
			if cIdx < 0 {
				break
			}
			keyEnd := scanPos + cIdx
			keyStart := keyEnd
			for keyStart > 0 && isHbsNameByte(region[keyStart-1]) {
				keyStart--
			}
			key := region[keyStart:keyEnd]
			argStart := keyEnd + len("=(component")
			for argStart < len(region) && (region[argStart] == ' ' || region[argStart] == '\n' ||
				region[argStart] == '\t' || region[argStart] == '\r') {
				argStart++
			}
			if argStart < len(region) && key != "" && key[0] >= 'A' && key[0] <= 'Z' {
				if region[argStart] == '"' || region[argStart] == '\'' {
					quote := region[argStart]
					if qEnd := strings.IndexByte(region[argStart+1:], quote); qEnd >= 0 {
						name := region[argStart+1 : argStart+1+qEnd]
						if name != "" {
							seen[key+"="+name] = true
						}
					}
				} else if region[argStart] >= 'A' && region[argStart] <= 'Z' {
					vEnd := argStart
					for vEnd < len(region) && isHbsNameByte(region[vEnd]) {
						vEnd++
					}
					seen[key+"=?"+region[argStart:vEnd]] = true
				}
			}
			scanPos = argStart
		}
		pos += idx + hashIdx + 5
	}
	if len(seen) == 0 {
		return nil
	}
	entries := make([]string, 0, len(seen))
	for e := range seen {
		entries = append(entries, e)
	}
	sort.Strings(entries)
	return entries
}

// scanEmberContextualUses tracks angle-bracket block params (`<Card as |card|>`
// … `<card.Item />`) within one template, returning sorted "Card#Item" pairs —
// the consuming half of a contextual component. Innermost binding wins on
// shadowing; anything crossing a template boundary is out of scope.
func scanEmberContextualUses(text string) []string {
	type frame struct {
		tag    string
		params map[string]bool
	}
	var stack []frame
	seen := make(map[string]bool)
	lookup := func(name string) string {
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].params[name] {
				return stack[i].tag
			}
		}
		return ""
	}
	i := 0
	for i < len(text) {
		if text[i] != '<' {
			i++
			continue
		}
		if i+1 < len(text) && text[i+1] == '/' {
			j := i + 2
			for j < len(text) && (isHbsNameByte(text[j]) || text[j] == ':') {
				j++
			}
			closing := text[i+2 : j]
			for len(stack) > 0 {
				top := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if top.tag == closing || top.tag == "" {
					break
				}
			}
			i = j
			continue
		}
		j := i + 1
		for j < len(text) && (isHbsNameByte(text[j]) || text[j] == ':' || text[j] == '.') {
			j++
		}
		tagName := text[i+1 : j]
		if tagName == "" {
			i++
			continue
		}
		tagEnd := j
		depth := 0
		for tagEnd < len(text) && (text[tagEnd] != '>' || depth > 0) {
			switch text[tagEnd] {
			case '{':
				depth++
			case '}':
				depth--
			}
			tagEnd++
		}
		if tagEnd >= len(text) {
			break
		}
		tagBody := text[i:tagEnd]
		selfClosing := strings.HasSuffix(strings.TrimSpace(tagBody), "/")

		if dot := strings.IndexByte(tagName, '.'); dot > 0 && tagName[0] >= 'a' && tagName[0] <= 'z' {
			paramName, key := tagName[:dot], tagName[dot+1:]
			if owner := lookup(paramName); owner != "" && key != "" && key[0] >= 'A' && key[0] <= 'Z' {
				seen[owner+"#"+key] = true
			}
		}

		params := map[string]bool{}
		if asIdx := strings.Index(tagBody, " as |"); asIdx >= 0 {
			pEnd := strings.IndexByte(tagBody[asIdx+5:], '|')
			if pEnd >= 0 {
				for _, p := range strings.Fields(tagBody[asIdx+5 : asIdx+5+pEnd]) {
					params[p] = true
				}
			}
		}
		if !selfClosing && tagName[0] >= 'A' && tagName[0] <= 'Z' {
			stack = append(stack, frame{tag: tagName, params: params})
		}
		i = tagEnd + 1
	}
	if len(seen) == 0 {
		return nil
	}
	pairs := make([]string, 0, len(seen))
	for p := range seen {
		pairs = append(pairs, p)
	}
	sort.Strings(pairs)
	return pairs
}

// isEmberEngineRoutesFile reports whether relFile is an in-repo engine's route
// map (lib/<engine>/addon/routes.{js,ts}).
func isEmberEngineRoutesFile(relFile string) (engine string, ok bool) {
	slashed := filepath.ToSlash(relFile)
	base := filepath.Base(slashed)
	if base != "routes.js" && base != "routes.ts" {
		return "", false
	}
	parts := strings.Split(slashed, "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "lib" && parts[i+2] == "addon" && i+3 == len(parts)-1 {
			return parts[i+1], true
		}
	}
	return "", false
}

// extractEmberEngineRoutes walks a buildRoutes callback with the same DSL walk
// as Router.map, emitting engine-relative routes labeled for composition. The
// composition itself happens in the repo-level post-pass, where every mount is
// visible.
func extractEmberEngineRoutes(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile, engine string) []facts.Fact {
	routes := extractEmberRoutes(kinds, root, src, relFile)
	for i := range routes {
		routes[i].SetProp("ember_engine", engine)
		routes[i].SetProp("router", "engine")
	}
	return routes
}

// composeEngineMounts rewrites engine-relative route paths onto their mount
// point when the repo mounts that engine exactly once. Two mounts genuinely
// serve both paths, and picking one would be a wrong fact — those skip, and
// the relative facts remain, labeled. Runs inside Extract, where the whole
// repo's facts are already in hand.
func composeEngineMounts(all []facts.Fact) {
	mounts := map[string][]string{}
	for _, f := range all {
		if f.Kind == facts.KindRoute && f.PropString("type") == "engine_mount" {
			name := f.PropString("ember_engine")
			mounts[name] = append(mounts[name], f.Name)
		}
	}
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindRoute || f.PropString("router") != "engine" {
			continue
		}
		ms := mounts[f.PropString("ember_engine")]
		if len(ms) != 1 {
			continue
		}
		f.Name = joinEmberPath(ms[0], strings.TrimPrefix(f.Name, "/"))
		f.SetProp("ember_mounted", true)
	}
}
