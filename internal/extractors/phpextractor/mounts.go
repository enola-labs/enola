package phpextractor

import (
	"bytes"
	"path"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	php "github.com/tree-sitter/tree-sitter-php/bindings/go"
)

// routeMount is one place a routes file is loaded from, with the group scopes that
// surround the load: the prefix, namespace and controller every route in the file
// is registered under.
//
// A routes file does not say where it is served. A service provider or
// bootstrap/app.php does, by handing the file to a group
// (`Route::prefix('api')->group(base_path('routes/api.php'))`), by naming it to
// withRouting, or by requiring it inside a group's closure. Read on its own the
// file yields paths one prefix short of the ones the application answers on.
type routeMount struct {
	target string
	stack  []phpRouteScope
}

// mentionsRouteMount is the cheap test for whether a file can mount a routes file,
// run before it is parsed a second time.
func mentionsRouteMount(src []byte) bool {
	if !bytes.Contains(src, []byte(".php")) {
		return false
	}
	return bytes.Contains(src, []byte("group(")) || bytes.Contains(src, []byte("withRouting(")) ||
		bytes.Contains(src, []byte("require")) || bytes.Contains(src, []byte("include"))
}

// extractLaravelMounts returns the routes files a PHP file loads.
func extractLaravelMounts(src []byte, relFile string) []routeMount {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(php.LanguagePHP())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()

	w := &laravelRouteWalker{src: src, relFile: relFile, uses: map[string]string{}, mountsOnly: true}
	collectUses(tree.RootNode(), src, w.uses)
	w.walk(tree.RootNode(), nil)
	return w.mounts
}

// addMount records that the file loads a routes file under the given scopes.
func (w *laravelRouteWalker) addMount(target string, stack []phpRouteScope) {
	if target == "" || target == w.relFile || !isLaravelRouteFile(target) {
		return
	}
	w.mounts = append(w.mounts, routeMount{target: target, stack: append([]phpRouteScope{}, stack...)})
}

// handleWithRouting reads Laravel 11's bootstrap: `->withRouting(web: …, api: …,
// apiPrefix: …)`. The api file is served under apiPrefix, which is "api" when the
// call does not give one.
func (w *laravelRouteWalker) handleWithRouting(call *sitter.Node, stack []phpRouteScope) {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return
	}
	named := map[string]*sitter.Node{}
	for i := uint(0); i < args.NamedChildCount(); i++ {
		arg := args.NamedChild(i)
		if name := arg.ChildByFieldName("name"); name != nil {
			named[phpText(name, w.src)] = argValue(arg)
		}
	}
	apiPrefix := "api"
	if node, given := named["apiPrefix"]; given {
		apiPrefix = stringLiteral(node, w.src)
	}
	for _, target := range w.routeFilePaths(named["web"]) {
		w.addMount(target, stack)
	}
	for _, target := range w.routeFilePaths(named["api"]) {
		w.addMount(target, append(append([]phpRouteScope{}, stack...), phpRouteScope{prefix: apiPrefix}))
	}
}

// routeFilePaths resolves one path expression, or an array of them.
func (w *laravelRouteWalker) routeFilePaths(node *sitter.Node) []string {
	if node == nil {
		return nil
	}
	if kindOf(node) != "array_creation_expression" {
		if target := w.routeFilePath(node); target != "" {
			return []string{target}
		}
		return nil
	}
	var out []string
	for i := uint(0); i < node.NamedChildCount(); i++ {
		el := node.NamedChild(i)
		if kindOf(el) == "array_element_initializer" && el.NamedChildCount() > 0 {
			if target := w.routeFilePath(el.NamedChild(el.NamedChildCount() - 1)); target != "" {
				out = append(out, target)
			}
		}
	}
	return out
}

