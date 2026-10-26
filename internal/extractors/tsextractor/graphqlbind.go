package tsextractor

import (
	"sort"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// propCodeSymbol names the symbol whose code a fact sits in: the resolver method a
// code-first field is declared on, the variable or function a client operation is
// written in. Kafka call sites record it under the same key.
const propCodeSymbol = "code_symbol"

var graphqlRoots = map[string]bool{"Query": true, "Mutation": true, "Subscription": true}

// graphqlEnclosingSymbol names the declaration n sits in that is a symbol, innermost
// first: a module-scope variable (const listQuery = gql`...`), a function, or a class
// method. A variable local to a function is no symbol, so the walk goes on to the
// declaration holding it.
func graphqlEnclosingSymbol(kinds *tsutil.KindTable, n *sitter.Node, src []byte, relFile string) string {
	dir := factpath.Dir(relFile)
	for p := n.Parent(); p != nil; p = p.Parent() {
		switch kindOf(kinds, p) {
		case "variable_declarator":
			if !isModuleScopeDeclarator(kinds, p) {
				continue
			}
			if name := p.ChildByFieldName("name"); name != nil && kindOf(kinds, name) == "identifier" {
				return dir + "." + nodeText(name, src)
			}
		case "function_declaration", "generator_function_declaration":
			// A function declared inside another is, like a local variable, no symbol.
			if !isModuleScopeStatement(kinds, p) {
				continue
			}
			if name := p.ChildByFieldName("name"); name != nil && kindOf(kinds, name) == "identifier" {
				return dir + "." + nodeText(name, src)
			}
		case "method_definition":
			name := p.ChildByFieldName("name")
			for c := p.Parent(); c != nil && name != nil; c = c.Parent() {
				if k := kindOf(kinds, c); k == "class_declaration" || k == "abstract_class_declaration" {
					if cn := c.ChildByFieldName("name"); cn != nil {
						return dir + "." + nodeText(cn, src) + "." + nodeText(name, src)
					}
				}
			}
			return ""
		}
	}
	return ""
}

// insideCallArguments reports whether n is an argument of a call, directly or
// through path helpers nested in one (join(__dirname, 'x.graphql')).
func insideCallArguments(kinds *tsutil.KindTable, n *sitter.Node) bool {
	for p := n.Parent(); p != nil; p = p.Parent() {
		switch kindOf(kinds, p) {
		case "arguments":
			return true
		case "call_expression", "member_expression", "template_string", "binary_expression":
			continue
		default:
			return false
		}
	}
	return false
}

func isGraphQLRoute(f facts.Fact, role string) bool {
	if f.Kind != facts.KindRoute || f.Props[facts.PropRole] != role {
		return false
	}
	t, _ := f.Props[facts.PropRouteType].(string)
	if t == "" {
		t, _ = f.Props["type"].(string)
	}
	return t == facts.RouteTypeGraphQL || f.Props["framework"] == "graphql" || f.Props["framework"] == "graphql-sdl" ||
		f.Props["framework"] == "graphql-code-first"
}

// bindGraphQL connects the parts a GraphQL field has in one repository.
//
//   - field -> resolver: a server field is handled_by the method a code-first
//     decorator sits on, or by the one resolver-map member named Root.field
//     (`const Query = { listWidgets() {} }`, `resolvers.Query.user`); of several,
//     the one beside the field's SDL. Otherwise none.
//   - sender -> operation: the symbol an operation is written in calls it; an
//     operation in a .graphql document is called by the code that imports the
//     document (a .vue file's component, else the file's file_ref). The operation
//     and the field share a name, so a resolver's dependents reach that code.
//
// Whether an operation is answered at all is decided after linking, with every
// extractor's fields and every loaded repository in view (the unmatched-routes
// binder writes unresolved_reason).
func bindGraphQL(all []facts.Fact) {
	symbols := map[string]bool{}
	symbolFile := map[string]string{}
	members := map[string][]string{}
	for _, f := range all {
		if f.Kind != facts.KindSymbol {
			continue
		}
		symbols[f.Name] = true
		symbolFile[f.Name] = f.File
		if facts.IsTestPath(f.File) {
			continue
		}
		parts := strings.Split(f.Name, ".")
		if len(parts) >= 3 && graphqlRoots[parts[len(parts)-2]] {
			key := parts[len(parts)-2] + "." + parts[len(parts)-1]
			members[key] = append(members[key], f.Name)
		}
	}
	docOps := map[string][]string{}
	for i := range all {
		f := &all[i]
		if !isGraphQLRoute(*f, facts.RoleServer) {
			continue
		}
		target, _ := f.Props[propCodeSymbol].(string)
		if !symbols[target] {
			target = ""
			m := members[f.Name]
			var sameFile []string
			for _, c := range m {
				if symbolFile[c] == f.File {
					sameFile = append(sameFile, c)
				}
			}
			switch {
			case len(m) == 1:
				target = m[0]
			case len(sameFile) == 1:
				// Several resolver maps answer this name; the one beside the SDL is its.
				target = sameFile[0]
			}
		}
		if target != "" && !f.HasRelation(facts.RelHandledBy, target) {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: target})
		}
	}
	// The calls edges go on the symbols, so they are added in a second pass over a
	// stable index of who sends what.
	sends := map[string][]string{}
	for _, f := range all {
		if !isGraphQLRoute(f, facts.RoleClient) {
			continue
		}
		if sym, _ := f.Props[propCodeSymbol].(string); symbols[sym] {
			sends[sym] = append(sends[sym], f.Name)
		} else if isGraphQLDocFile(f.File) {
			docOps[f.File] = append(docOps[f.File], f.Name)
		}
	}
	fileRefs := map[string]bool{}
	for _, f := range all {
		if f.Kind == facts.KindFileRef {
			fileRefs[f.Name] = true
		}
	}
	for _, f := range all {
		if f.Kind != facts.KindDependency || f.Props[facts.PropSource] != "internal" {
			continue
		}
		for _, r := range f.Relations {
			ops := docOps[r.Target]
			if r.Kind != facts.RelImports || len(ops) == 0 {
				continue
			}
			sender := ""
			if comp := factpath.Dir(f.File) + "." + fileSymbolName(f.File); isVueFile(f.File) && symbols[comp] {
				sender = comp
			} else if fileRefs[f.File] {
				sender = f.File
			}
			if sender != "" {
				sends[sender] = append(sends[sender], ops...)
			}
		}
	}
	for i := range all {
		f := &all[i]
		ops := sends[f.Name]
		if (f.Kind != facts.KindSymbol && f.Kind != facts.KindFileRef) || len(ops) == 0 {
			continue
		}
		sort.Strings(ops)
		for _, op := range ops {
			if !f.HasRelation(facts.RelCalls, op) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: op})
			}
		}
		delete(sends, f.Name)
	}
}

// isModuleScopeDeclarator reports whether a variable_declarator is declared at the
// top of the module, directly or exported.
func isModuleScopeDeclarator(kinds *tsutil.KindTable, d *sitter.Node) bool {
	return d.Parent() != nil && isModuleScopeStatement(kinds, d.Parent())
}

// isModuleScopeStatement reports whether a declaration statement sits at the top of
// the module, directly or exported.
func isModuleScopeStatement(kinds *tsutil.KindTable, stmt *sitter.Node) bool {
	switch outer := stmt.Parent(); {
	case outer == nil:
		return false
	case kindOf(kinds, outer) == "program":
		return true
	case kindOf(kinds, outer) == "export_statement":
		return outer.Parent() != nil && kindOf(kinds, outer.Parent()) == "program"
	}
	return false
}
