package tsextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
)

// Calls on a member of a typed value, where the member's type is declared on the
// value's type, often in another file.
//
//	export type DragCallback = (props: { scene: Scene; … }) => void;   // in one file
//
//	const onDrag: DragCallback = ({ scene }) => {                      // in another
//	    scene.triggerUpdate();                  // Scene.triggerUpdate
//	};
//	function open(app: AppProps) { app.scene.triggerUpdate(); }       // the same
//
// A destructured parameter took no type, and `app.scene` was typed only for `this`.
// The type is not knowable from one file: the walker writes the call down against a
// deferred type, "<type>::<member>" ("<alias>::(0)::<member>" for the first
// parameter of a function-type alias), and resolveMemberTypedCalls evaluates it
// against the member types the declarations recorded. A call it cannot place on a
// declared method is dropped: a missing edge beats a wrong one.

const (
	// memberSep separates a deferred type's steps. TypeScript names never hold it.
	memberSep = "::"
	// param0Step is the step to a function type's first parameter.
	param0Step = "(0)"
	// propMemberTypes holds, on an interface, type alias or class, the declared
	// type of each member; propParam0Members the same for the object type a
	// function-type alias takes as its first parameter, and propParam0Type the type
	// it names there instead. None leaves the extractor.
	propMemberTypes   = "ts_member_types"
	propParam0Members = "ts_param0_members"
	propParam0Type    = "ts_param0_type"
)

// objectMemberTypes returns the declared type of each property of an object type or
// interface body, qualified as a local's type is.
func objectMemberTypes(kinds *tsutil.KindTable, body *sitter.Node, src []byte, dir string, importMap map[string]string, declared map[string]bool) map[string]string {
	if body == nil {
		return nil
	}
	out := map[string]string{}
	for i := range body.NamedChildCount() {
		m := body.NamedChild(i)
		if kindOf(kinds, m) != "property_signature" {
			continue
		}
		name := m.ChildByFieldName("name")
		if name == nil || kindOf(kinds, name) != "property_identifier" {
			continue
		}
		if q := qualifyTSType(annotatedTypeName(kinds, m.ChildByFieldName("type"), src), dir, importMap, declared); q != "" {
			out[nodeText(name, src)] = q
		}
	}
	return out
}

// recordMemberTypes writes onto an interface or type alias fact the member types
// resolveMemberTypedCalls reads.
func recordMemberTypes(f *facts.Fact, kinds *tsutil.KindTable, decl *sitter.Node, ctx *extractCtx) {
	if ctx.typeNames == nil {
		ctx.typeNames = &typeNameCache{}
	}
	declared := ctx.typeNames.of(kinds, decl, ctx.src)
	switch kindOf(kinds, decl) {
	case "interface_declaration":
		if m := objectMemberTypes(kinds, decl.ChildByFieldName("body"), ctx.src, ctx.dir, ctx.importMap, declared); len(m) > 0 {
			f.SetProp(propMemberTypes, m)
		}
	case "type_alias_declaration":
		value := decl.ChildByFieldName("value")
		if value == nil {
			return
		}
		switch kindOf(kinds, value) {
		case "object_type":
			if m := objectMemberTypes(kinds, value, ctx.src, ctx.dir, ctx.importMap, declared); len(m) > 0 {
				f.SetProp(propMemberTypes, m)
			}
		case "function_type":
			params := value.ChildByFieldName("parameters")
			if params == nil || params.NamedChildCount() == 0 {
				return
			}
			ann := params.NamedChild(0).ChildByFieldName("type")
			if ann == nil {
				return
			}
			if obj := findChildByKind(kinds, ann, "object_type"); obj != nil {
				if m := objectMemberTypes(kinds, obj, ctx.src, ctx.dir, ctx.importMap, declared); len(m) > 0 {
					f.SetProp(propParam0Members, m)
				}
			} else if q := qualifyTSType(annotatedTypeName(kinds, ann, ctx.src), ctx.dir, ctx.importMap, declared); q != "" {
				f.SetProp(propParam0Type, q)
			}
		}
	}
}

