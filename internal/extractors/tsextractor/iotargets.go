package tsextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
)

// I/O that the name of the callee does not show.
//
// tsIsIOCall reads a call as written: `fetch(…)`, `axios.get(…)`, a binding
// imported from a network module. Three shapes it could not see are read here,
// each from something the source states.

// tsIOTargets are resolved call targets that are a network round trip: the
// methods of Angular's HttpClient. A service holds the client in a constructor
// parameter property (`private http: HttpClient`), so the call is written
// `this.http.post(…)`, a receiver named for nothing. The declared type of the
// field is what says it, and call resolution already reads that: the target is
// `@angular/common/http.HttpClient.post`. On one Angular application that was 460
// call sites, in services with 14 functions marked io_direct between them.
var tsIOTargets = map[string]bool{
	"@angular/common/http.HttpClient.get":     true,
	"@angular/common/http.HttpClient.post":    true,
	"@angular/common/http.HttpClient.put":     true,
	"@angular/common/http.HttpClient.patch":   true,
	"@angular/common/http.HttpClient.delete":  true,
	"@angular/common/http.HttpClient.head":    true,
	"@angular/common/http.HttpClient.options": true,
	"@angular/common/http.HttpClient.request": true,
	"@angular/common/http.HttpClient.jsonp":   true,
}

// tsPassesFetch reports whether a call hands the global fetch to something else
// to make the request with: `fetchRetry(fetch, options)`. The function that does
// so arranges a request as surely as one that calls fetch, and a wrapper built
// this way is how a client library adds retries and timeouts.
func tsPassesFetch(kinds *tsutil.KindTable, call *sitter.Node, src []byte, importMap map[string]string) bool {
	if _, shadowed := importMap["fetch"]; shadowed {
		return false // an imported `fetch` is a binding of its module, read elsewhere
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return false
	}
	for i := range args.NamedChildCount() {
		if a := args.NamedChild(i); kindOf(kinds, a) == "identifier" && nodeText(a, src) == "fetch" {
			return true
		}
	}
	return false
}

// fileReturnTypes maps the functions a file declares at its top level to the
// class they are declared to return, where the annotation is a plain type name:
//
//	function getInstance(): SupersetClientClass { … }
//	const current = (): Session => store.session
//
// It lets a call on such a function's result resolve: `getInstance().delete(x)`
// is SupersetClientClass.delete. A generic return type (`Promise<T>`,
// `Observable<T>`) names the wrapper and is not recorded, and neither is a
// function another file declares: only this file's own are known here.
func fileReturnTypes(kinds *tsutil.KindTable, root *sitter.Node, src []byte, dir string, importMap map[string]string) map[string]string {
	out := map[string]string{}
	note := func(name string, fn *sitter.Node) {
		if name == "" || fn == nil {
			return
		}
		ann := fn.ChildByFieldName("return_type")
		if ann == nil {
			return
		}
		var typ string
		for i := range ann.NamedChildCount() {
			if c := ann.NamedChild(i); kindOf(kinds, c) == "type_identifier" && ann.NamedChildCount() == 1 {
				typ = nodeText(c, src)
			}
		}
		if typ == "" {
			return
		}
		if q, ok := importMap[typ]; ok {
			if q != "" {
				out[name] = q
			}
			return
		}
		out[name] = dir + "." + typ
	}
	for i := range root.ChildCount() {
		stmt := root.Child(i)
		if kindOf(kinds, stmt) == "export_statement" {
			if d := stmt.ChildByFieldName("declaration"); d != nil {
				stmt = d
			}
		}
		switch kindOf(kinds, stmt) {
		case "function_declaration":
			if n := stmt.ChildByFieldName("name"); n != nil {
				note(nodeText(n, src), stmt)
			}
		case "lexical_declaration", "variable_declaration":
			for j := range stmt.ChildCount() {
				decl := stmt.Child(j)
				if kindOf(kinds, decl) != "variable_declarator" {
					continue
				}
				name, value := decl.ChildByFieldName("name"), decl.ChildByFieldName("value")
				if name == nil || value == nil || kindOf(kinds, name) != "identifier" {
					continue
				}
				if k := kindOf(kinds, value); k == "arrow_function" || k == "function_expression" || k == "function" {
					note(nodeText(name, src), value)
				}
			}
		}
	}
	return out
}