// routeFilePath resolves an expression naming a PHP file to its path in the
// repository: base_path('routes/api.php'), __DIR__.'/../routes/web.php',
// dirname(__DIR__).'/routes/web.php'. "" for a path that is computed
// (`sprintf('routes/%s.php', $type)`) or leaves the repository: nothing is guessed.
func (w *laravelRouteWalker) routeFilePath(node *sitter.Node) string {
	target, ok := w.pathExpr(node)
	if !ok || !strings.HasSuffix(target, ".php") {
		return ""
	}
	target = path.Clean(target)
	if target == "." || strings.HasPrefix(target, "..") || strings.HasPrefix(target, "/") {
		return ""
	}
	return target
}

// pathExpr evaluates a path expression relative to the repository root.
func (w *laravelRouteWalker) pathExpr(node *sitter.Node) (string, bool) {
	if node == nil {
		return "", false
	}
	switch kindOf(node) {
	case "parenthesized_expression":
		if node.NamedChildCount() == 1 {
			return w.pathExpr(node.NamedChild(0))
		}
	case "name":
		if phpText(node, w.src) == "__DIR__" {
			return path.Dir(w.relFile), true
		}
	case "binary_expression":
		left, lok := w.pathExpr(node.ChildByFieldName("left"))
		right := stringLiteral(node.ChildByFieldName("right"), w.src)
		if lok && right != "" {
			return path.Join(left, right), true
		}
	case "function_call_expression":
		args := node.ChildByFieldName("arguments")
		switch phpText(node.ChildByFieldName("function"), w.src) {
		case "base_path":
			if positionalArg(args, 0) == nil {
				return ".", true
			}
			if rel := positionalString(args, 0, w.src); rel != "" {
				return rel, true
			}
		case "app_path":
			if rel := positionalString(args, 0, w.src); rel != "" {
				return path.Join("app", rel), true
			}
		case "dirname":
			if dir, ok := w.pathExpr(positionalArg(args, 0)); ok && positionalArg(args, 1) == nil {
				return path.Dir(dir), true
			}
		}
	}
	return "", false
}

// laravelMountStacks turns the mounts of a repository into the scope stacks each
// routes file is registered under. A file loaded from two places has two, because
// Laravel serves its routes under both. A routes file loaded by another routes file
// inherits that file's own stacks. A file nothing loads is not in the result.
func laravelMountStacks(files []string, mounts map[string][]routeMount) map[string][][]phpRouteScope {
	type incoming struct {
		source string
		stack  []phpRouteScope
	}
	loadedBy := map[string][]incoming{}
	for _, source := range files {
		for _, m := range mounts[source] {
			loadedBy[m.target] = append(loadedBy[m.target], incoming{source: source, stack: m.stack})
		}
	}

	resolved := map[string][][]phpRouteScope{}
	visiting := map[string]bool{}
	var stacksOf func(file string) [][]phpRouteScope
	stacksOf = func(file string) [][]phpRouteScope {
		if stacks, done := resolved[file]; done {
			return stacks
		}
		// The file nothing loads, and the one that loads itself in a circle, are
		// registered as written.
		if len(loadedBy[file]) == 0 || visiting[file] {
			return [][]phpRouteScope{nil}
		}
		visiting[file] = true
		var out [][]phpRouteScope
		seen := map[string]bool{}
		for _, in := range loadedBy[file] {
			for _, outer := range stacksOf(in.source) {
				stack := append(append([]phpRouteScope{}, outer...), in.stack...)
				if key := stackKey(stack); !seen[key] {
					seen[key] = true
					out = append(out, stack)
				}
			}
		}
		visiting[file] = false
		resolved[file] = out
		return out
	}

	stacks := map[string][][]phpRouteScope{}
	for target := range loadedBy {
		stacks[target] = stacksOf(target)
	}
	return stacks
}

// stackKey spells a scope stack, to tell two mounts of one file apart.
func stackKey(stack []phpRouteScope) string {
	var b strings.Builder
	for _, s := range stack {
		b.WriteString(s.prefix + "\x00" + s.name + "\x00" + s.controller + "\x00" + s.namespace + "\x01")
	}
	return b.String()
}
