package dartextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

type dartValueType struct {
	Name, File string
	Namespace  bool
	Owner      *facts.Fact
}
type dartValueCacheKey struct {
	From       *facts.Fact
	Expression string
}
type dartDeclaredValue struct {
	dartValueDeclaration
	File string
}
type dartGlobalValueKey struct{ Library, Name string }

func dartBaseType(name string) string {
	name = strings.TrimSpace(strings.TrimSuffix(name, "?"))
	if cut := strings.IndexByte(name, '<'); cut >= 0 {
		name = name[:cut]
	}
	return strings.TrimSpace(name)
}
func (r *dartMemberResolver) valueOwner(typ dartValueType) *facts.Fact {
	if typ.Owner != nil {
		return typ.Owner
	}
	return r.typeNamed(dartBaseType(typ.Name), typ.File)
}
func (r *dartMemberResolver) declarationContext(owner, file string) *facts.Fact {
	return &facts.Fact{Name: owner, File: file}
}

func (r *dartMemberResolver) initializeValues(all []facts.Fact) {
	r.globalValues = map[dartGlobalValueKey][]dartDeclaredValue{}
	r.importedValueCache = map[dartImportedFunctionKey][]dartDeclaredValue{}
	r.exportedValueCache = map[dartTypeKey][]dartDeclaredValue{}
	r.fieldValues = map[dartMemberKey][]dartDeclaredValue{}
	for i := range all {
		f := &all[i]
		decls := dartValueDeclarations(f.Props["dart_values"])
		for _, decl := range decls {
			value := dartDeclaredValue{decl, f.File}
			if decl.Owner == "" {
				key := dartGlobalValueKey{r.libraryFile(f.File), decl.Name}
				r.globalValues[key] = append(r.globalValues[key], value)
			} else {
				owner := r.lexicalOwner(r.declarationContext(decl.Owner, f.File))
				if owner == nil {
					continue
				}
				key := dartMemberKey{owner, decl.Name}
				r.fieldValues[key] = append(r.fieldValues[key], value)
			}
		}
	}
}
func (r *dartMemberResolver) declaredValueType(v dartDeclaredValue, seen map[string]bool, sub *dartSubst) dartValueType {
	if v.Type == "dynamic" {
		return dartValueType{}
	}
	if sub != nil && v.Type != "" {
		if typ, ok := r.substituteType(v.Type, v.File, sub); ok {
			return typ
		}
		return dartValueType{}
	}
	if r.typeHasParameter(v.Type, r.declarationContext(v.Owner, v.File)) {
		return dartValueType{}
	}
	if v.Type != "" {
		return dartValueType{Name: v.Type, File: v.File}
	}
	return r.expressionType(v.Expression, r.declarationContext(v.Owner, v.File), nil, seen)
}
func (r *dartMemberResolver) resultType(f *facts.Fact, seen map[string]bool, sub *dartSubst) dartValueType {
	if f == nil {
		return dartValueType{}
	}
	key := "return:" + f.File + "|" + f.Name
	if seen[key] {
		return dartValueType{}
	}
	seen[key] = true
	defer delete(seen, key)
	if f.PropBool("constructor") {
		if owner := r.lexicalOwner(f); owner != nil {
			return dartValueType{Name: owner.Name, File: owner.File, Owner: owner}
		}
	}
	if typ := f.PropString("return_type"); typ != "" {
		if sub != nil && typ != "dynamic" && typ != "void" && r.lexicalOwner(f) == sub.Owner {
			if own, _ := f.Props["type_parameters"].([]string); !dartMentionsAny(typ, own) {
				if t, ok := r.substituteType(typ, f.File, sub); ok {
					return t
				}
			}
			return dartValueType{}
		}
		if typ == "dynamic" || typ == "void" || r.typeHasParameter(typ, f) {
			return dartValueType{}
		}
		return dartValueType{Name: typ, File: f.File}
	}
	if owner := r.lexicalOwner(f); owner != nil && !f.PropBool("static") {
		name := f.Name[strings.LastIndexByte(f.Name, '.')+1:]
		if parent, _ := r.parentMember(owner, name, map[*facts.Fact]bool{}); parent != nil {
			return r.resultType(parent, seen, nil)
		}
	}
	return r.expressionType(f.PropString("return_expression"), f, nil, seen)
}
func (r *dartMemberResolver) fieldType(owner *facts.Fact, name string, namespace bool, seen map[string]bool, visited map[*facts.Fact]bool, sub *dartSubst) dartValueType {
	if owner == nil || visited[owner] {
		return dartValueType{}
	}
	visited[owner] = true
	if sub != nil && sub.Owner != owner {
		sub = nil
	}
	if values, ok := r.fieldValues[dartMemberKey{owner, name}]; ok {
		if len(values) != 1 || (namespace && !values[0].Static) {
			return dartValueType{}
		}
		return r.declaredValueType(values[0], seen, sub)
	}
	if entry, ok := r.members[owner.Name+"."+name]; ok && (entry.fact == nil || r.sameDartLibrary(entry.fact, owner)) {
		if f := entry.readable(); f != nil && f.PropString("accessor") == "getter" && (!namespace || f.PropBool("static")) {
			return r.resultType(f, seen, sub)
		}
		// An own callable is a tear-off, not a base field.
		if entry.readable() != nil {
			return dartValueType{}
		}
	}
	mixins, _ := owner.Props["mixin_types"].([]string)
	for i := len(mixins) - 1; i >= 0; i-- {
		if t := r.fieldType(r.typeNamed(mixins[i], owner.File), name, namespace, seen, visited, nil); t.Name != "" {
			return t
		}
	}
	return r.fieldType(r.typeNamed(owner.PropString("extends_type"), owner.File), name, namespace, seen, visited, nil)
}
func (r *dartMemberResolver) exportedValues(file, name string, seen map[string]bool) []dartDeclaredValue {
	file = r.libraryFile(file)
	if seen[file] || strings.HasPrefix(name, "_") {
		return nil
	}
	seen[file] = true
	if own := r.globalValues[dartGlobalValueKey{file, name}]; len(own) > 0 {
		return own
	}
	var out []dartDeclaredValue
	for _, dep := range r.imports[file] {
		if dep.PropBool("reexport") && dartImportAllows(dep, name) {
			for _, target := range r.importTargets(dep) {
				out = append(out, r.exportedValues(target, name, seen)...)
			}
		}
	}
	return out
}
func uniqueDartValue(values []dartDeclaredValue) (dartDeclaredValue, bool) {
	if len(values) == 0 {
		return dartDeclaredValue{}, false
	}
	first := values[0]
	for _, v := range values[1:] {
		if v.File != first.File || v.Owner != first.Owner || v.Name != first.Name {
			return dartDeclaredValue{}, false
		}
	}
	return first, true
}
func (r *dartMemberResolver) importedValues(prefix, name, file string) []dartDeclaredValue {
	key := dartImportedFunctionKey{prefix, name, r.libraryFile(file)}
	if cached, ok := r.importedValueCache[key]; ok {
		return cached
	}
	var out []dartDeclaredValue
	for _, dep := range r.imports[r.libraryFile(file)] {
		if !dep.PropBool("reexport") && dep.PropString("import_prefix") == prefix && dartImportAllows(dep, name) {
			for _, target := range r.importTargets(dep) {
				out = append(out, r.exportedValueFacts(target, name)...)
			}
		}
	}
	hasSDK, hasOther := false, false
	for _, v := range out {
		if r.sdkHosts[r.libraryFile(v.File)] {
			hasSDK = true
		} else {
			hasOther = true
		}
	}
	if hasSDK && hasOther {
		filtered := make([]dartDeclaredValue, 0, len(out))
		for _, v := range out {
			if !r.sdkHosts[r.libraryFile(v.File)] {
				filtered = append(filtered, v)
			}
		}
		out = filtered
	}
	r.importedValueCache[key] = out
	return out
}
func (r *dartMemberResolver) globalValueType(prefix, name string, from *facts.Fact, seen map[string]bool) dartValueType {
	if prefix == "" {
		own := r.globalValues[dartGlobalValueKey{r.libraryFile(from.File), name}]
		if len(own) > 0 {
			if v, ok := uniqueDartValue(own); ok {
				return r.declaredValueType(v, seen, nil)
			}
			return dartValueType{}
		}
		for _, f := range r.functions[name] {
			if r.libraryFile(f.File) == r.libraryFile(from.File) && f.PropString("accessor") == "getter" {
				return r.resultType(f, seen, nil)
			}
		}
	}
	if v, ok := uniqueDartValue(r.importedValues(prefix, name, from.File)); ok {
		return r.declaredValueType(v, seen, nil)
	}
	if f := uniqueDartFact(r.importedFunctions(prefix, name, from.File)); f != nil && f.PropString("accessor") == "getter" {
		return r.resultType(f, seen, nil)
	}
	return dartValueType{}
}

