package scalaextractor

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Scalatra routes.
//
// A Scalatra servlet or filter registers each route as a statement of its body,
// the path first and the action as a second argument group:
//
//	get("/:owner/:repository/issues")(referrersOnly { repository => ... })
//	post("/:owner/:repository/issues/new", issueCreateForm)(readableUsersOnly { ... })
//	ajaxPost("/:owner/:repository/issues/edit/:id", form) { ... }
//
// The route is the verb and the literal path; its handler is the action, written in
// place, which is given a symbol "<dir>.<Type>.handler:<VERB>:<line>" that owns its
// calls. The path must be a literal starting with "/" and the call a statement of a
// class, trait or object body, which together keep `params.get("id")` out; and the
// pass runs only in a repository that depends on Scalatra or a file that imports it,
// since a controller trait (gitbucket's IssuesControllerBase) imports nothing of
// Scalatra itself and inherits the verbs from a base that does.

// scalatraVerbs maps a route method to the HTTP verb it registers. ajaxGet and
// ajaxPost are a common project helper over get and post.
var scalatraVerbs = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT", "delete": "DELETE", "patch": "PATCH",
	"head": "HEAD", "options": "OPTIONS", "ajaxGet": "GET", "ajaxPost": "POST",
}

// usesScalatra reports whether the repository's build declares Scalatra.
func usesScalatra(repoPath string) bool {
	var builds []string
	for _, name := range []string{"build.sbt", "pom.xml", "build.gradle", "build.gradle.kts"} {
		builds = append(builds, filepath.Join(repoPath, name))
	}
	if extra, err := filepath.Glob(filepath.Join(repoPath, "project", "*.sbt")); err == nil {
		builds = append(builds, extra...)
	}
	for _, b := range builds {
		if data, err := os.ReadFile(b); err == nil && strings.Contains(string(data), "org.scalatra") {
			return true
		}
	}
	return false
}

// scalatraRoutes returns a file's Scalatra routes and their handler nodes.
func scalatraRoutes(root *sitter.Node, src []byte, relFile string, enabled bool) ([]facts.Fact, map[handlerKey]string) {
	if !enabled && !strings.Contains(string(src), "import org.scalatra") {
		return nil, nil
	}
	w := &dslWalker{src: src, relFile: relFile, dir: factpath.Dir(relFile)}
	var out []facts.Fact
	handlers := map[handlerKey]string{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if kindOf(n) == "template_body" {
			for i := uint(0); i < n.NamedChildCount(); i++ {
				if route, handler, ok := w.scalatraRoute(n.NamedChild(i)); ok {
					out = append(out, route)
					handlers[keyOf(handler)] = route.PropString("handler")
				}
			}
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	return out, handlers
}

// scalatraRoute reads one body statement as `verb("/path"[, form])(action)` or
// `verb("/path") { action }`.
func (w *dslWalker) scalatraRoute(stmt *sitter.Node) (facts.Fact, *sitter.Node, bool) {
	if kindOf(stmt) != "call_expression" {
		return facts.Fact{}, nil, false
	}
	inner := firstNamedChild(stmt)
	if inner == nil || kindOf(inner) != "call_expression" {
		return facts.Fact{}, nil, false
	}
	fn := firstNamedChild(inner)
	if fn == nil || kindOf(fn) != "identifier" {
		return facts.Fact{}, nil, false
	}
	verb, ok := scalatraVerbs[w.text(fn)]
	if !ok {
		return facts.Fact{}, nil, false
	}
	var path string
	for i := uint(0); i < inner.NamedChildCount(); i++ {
		if a := inner.NamedChild(i); kindOf(a) == "arguments" {
			if lit := firstNamedChild(a); lit != nil && kindOf(lit) == "string" {
				path = strings.Trim(w.text(lit), `"`)
			}
		}
	}
	if !strings.HasPrefix(path, "/") {
		return facts.Fact{}, nil, false
	}
	var handler *sitter.Node
	for i := uint(0); i < stmt.NamedChildCount(); i++ {
		switch c := stmt.NamedChild(i); kindOf(c) {
		case "block", "lambda_expression":
			handler = c
		case "arguments":
			if c.NamedChildCount() == 1 {
				handler = c.NamedChild(0)
			}
		}
	}
	if handler == nil {
		return facts.Fact{}, nil, false
	}
	line := int(stmt.StartPosition().Row) + 1
	name := inlineHandlerName(w.dir, enclosingTypes(stmt, w.src), verb, line)
	return facts.Fact{
		Kind: facts.KindRoute,
		Name: path,
		File: w.relFile,
		Line: line,
		Props: map[string]any{
			"language":          "scala",
			facts.PropFramework: "scalatra",
			facts.PropRole:      facts.RoleServer,
			"method":            verb,
			"path":              path,
			"handler":           name,
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
			{Kind: facts.RelHandledBy, Target: name},
		},
	}, handler, true
}
