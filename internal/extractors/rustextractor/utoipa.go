package rustextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// utoipaHTTPMethods are the bare verb tokens `#[utoipa::path(...)]` accepts as
// its operation, either directly (`get,`) or inside `method(get, head)`.
var utoipaHTTPMethods = map[string]bool{
	"get": true, "post": true, "put": true, "delete": true,
	"patch": true, "head": true, "options": true, "trace": true,
}

// extractUtoipaRoutes emits one facts.KindRoute per verb declared by a
// `#[utoipa::path(get, path = "/x")]` attribute on a handler function.
//
// utoipa_axum's `routes!(handler)` macro registers a handler WITHOUT repeating
// its path: the path lives only in this attribute, so a codebase that registers
// its API that way has no `.route("/path", …)` call for extractAxumRoutes to
// find and its whole server surface is invisible. A corpus application exposed it:
// 8 routes extracted where it declares 57.
//
// Attributes are siblings of the item they annotate, not children of it, so the
// walk pairs a pending attribute with the item that follows. Other attributes in
// between (`#[deprecated]`, `#[cfg(…)]` — both occur in real handlers) do not
// clear the pending one; the first non-attribute sibling does.
//
// Unlike extractAxumRoutes this runs at every level of the tree, so a handler
// declared in an `impl` block is covered too, and it declines to descend into a
// `#[cfg(test)]` module: a route declared there is not one the binary serves.
func extractUtoipaRoutes(root *sitter.Node, src []byte, relFile, dir string) []facts.Fact {
	var out []facts.Fact
	var walk func(parent *sitter.Node)
	walk = func(parent *sitter.Node) {
		var pending *sitter.Node
		sawCfgTest := false
		for i := uint(0); i < uint(parent.ChildCount()); i++ {
			c := parent.Child(i)
			if kindOf(c) == "attribute_item" {
				if attr, ok := parseRustAttribute(c, src); ok && attr.isCfgTest(src) {
					sawCfgTest = true
				}
				if isUtoipaPathAttribute(c, src) {
					pending = c
				}
				continue
			}
			if kindOf(c) == "mod_item" && sawCfgTest {
				pending, sawCfgTest = nil, false
				continue
			}
			if pending != nil {
				out = append(out, utoipaRouteFacts(pending, c, src, relFile, dir)...)
			}
			walk(c)
			pending, sawCfgTest = nil, false
		}
	}
	walk(root)
	return out
}

// isUtoipaPathAttribute reports whether an attribute_item is `#[utoipa::path(…)]`.
// Only the scoped spelling counts: a bare `#[path(…)]` is Rust's own built-in
// module-path attribute, which has nothing to do with HTTP.
func isUtoipaPathAttribute(item *sitter.Node, src []byte) bool {
	attr := findChildByKind(item, "attribute")
	if attr == nil || attr.NamedChildCount() == 0 {
		return false
	}
	name := attr.NamedChild(0)
	if kindOf(name) != "scoped_identifier" {
		return false
	}
	return strings.Join(strings.Fields(nodeText(name, src)), "") == "utoipa::path"
}

// utoipaRouteFacts converts one `#[utoipa::path(…)]` attribute plus the item it
// annotates into its route facts — one per declared verb — or nil if the
// attribute declares no verb or no literal path.
func utoipaRouteFacts(attr, item *sitter.Node, src []byte, relFile, dir string) []facts.Fact {
	tree := findChildByKind(findChildByKind(attr, "attribute"), "token_tree")
	if tree == nil {
		return nil
	}
	verbs, path, ok := parseUtoipaAttribute(tree, src)
	if !ok || len(verbs) == 0 {
		return nil
	}

	handler := ""
	if kindOf(item) == "function_item" {
		if name := item.ChildByFieldName("name"); name != nil {
			handler = nodeText(name, src)
		}
	}
	line := int(attr.StartPosition().Row) + 1

	out := make([]facts.Fact, 0, len(verbs))
	for _, v := range verbs {
		props := map[string]any{
			"method":    strings.ToUpper(v),
			"framework": "utoipa",
			"language":  "rust",
		}
		if handler != "" {
			props["handler"] = handler
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindRoute,
			Name:      path,
			File:      relFile,
			Line:      line,
			Props:     props,
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
		})
	}
	return out
}

// parseUtoipaAttribute reads the verbs and the composed path out of a
// `#[utoipa::path(…)]` token_tree. ok is false when the attribute declares no
// `path` key or declares one that is not a string literal — a `const`, a
// `concat!` or a macro path is left un-extracted rather than guessed at, since a
// route stored at the wrong path is worse than one that is missing.
//
// Only the token_tree's DIRECT children are read. `params(("widget" = String, Path,
// description = "…"))` and `responses(…)` nest token_trees carrying their own
// `key = value` pairs, so a recursive scan would read a parameter's description
// as the route's path.
func parseUtoipaAttribute(tree *sitter.Node, src []byte) (verbs []string, path string, ok bool) {
	seen := map[string]bool{}
	addVerb := func(v string) {
		if utoipaHTTPMethods[v] && !seen[v] {
			seen[v] = true
			verbs = append(verbs, v)
		}
	}

	contextPath := ""
	sawPath := false
	n := tree.ChildCount()
	for i := uint(0); i < n; i++ {
		c := tree.Child(i)
		if kindOf(c) != "identifier" {
			continue
		}
		key := nodeText(c, src)
		// `method(get, head)` declares several verbs for one operation.
		if key == "method" && i+1 < n && kindOf(tree.Child(i+1)) == "token_tree" {
			sub := tree.Child(i + 1)
			for j := uint(0); j < uint(sub.ChildCount()); j++ {
				if v := sub.Child(j); kindOf(v) == "identifier" {
					addVerb(nodeText(v, src))
				}
			}
			continue
		}
		if i+2 >= n || kindOf(tree.Child(i+1)) != "=" {
			addVerb(key)
			continue
		}
		value := tree.Child(i + 2)
		switch key {
		case "path":
			sawPath = true
			path, _ = stringLiteralValue(value, src)
		case "context_path":
			contextPath, _ = stringLiteralValue(value, src)
		}
	}

	if !sawPath || path == "" {
		return nil, "", false
	}
	if contextPath != "" {
		path = facts.JoinRoutePath(contextPath, path)
	}
	return verbs, path, true
}