// Consume normalized selectors; type arguments in expressions were erased by
// the AST walker, while written declaration types retain their generic content.
func dartExpressionParts(expr string) []string {
	var parts []string
	start, depth := 0, 0
	for i, c := range expr {
		switch c {
		case '{':
			depth++
		case '}':
			depth--
		case '.':
			if depth == 0 {
				parts = append(parts, expr[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, expr[start:])
}
func (r *dartMemberResolver) expressionType(expr string, from *facts.Fact, locals map[string]dartLocalValue, seen map[string]bool) dartValueType {
	if expr == "" || len(seen) > 64 {
		return dartValueType{}
	}
	key := from.File + "|" + from.Name + "|" + expr
	if seen[key] {
		return dartValueType{}
	}
	seen[key] = true
	defer delete(seen, key)
	if strings.HasPrefix(expr, "await ") {
		t := r.expressionType(strings.TrimPrefix(expr, "await "), from, locals, seen)
		base := dartBaseType(t.Name)
		short := base[strings.LastIndexByte(base, '.')+1:]
		if (short == "Future" || short == "FutureOr") && r.isAsyncFutureType(t, base) {
			if start := strings.IndexByte(t.Name, '<'); start >= 0 && strings.HasSuffix(strings.TrimSpace(t.Name), ">") {
				t.Name = strings.TrimSpace(t.Name[start+1 : len(strings.TrimSpace(t.Name))-1])
				t.Owner = nil
				return t
			}
		}

		return t
	}
	parts := dartExpressionParts(expr)
	root := parts[0]
	rest := parts[1:]
	var typ dartValueType
	if strings.HasPrefix(root, "@value:") {
		binding, ok := locals[root]
		if !ok {
			return typ
		}
		typ = r.expressionType(binding.Expression, from, locals, seen)
	} else if strings.HasPrefix(root, "@type{") && strings.HasSuffix(root, "}") {
		if r.typeHasParameter(root[6:len(root)-1], from) {
			return dartValueType{}
		}
		typ = dartValueType{Name: root[6 : len(root)-1], File: from.File}
	} else if strings.HasPrefix(root, "@unknown") || strings.HasPrefix(root, "(") {
		return typ
	} else if root == "this" || root == "super" {
		owner := r.lexicalOwner(from)
		if root == "super" && owner != nil {
			owner = r.typeNamed(owner.PropString("extends_type"), owner.File)
		}
		if owner != nil {
			typ = dartValueType{Name: owner.Name, File: owner.File, Owner: owner}
		}
	} else {
		construct := strings.HasPrefix(root, "new ") || strings.HasPrefix(root, "const ")
		root = strings.TrimPrefix(strings.TrimPrefix(root, "new "), "const ")
		call := strings.HasSuffix(root, "()")
		name := strings.TrimSuffix(root, "()")
		if call {
			typ = r.bareCallType(name, construct, from, seen)

		} else {
			if owner := r.lexicalOwner(from); owner != nil {
				typ = r.fieldType(owner, name, false, seen, map[*facts.Fact]bool{}, nil)
			}
			if typ.Name == "" {
				typ = r.globalValueType("", name, from, seen)
			}
			if typ.Name == "" {
				owner := r.lexicalOwner(from)
				_, memberBound := r.lookupMember(owner, name)
				if memberBound || r.hasGlobalValueName(name, from) || r.hasValueField(owner, name, map[*facts.Fact]bool{}) {
					return dartValueType{}
				}
				if owner := r.typeNamed(name, from.File); owner != nil {
					typ = dartValueType{Name: owner.Name, File: owner.File, Namespace: true, Owner: owner}
				}
			}
			if typ.Name == "" && len(rest) > 0 {
				// An import prefix can name a class, function, or global value.
				next := rest[0]
				called := strings.HasSuffix(next, "()")
				member := strings.TrimSuffix(next, "()")
				if owner := r.typeNamed(name+"."+member, from.File); owner != nil {
					typ = dartValueType{Name: owner.Name, File: owner.File, Namespace: !called, Owner: owner}
				} else if called {
					typ = r.resultType(uniqueDartFact(r.importedFunctions(name, member, from.File)), seen, nil)
				} else {
					typ = r.globalValueType(name, member, from, seen)
				}
				if typ.Name != "" {
					rest = rest[1:]
				}
			}
		}
	}
	for _, step := range rest {
		if typ.Name == "" {
			return typ
		}
		call := strings.HasSuffix(step, "()")
		name := strings.TrimSuffix(step, "()")
		owner := r.valueOwner(typ)
		if call {
			if owner == nil {
				return dartValueType{}
			}
			if typ.Namespace {
				ctor := r.members[owner.Name+"."+owner.Name[strings.LastIndexByte(owner.Name, '.')+1:]+"."+name].readable()
				if ctor != nil && ctor.PropBool("constructor") {
					typ = r.resultType(ctor, seen, nil)
					continue
				}
			}
			f, found := r.lookupMember(owner, name)
			if !found || f == nil || (typ.Namespace && !f.PropBool("static")) {
				return dartValueType{}
			}
			typ = r.resultType(f, seen, r.receiverSubst(typ, owner))
		} else {
			typ = r.fieldType(owner, name, typ.Namespace, seen, map[*facts.Fact]bool{}, r.receiverSubst(typ, owner))
		}
	}
	return typ
}
func (r *dartMemberResolver) valueCall(receiver, member string, from *facts.Fact) string {
	key := dartValueCacheKey{from, receiver}
	typ, ok := r.valueTypeCache[key]
	if !ok {
		typ = r.expressionType(receiver, from, r.currentValues, map[string]bool{})
		r.valueTypeCache[key] = typ
	}
	if typ.Name == "" || typ.Namespace {
		return ""
	}
	owner := r.valueOwner(typ)
	if f, found := r.lookupMember(owner, member); found {
		if f != nil {
			return f.Name
		}
		return ""
	}
	if ext := r.extensionMethod(owner, dartBaseType(typ.Name), member, from.File); ext != nil {
		return ext.Name
	}

	return ""
}
func (r *dartMemberResolver) displayValueTarget(target string) string {
	for id, binding := range r.currentValues {
		if strings.HasPrefix(target, id) && len(target) > len(id) && strings.ContainsRune(".([", rune(target[len(id)])) {
			return "(" + binding.Name + ")" + target[len(id):]
		}
	}
	if strings.HasPrefix(target, "@expr:") {
		value := strings.TrimPrefix(target, "@expr:")
		end := strings.IndexAny(value, ".(")
		if end < 0 {
			return value
		}
		return "(" + value[:end] + ")" + value[end:]
	}
	if strings.HasPrefix(target, "@type{") {
		if end := strings.IndexByte(target, '}'); end >= 0 {
			return target[6:end] + target[end+1:]
		}
	}
	return target
}

func (r *dartMemberResolver) typeHasParameter(typ string, from *facts.Fact) bool {
	if typ == "" {
		return false
	}
	params, _ := from.Props["type_parameters"].([]string)
	if owner := r.lexicalOwner(from); owner != nil {
		own, _ := owner.Props["type_parameters"].([]string)
		params = append(append([]string{}, params...), own...)
	}
	if len(params) == 0 {
		return false
	}
	for _, word := range strings.FieldsFunc(typ, func(c rune) bool {
		return !(c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')
	}) {
		for _, param := range params {
			if word == param {
				return true
			}
		}
	}
	return false
}

func (r *dartMemberResolver) hasValueField(owner *facts.Fact, name string, seen map[*facts.Fact]bool) bool {
	if owner == nil || seen[owner] {
		return false
	}
	seen[owner] = true
	if _, ok := r.fieldValues[dartMemberKey{owner, name}]; ok {
		return true
	}
	mixins, _ := owner.Props["mixin_types"].([]string)
	for i := len(mixins) - 1; i >= 0; i-- {
		if r.hasValueField(r.typeNamed(mixins[i], owner.File), name, seen) {
			return true
		}
	}
	return r.hasValueField(r.typeNamed(owner.PropString("extends_type"), owner.File), name, seen)
}
func (r *dartMemberResolver) callableValueType(typ dartValueType, seen map[string]bool) dartValueType {
	if owner := r.valueOwner(typ); owner != nil {
		if f, found := r.lookupMember(owner, "call"); found && f != nil && f.PropString("accessor") == "" {
			return r.resultType(f, seen, nil)
		}
	}
	return dartValueType{}
}
func (r *dartMemberResolver) bareCallType(name string, construct bool, from *facts.Fact, seen map[string]bool) dartValueType {
	if !construct {
		if owner := r.lexicalOwner(from); owner != nil {
			if r.hasValueField(owner, name, map[*facts.Fact]bool{}) {
				return r.callableValueType(r.fieldType(owner, name, false, seen, map[*facts.Fact]bool{}, nil), seen)
			}
			if f, found := r.lookupMember(owner, name); found {
				if f != nil && f.PropString("accessor") == "getter" {
					return r.callableValueType(r.resultType(f, seen, nil), seen)
				}
				return r.resultType(f, seen, nil)
			}
		}
		if values := r.globalValues[dartGlobalValueKey{r.libraryFile(from.File), name}]; len(values) > 0 {
			if v, ok := uniqueDartValue(values); ok {
				return r.callableValueType(r.declaredValueType(v, seen, nil), seen)
			}
			return dartValueType{}
		}
		for _, f := range r.functions[name] {
			if r.libraryFile(f.File) == r.libraryFile(from.File) {
				if f.PropString("accessor") == "getter" {
					return r.callableValueType(r.resultType(f, seen, nil), seen)
				}
				return r.resultType(f, seen, nil)
			}
		}
	}
	if owner := r.typeNamed(name, from.File); owner != nil {
		return dartValueType{Name: owner.Name, File: owner.File, Owner: owner}
	}
	if !construct {
		return r.resultType(uniqueDartFact(r.importedFunctions("", name, from.File)), seen, nil)
	}
	return dartValueType{}
}

func dartValueDeclarations(value any) []dartValueDeclaration {
	if typed, ok := value.([]dartValueDeclaration); ok {
		return typed
	}
	values, _ := value.([]any)
	out := make([]dartValueDeclaration, 0, len(values))
	for _, value := range values {
		m, ok := value.(map[string]any)
		if !ok {
			continue
		}
		decl := dartValueDeclaration{}
		decl.Owner, _ = m["owner"].(string)
		decl.Name, _ = m["name"].(string)
		decl.Type, _ = m["type"].(string)
		decl.Expression, _ = m["expression"].(string)
		decl.Static, _ = m["static"].(bool)
		if decl.Name != "" {
			out = append(out, decl)
		}
	}
	return out
}

func (r *dartMemberResolver) isAsyncFutureType(t dartValueType, base string) bool {
	if owner := r.valueOwner(t); owner != nil {
		return r.sdkLibraryHosts["dart:async"][r.libraryFile(owner.File)]
	}
	prefix, _, qualified := strings.Cut(base, ".")
	if !qualified {
		return true
	}
	for _, dep := range r.imports[r.libraryFile(t.File)] {
		if dep.PropString("uri") == "dart:async" && dep.PropString("import_prefix") == prefix {
			return true
		}
	}
	return false
}

func (r *dartMemberResolver) exportedValueFacts(file, name string) []dartDeclaredValue {
	key := dartTypeKey{name, r.libraryFile(file)}
	if cached, ok := r.exportedValueCache[key]; ok {
		return cached
	}
	out := r.exportedValues(file, name, map[string]bool{})
	r.exportedValueCache[key] = out
	return out
}
func (r *dartMemberResolver) hasGlobalValueName(name string, from *facts.Fact) bool {
	if len(r.globalValues[dartGlobalValueKey{r.libraryFile(from.File), name}]) > 0 {
		return true
	}
	for _, f := range r.functions[name] {
		if r.libraryFile(f.File) == r.libraryFile(from.File) {
			return true
		}
	}
	return len(r.importedValues("", name, from.File)) > 0 || len(r.importedFunctions("", name, from.File)) > 0
}