// destructuredParamTypes returns the deferred type of each name a function binds by
// destructuring an object parameter: from the parameter's own annotation, or, for
// the first parameter of an arrow or function expression assigned to a variable
// annotated with a function-type alias, from that alias.
func destructuredParamTypes(kinds *tsutil.KindTable, fn *sitter.Node, src []byte, dir string, importMap map[string]string, declared map[string]bool) map[string]string {
	if fn == nil {
		return nil
	}
	params := fn.ChildByFieldName("parameters")
	if params == nil {
		return nil
	}
	out := map[string]string{}
	for i := range params.NamedChildCount() {
		p := params.NamedChild(i)
		pat := p.ChildByFieldName("pattern")
		if pat == nil || kindOf(kinds, pat) != "object_pattern" {
			continue
		}
		base := qualifyTSType(declaredTypeName(kinds, p, src), dir, importMap, declared)
		if base == "" && i == 0 {
			if alias := assignedAliasType(kinds, fn, src, dir, importMap, declared); alias != "" {
				base = alias + memberSep + param0Step
			}
		}
		if base == "" {
			continue
		}
		for j := range pat.NamedChildCount() {
			c := pat.NamedChild(j)
			switch kindOf(kinds, c) {
			case "shorthand_property_identifier_pattern":
				out[nodeText(c, src)] = base + memberSep + nodeText(c, src)
			case "pair_pattern":
				// `{ scene: s }` binds s to the member scene.
				key, value := c.ChildByFieldName("key"), c.ChildByFieldName("value")
				if key != nil && value != nil && kindOf(kinds, value) == "identifier" {
					out[nodeText(value, src)] = base + memberSep + nodeText(key, src)
				}
			}
		}
	}
	return out
}

// assignedAliasType is the type alias a function expression's variable is annotated
// with (`const f: DragCallback = (…) => …`), or "".
func assignedAliasType(kinds *tsutil.KindTable, fn *sitter.Node, src []byte, dir string, importMap map[string]string, declared map[string]bool) string {
	switch kindOf(kinds, fn) {
	case "arrow_function", "function_expression", "function":
	default:
		return ""
	}
	decl := fn.Parent()
	if decl == nil || kindOf(kinds, decl) != "variable_declarator" {
		return ""
	}
	return qualifyTSType(declaredTypeName(kinds, decl, src), dir, importMap, declared)
}

// resolveMemberTypedCalls evaluates the deferred calls the walker wrote and keeps the
// ones that land on a method the repository declares.
func resolveMemberTypedCalls(all []facts.Fact) {
	exists := make(map[string]bool, len(all))
	members := make(map[string]map[string]string)
	param0 := make(map[string]map[string]string)
	param0Type := make(map[string]string)
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		exists[f.Name] = true
		if m, ok := f.PropAny(propMemberTypes).(map[string]string); ok {
			members[f.Name] = m
			f.DelProp(propMemberTypes)
		}
		if m, ok := f.PropAny(propParam0Members).(map[string]string); ok {
			param0[f.Name] = m
			f.DelProp(propParam0Members)
		}
		if t, ok := f.PropAny(propParam0Type).(string); ok {
			param0Type[f.Name] = t
			f.DelProp(propParam0Type)
		}
	}
	// resolve turns "<type>::<step>….<method>" into the method it names, or "".
	resolve := func(target string) string {
		dot := strings.LastIndexByte(target, '.')
		if dot < 0 {
			return ""
		}
		steps := strings.Split(target[:dot], memberSep)
		t := steps[0]
		inParam0 := false
		for _, s := range steps[1:] {
			switch {
			case s == param0Step:
				if pt, ok := param0Type[t]; ok {
					t = pt
				} else {
					inParam0 = true
				}
			case inParam0:
				t, inParam0 = param0[t][s], false
			default:
				t = members[t][s]
			}
			if t == "" {
				return ""
			}
		}
		if m := t + target[dot:]; !inParam0 && exists[m] {
			return m
		}
		return ""
	}
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		kept := f.Relations[:0]
		for _, r := range f.Relations {
			if strings.Contains(r.Target, memberSep) {
				to := resolve(r.Target)
				if to == "" || f.HasRelation(r.Kind, to) {
					continue
				}
				r.Target = to
			}
			kept = append(kept, r)
		}
		f.Relations = kept
		for _, v := range f.Props {
			list, ok := v.([]string)
			if !ok {
				continue
			}
			// The loop lists run parallel to their depth lists, so an entry that
			// does not resolve keeps its place, as the method's bare name.
			for j, c := range list {
				if strings.Contains(c, memberSep) {
					if to := resolve(c); to != "" {
						list[j] = to
					} else {
						list[j] = c[strings.LastIndexByte(c, '.')+1:]
					}
				}
			}
		}
	}
}
