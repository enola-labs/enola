package dartextractor

import (
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

func (w *walker) inheritedTypeNames(n *sitter.Node) []string {
	var names []string
	var end uint
	for _, c := range namedChildren(n) {
		if kindOf(c) != "type_identifier" {
			continue
		}
		name := c.Utf8Text(w.src)
		if len(names) > 0 && strings.TrimSpace(string(w.src[end:c.StartByte()])) == "." {
			names[len(names)-1] += "." + name
		} else {
			names = append(names, name)
		}
		end = c.EndByte()
	}
	return names
}

// A getter/setter pair is one readable member, not two competing libraries.
// Other duplicate declarations remain ambiguous, including same names in files
// selected by different conditional imports.
func dartReadablePair(a, b *facts.Fact) *facts.Fact {
	if a == nil || b == nil || a.File != b.File || a.Repo != b.Repo || a.Name != b.Name {
		return nil
	}
	if a.PropString("accessor") == "getter" && b.PropString("accessor") == "setter" {
		return a
	}
	if a.PropString("accessor") == "setter" && b.PropString("accessor") == "getter" {
		return b
	}
	return nil
}

type dartMemberEntry struct {
	fact  *facts.Fact
	count int
}

func (e dartMemberEntry) readable() *facts.Fact {
	if e.fact != nil && e.fact.PropString("accessor") != "setter" {
		return e.fact
	}
	return nil
}

func (e dartMemberEntry) add(f *facts.Fact) dartMemberEntry {
	switch e.count {
	case 0:
		e.fact = f
	case 1:
		e.fact = dartReadablePair(e.fact, f)
	default:
		e.fact = nil
	}
	e.count++
	return e
}

type dartTypeKey struct{ Name, File string }
type dartMemberKey struct {
	Owner *facts.Fact
	Name  string
}
type dartMemberResult struct {
	Fact  *facts.Fact
	Found bool
}

type dartExtensionReceiver struct {
	Owner         *facts.Fact
	Type, Library string
}

type dartImportedFunctionKey struct{ Prefix, Name, Library string }

type dartMemberResolver struct {
	canonicalTypes        map[string][]*facts.Fact
	members               map[string]dartMemberEntry
	functions             map[string][]*facts.Fact
	imports               map[string][]*facts.Fact
	fileTypes             map[string]map[string][]*facts.Fact
	library               map[string]string
	libraryFiles          map[string][]string
	typeCache             map[dartTypeKey]*facts.Fact
	memberCache           map[dartMemberKey]dartMemberResult
	extensions            map[string][]*facts.Fact
	extensionCache        map[dartTypeKey]*facts.Fact
	applicableExtensions  map[dartExtensionReceiver][]*facts.Fact
	importFiles           map[*facts.Fact][]string
	packageRoots          map[string]map[string]bool
	exportedTypeCache     map[dartTypeKey][]*facts.Fact
	exportedFunctionCache map[dartTypeKey][]*facts.Fact
	importedFunctionCache map[dartImportedFunctionKey][]*facts.Fact
	globalValues          map[dartGlobalValueKey][]dartDeclaredValue
	fieldValues           map[dartMemberKey][]dartDeclaredValue
	currentValues         map[string]dartLocalValue
	lexicalOwners         map[dartTypeKey]*facts.Fact
	valueTypeCache        map[dartValueCacheKey]dartValueType
	sdkLibraryHosts       map[string]map[string]bool
	sdkHosts              map[string]bool
	enumConstants         map[string]bool
	importedValueCache    map[dartImportedFunctionKey][]dartDeclaredValue
	exportedValueCache    map[dartTypeKey][]dartDeclaredValue
}

func newDartMemberResolver(all []facts.Fact) *dartMemberResolver {
	r := &dartMemberResolver{canonicalTypes: map[string][]*facts.Fact{}, members: map[string]dartMemberEntry{}, functions: map[string][]*facts.Fact{}, imports: map[string][]*facts.Fact{}, fileTypes: map[string]map[string][]*facts.Fact{}}
	r.sdkLibraryHosts = map[string]map[string]bool{}
	r.sdkHosts = map[string]bool{}
	r.enumConstants = map[string]bool{}
	r.exportedTypeCache = map[dartTypeKey][]*facts.Fact{}
	r.exportedFunctionCache = map[dartTypeKey][]*facts.Fact{}
	r.importedFunctionCache = map[dartImportedFunctionKey][]*facts.Fact{}
	r.importFiles = map[*facts.Fact][]string{}
	r.packageRoots = map[string]map[string]bool{}
	r.extensions = map[string][]*facts.Fact{}
	r.extensionCache = map[dartTypeKey]*facts.Fact{}
	r.applicableExtensions = map[dartExtensionReceiver][]*facts.Fact{}
	r.library = map[string]string{}
	r.libraryFiles = map[string][]string{}
	files := map[string]bool{}
	for i := range all {
		f := &all[i]
		files[f.File] = true
		if pkg := f.PropString("pub_package"); pkg != "" {
			path := "/" + f.File
			if cut := strings.LastIndex(path, "/lib/"); cut >= 0 {
				root := strings.TrimPrefix(path[:cut], "/")
				if r.packageRoots[pkg] == nil {
					r.packageRoots[pkg] = map[string]bool{}
				}
				r.packageRoots[pkg][root] = true
			}
		}
		if host := f.PropString("library_file"); host != "" {
			r.library[f.File] = host
		}
		if parts, ok := f.Props["library_parts"].([]string); ok {
			for _, part := range parts {
				r.library[part] = f.File
			}
		}
	}
	// Resolve each parent chain once. After construction library is a direct,
	// immutable file-to-host index, including identity entries for library files.
	parents := r.library
	r.library = map[string]string{}
	for file, parent := range parents {
		files[file] = true
		files[parent] = true
	}
	for file := range files {
		resolveDartLibraryHost(file, parents, r.library)
	}
	for file := range files {
		host := r.libraryFile(file)
		r.libraryFiles[host] = append(r.libraryFiles[host], file)
	}
	for i := range all {
		f := &all[i]
		if f.Kind == facts.KindDependency && f.PropString("uri") != "" {
			if uri, path := f.PropString("uri"), f.PropString("resolved_file"); strings.HasPrefix(uri, "dart:") && path != "" {
				if r.sdkLibraryHosts[uri] == nil {
					r.sdkLibraryHosts[uri] = map[string]bool{}
				}
				r.sdkLibraryHosts[uri][r.libraryFile(path)] = true
				r.sdkHosts[r.libraryFile(path)] = true
			}
			host := r.libraryFile(f.File)
			r.imports[host] = append(r.imports[host], f)
		}
		if f.Kind != facts.KindSymbol {
			continue
		}
		short := f.Name[strings.LastIndexByte(f.Name, '.')+1:]
		if f.PropBool("enum_constant") {
			r.enumConstants[f.Name] = true
		}
		if on := f.PropString("extension_on"); on != "" {
			key := on[strings.LastIndexByte(on, '.')+1:]
			r.extensions[key] = append(r.extensions[key], f)
		}
		switch f.PropString("symbol_kind") {
		case facts.SymbolMethod, facts.SymbolFunc:
			r.members[f.Name] = r.members[f.Name].add(f)
			if f.PropString("symbol_kind") == facts.SymbolFunc {
				r.functions[short] = append(r.functions[short], f)
			}
		case facts.SymbolClass, facts.SymbolInterface, facts.SymbolEnum, facts.SymbolType:
			r.canonicalTypes[f.Name] = append(r.canonicalTypes[f.Name], f)
			if r.fileTypes[f.File] == nil {
				r.fileTypes[f.File] = map[string][]*facts.Fact{}
			}
			r.fileTypes[f.File][short] = append(r.fileTypes[f.File][short], f)
		}
	}
	for host, deps := range r.imports {
		explicitCore := false
		for _, dep := range deps {
			if dep.PropString("uri") == "dart:core" && !dep.PropBool("implicit") {
				explicitCore = true
			}
		}
		filtered := deps[:0]
		implicitCore := false
		for _, dep := range deps {
			if dep.PropString("uri") == "dart:core" && dep.PropBool("implicit") {
				if explicitCore || implicitCore {
					continue
				}
				implicitCore = true
			}
			filtered = append(filtered, dep)
		}
		r.imports[host] = filtered
	}
	r.lexicalOwners = map[dartTypeKey]*facts.Fact{}
	r.valueTypeCache = map[dartValueCacheKey]dartValueType{}
	r.initializeValues(all)
	return r
}

func uniqueDartFact(xs []*facts.Fact) *facts.Fact {
	if len(xs) == 0 {
		return nil
	}
	first := xs[0]
	for _, f := range xs[1:] {
		if f != first {
			return nil
		}
	}
	return first
}

func dartImportAllows(dep *facts.Fact, name string) bool {
	show, _ := dep.Props["show_names"].([]string)
	hide, _ := dep.Props["hide_names"].([]string)
	return !dep.PropBool("show_none") && (len(show) == 0 || slices.Contains(show, name)) && !slices.Contains(hide, name)
}

func (r *dartMemberResolver) exportedTypes(file, name string, seen map[string]bool) []*facts.Fact {
	if seen[file] {
		return nil
	}
	seen[file] = true
	file = r.libraryFile(file)
	var out []*facts.Fact
	for _, member := range r.libraryFiles[file] {
		out = append(out, r.fileTypes[member][name]...)
	}
	if strings.HasPrefix(name, "_") {
		return nil
	}
	if len(out) > 0 {
		return out
	}
	for _, dep := range r.imports[file] {
		if dep.PropBool("reexport") && dartImportAllows(dep, name) {
			for _, file := range r.importTargets(dep) {
				out = append(out, r.exportedTypes(file, name, seen)...)
			}
		}
	}
	return out
}

func (r *dartMemberResolver) typeNamed(name, file string) *facts.Fact {
	if r.typeCache == nil {
		r.typeCache = map[dartTypeKey]*facts.Fact{}
	}
	key := dartTypeKey{name, file}
	if value, ok := r.typeCache[key]; ok {
		return value
	}
	value := r.lookupType(name, file)
	r.typeCache[key] = value
	return value
}

func (r *dartMemberResolver) libraryFile(file string) string {
	if host, ok := r.library[file]; ok {
		return host
	}
	return file
}

func resolveDartLibraryHost(file string, parents, hosts map[string]string) {
	if _, ok := hosts[file]; ok {
		return
	}
	if parents[file] == "" {
		hosts[file] = file
		return
	}
	path := []string{}
	positions := map[string]int{}
	current := file
	for {
		if pos, cycle := positions[current]; cycle {
			// Preserve the old cycle guard: each cycle node resolves to itself, and
			// an incoming path resolves to the node where it enters the cycle.
			for _, node := range path[pos:] {
				hosts[node] = node
			}
			for _, node := range path[:pos] {
				hosts[node] = current
			}
			return
		}
		if host, ok := hosts[current]; ok {
			for _, node := range path {
				hosts[node] = host
			}
			return
		}
		if parents[current] == "" {
			hosts[current] = current
			for _, node := range path {
				hosts[node] = current
			}
			return
		}
		positions[current] = len(path)
		path = append(path, current)
		current = parents[current]
	}
}

func (r *dartMemberResolver) lookupType(name, file string) *facts.Fact {
	host := r.libraryFile(file)
	if prefix, short, ok := strings.Cut(name, "."); ok {
		var candidates []*facts.Fact
		prefixed := false
		for _, dep := range r.imports[host] {
			if !dep.PropBool("reexport") && dep.PropString("import_prefix") == prefix {
				prefixed = true
				if dartImportAllows(dep, short) {
					for _, file := range r.importTargets(dep) {
						candidates = append(candidates, r.exportedTypeFacts(file, short)...)
					}
				}
			}
		}
		if prefixed {
			return uniqueDartFact(r.preferNonSDKFacts(candidates))
		}
		// Canonical owners emitted for this/implicit calls are usable only inside
		// their own library. A directory name does not create a Dart namespace.
		for _, typ := range r.canonicalTypes[name] {
			if r.libraryFile(typ.File) == host {
				candidates = append(candidates, typ)
			}
		}
		return uniqueDartFact(candidates)
	}
	var local []*facts.Fact
	for _, member := range r.libraryFiles[host] {
		local = append(local, r.fileTypes[member][name]...)
	}
	if len(local) > 0 {
		return uniqueDartFact(local)
	}
	var imported, implicit []*facts.Fact
	for _, dep := range r.imports[host] {
		if !dep.PropBool("reexport") && dep.PropString("import_prefix") == "" && dartImportAllows(dep, name) {
			for _, file := range r.importTargets(dep) {
				if dep.PropBool("implicit") {
					implicit = append(implicit, r.exportedTypeFacts(file, name)...)
				} else {
					imported = append(imported, r.exportedTypeFacts(file, name)...)
				}
			}
		}
	}
	if len(imported) == 0 {
		imported = implicit
	}
	return uniqueDartFact(r.preferNonSDKFacts(imported))
}

func (r *dartMemberResolver) member(owner *facts.Fact, name string, seen map[*facts.Fact]bool) (*facts.Fact, bool) {
	if owner == nil || seen[owner] {
		return nil, false
	}
	seen[owner] = true
	if entry, ok := r.members[owner.Name+"."+name]; ok && (entry.fact == nil || r.sameDartLibrary(entry.fact, owner)) {
		if entry.fact == nil {
			return nil, true
		}
		if !entry.fact.PropBool("constructor") && entry.fact.PropString("accessor") != "setter" {
			return entry.fact, true
		}
	}
	return r.parentMember(owner, name, seen)
}

func (r *dartMemberResolver) parentMember(owner *facts.Fact, name string, seen map[*facts.Fact]bool) (*facts.Fact, bool) {
	if owner == nil {
		return nil, false
	}
	seen[owner] = true
	// Mixins override earlier mixins and the superclass, in declaration order.
	mixins, _ := owner.Props["mixin_types"].([]string)
	for i := len(mixins) - 1; i >= 0; i-- {
		if found, blocked := r.member(r.typeNamed(mixins[i], owner.File), name, seen); blocked {
			return found, true
		}
	}
	if parent := owner.PropString("extends_type"); parent != "" {
		if found, blocked := r.member(r.typeNamed(parent, owner.File), name, seen); blocked {
			return found, true
		}
	}
	return nil, false
}

func (r *dartMemberResolver) interfaceMember(owner *facts.Fact, name string) (*facts.Fact, bool) {
	if owner == nil {
		return nil, false
	}
	seen := map[*facts.Fact]bool{}
	// An abstract class may expose a callable solely through its interfaces.
	// Competing declarations remain ambiguous; never guess between contracts.
	interfaces, _ := owner.Props["interface_types"].([]string)
	var candidate *facts.Fact
	foundAny := false
	for _, iface := range interfaces {
		if found, blocked := r.member(r.typeNamed(iface, owner.File), name, seen); blocked {
			// Interfaces expose declarations, never an inherited implementation or its I/O.
			if found != nil && !found.PropBool("abstract_member") && !found.PropBool("abstract") {
				continue
			}
			if found == nil || (candidate != nil && candidate != found) {
				return nil, true
			}
			candidate, foundAny = found, true
		}
	}
	return candidate, foundAny
}

func (r *dartMemberResolver) lookupMember(owner *facts.Fact, name string) (*facts.Fact, bool) {
	if r.memberCache == nil {
		r.memberCache = map[dartMemberKey]dartMemberResult{}
	}
	key := dartMemberKey{owner, name}
	if cached, ok := r.memberCache[key]; ok {
		return cached.Fact, cached.Found
	}
	f, found := r.member(owner, name, map[*facts.Fact]bool{})
	if !found {
		f, found = r.interfaceMember(owner, name)
	}
	r.memberCache[key] = dartMemberResult{f, found}
	return f, found
}

func (r *dartMemberResolver) lexicalOwner(from *facts.Fact) *facts.Fact {
	key := dartTypeKey{from.Name, from.File}
	if f, ok := r.lexicalOwners[key]; ok {
		return f
	}
	if from.PropString("symbol_kind") == facts.SymbolFunc {
		r.lexicalOwners[key] = nil
		return nil
	}
	f := r.findLexicalOwner(from)
	r.lexicalOwners[key] = f
	return f
}
func (r *dartMemberResolver) findLexicalOwner(from *facts.Fact) *facts.Fact {
	name := from.Name
	for {
		if found := r.canonicalTypes[name]; len(found) > 0 {
			var local []*facts.Fact
			for _, f := range found {
				if f.File == from.File {
					local = append(local, f)
				}
			}
			if len(local) > 0 {
				return uniqueDartFact(local)
			}
			return uniqueDartFact(found)
		}
		dot := strings.LastIndexByte(name, '.')
		if dot < 0 {
			return nil
		}
		name = name[:dot]
	}
}

func (r *dartMemberResolver) call(target string, from *facts.Fact, implicit, instance bool, constructed map[string]*facts.Fact) string {
	if strings.HasPrefix(target, "@lexical:") {
		return strings.TrimPrefix(target, "@lexical:")
	}
	if entry, ok := r.members[target]; ok && implicit {
		if readable := entry.readable(); readable != nil {
			return readable.Name
		}
		return target
	}

	if !instance && !implicit && !strings.ContainsAny(target, "()@{}") && r.constructorNamespace(target, from.File) {
		if typ := r.typeNamed(target, from.File); typ != nil {
			if ctor := r.members[typ.Name+"."+typ.Name[strings.LastIndexByte(typ.Name, '.')+1:]].readable(); ctor != nil && ctor.PropBool("constructor") && r.sameDartLibrary(ctor, typ) {
				return ctor.Name
			}
		}
	}
	if dot := strings.LastIndexByte(target, '.'); dot >= 0 {
		ownerName, member := target[:dot], target[dot+1:]
		if ownerName == "super" {
			if found, _ := r.parentMember(r.lexicalOwner(from), member, map[*facts.Fact]bool{}); found != nil {
				return found.Name
			}
			return target
		}
		if instance && !strings.ContainsAny(ownerName, "@(){}") && r.typeHasParameter(ownerName, from) {
			return "(" + ownerName + ")." + member
		}
		var owner *facts.Fact
		if ownerName == "this" || implicit {
			owner = r.lexicalOwner(from)
		} else {
			owner = r.typeNamed(ownerName, from.File)
		}
		for _, prefix := range []string{"new ", "const "} {
			if strings.HasPrefix(ownerName, prefix) && strings.HasSuffix(ownerName, "()") {
				owner = r.typeNamed(strings.TrimSuffix(strings.TrimPrefix(ownerName, prefix), "()"), from.File)
				instance = true
			}
		}
		if owner == nil && constructed[ownerName] != nil {
			owner = constructed[ownerName]
			instance = true
		}
		if owner != nil {
			if implicit && member == owner.Name[strings.LastIndexByte(owner.Name, '.')+1:] {
				if ctor := r.members[owner.Name+"."+member].fact; ctor != nil && ctor.PropBool("constructor") && r.sameDartLibrary(ctor, owner) {
					return ctor.Name
				}
			}
			// An own constructor outranks inherited methods, but cannot coexist
			// with another own callable of the same name.
			ctor := r.members[owner.Name+"."+owner.Name[strings.LastIndexByte(owner.Name, '.')+1:]+"."+member]
			if !instance && ctor.count > 0 {
				existing := r.members[owner.Name+"."+member]
				if (existing.count > 0 && (existing.fact == nil || existing.fact.PropBool("static"))) || ctor.fact == nil {
					return target
				}
				if !r.sameDartLibrary(ctor.fact, owner) {
					return target
				}
				return ctor.fact.Name
			}
			if found, blocked := r.lookupMember(owner, member); blocked {
				if found != nil {
					return found.Name
				}
				return target
			}
		}
		if instance {
			if ext := r.extensionMethod(owner, ownerName, member, from.File); ext != nil {
				return ext.Name
			}
		}
		if !instance && !strings.Contains(ownerName, ".") {
			if fn := uniqueDartFact(r.importedFunctions(ownerName, member, from.File)); fn != nil {
				return fn.Name
			}
		}
		if value := r.valueCall(strings.TrimPrefix(ownerName, "@expr:"), member, from); value != "" {
			return value
		}
		if !implicit {
			if strings.HasPrefix(target, "@") {
				return r.displayValueTarget(target)
			}
			// Keep an unresolved spelling from accidentally acquiring the ID of an
			// unrelated canonical symbol when the store serializes the relations.
			if _, collision := r.members[target]; collision {
				return "(" + ownerName + ")." + member
			}
			return target
		}
		target = member
	}
	// A top-level function must belong to this library or a visible import.
	var local []*facts.Fact
	for _, f := range r.functions[target] {
		if r.libraryFile(f.File) == r.libraryFile(from.File) {
			local = append(local, f)
		}
	}
	if len(local) > 0 {
		if f := uniqueDartFact(local); f != nil {
			return f.Name
		}
		return target
	}
	if f := uniqueDartFact(r.importedFunctions("", target, from.File)); f != nil {
		return f.Name
	}

	return target
}

// Tree-sitter also uses constructor_signature for methods with an omitted
// return type. A bare name other than the class name is a method unless the
// concise constructor syntax supplies an explicit new/factory/const keyword.
func dartConstructorSignature(n *sitter.Node, typeName string, src []byte) bool {
	switch kindOf(n) {
	case "factory_constructor_signature", "constant_constructor_signature":
		return true
	case "constructor_signature":
		if name := childOfKind(n, "identifier"); name != nil && name.Utf8Text(src) == typeName {
			return true
		}
		return hasToken(n, "new") || hasToken(n, "factory") || hasToken(n, "const")
	}
	return false
}

func (r *dartMemberResolver) exportedFunctions(file, name string, seen map[string]bool) []*facts.Fact {
	file = r.libraryFile(file)
	if seen[file] || strings.HasPrefix(name, "_") {
		return nil
	}
	seen[file] = true
	var out []*facts.Fact
	for _, f := range r.functions[name] {
		if r.libraryFile(f.File) == file {
			out = append(out, f)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, dep := range r.imports[file] {
		if dep.PropBool("reexport") && dartImportAllows(dep, name) {
			for _, target := range r.importTargets(dep) {
				out = append(out, r.exportedFunctions(target, name, seen)...)
			}
		}
	}
	return out
}
func (r *dartMemberResolver) importedFunctions(prefix, name, file string) []*facts.Fact {
	key := dartImportedFunctionKey{prefix, name, r.libraryFile(file)}
	if cached, ok := r.importedFunctionCache[key]; ok {
		return cached
	}
	var out, implicit []*facts.Fact
	for _, dep := range r.imports[r.libraryFile(file)] {
		if !dep.PropBool("reexport") && dep.PropString("import_prefix") == prefix && dartImportAllows(dep, name) {
			for _, target := range r.importTargets(dep) {
				if dep.PropBool("implicit") {
					implicit = append(implicit, r.exportedFunctionFacts(target, name)...)
				} else {
					out = append(out, r.exportedFunctionFacts(target, name)...)
				}
			}
		}
	}
	if len(out) == 0 {
		out = implicit
	}
	out = r.preferNonSDKFacts(out)
	r.importedFunctionCache[key] = out
	return out
}
func (r *dartMemberResolver) extensionMethod(owner *facts.Fact, typ, name, file string) *facts.Fact {
	// A constructor-result receiver can acquire its owner in the second pass.
	// Do not cache a failed lookup before that construction has been verified.
	if owner == nil && strings.ContainsAny(typ, "()[]<>") {
		return nil
	}
	key := dartTypeKey{typ + "." + name, file}
	if result, ok := r.extensionCache[key]; ok {
		return result
	}
	var candidates []*facts.Fact
	for _, ext := range r.extensionsFor(owner, typ, file) {
		if entry := r.members[ext.Name+"."+name]; entry.readable() != nil && r.sameDartLibrary(entry.readable(), ext) {
			candidates = append(candidates, entry.readable())
		}
	}
	result := uniqueDartFact(candidates)
	r.extensionCache[key] = result
	return result
}

func (r *dartMemberResolver) extensionsFor(owner *facts.Fact, typ, file string) []*facts.Fact {
	host := r.libraryFile(file)
	key := dartExtensionReceiver{owner, typ, host}
	if cached, ok := r.applicableExtensions[key]; ok {
		return cached
	}
	short := typ[strings.LastIndexByte(typ, '.')+1:]
	if owner != nil {
		short = owner.Name[strings.LastIndexByte(owner.Name, '.')+1:]
	}
	var out []*facts.Fact
	for _, ext := range r.extensions[short] {
		on := ext.PropString("extension_on")
		onType := r.typeNamed(on, ext.File)
		if owner != nil || onType != nil {
			if owner == nil || owner != onType {
				continue
			}
		} else if on != typ || !dartCoreExtensionType(typ) {
			continue
		}
		visible := r.libraryFile(ext.File) == host
		extName := ext.Name[strings.LastIndexByte(ext.Name, '.')+1:]
		if !visible {
			for _, dep := range r.imports[host] {
				if !dep.PropBool("reexport") && dep.PropString("import_prefix") == "" && dartImportAllows(dep, extName) {
					for _, target := range r.importTargets(dep) {
						for _, f := range r.exportedTypeFacts(target, extName) {
							if f == ext {
								visible = true
							}
						}
					}
				}
			}
		}
		if visible {
			out = append(out, ext)
		}
	}
	r.applicableExtensions[key] = out
	return out
}

// Resolve import paths using URI/package identity, never a type's short name.
func (r *dartMemberResolver) importTargets(dep *facts.Fact) []string {
	if cached, ok := r.importFiles[dep]; ok {
		return cached
	}
	paths, _ := dep.Props["resolved_files"].([]string)
	if len(paths) == 0 {
		path := dep.PropString("resolved_file")
		if path == "" {
			path = dep.PropString("uri")
			if !strings.Contains(path, ":") {
				path = factpath.Join(factpath.Dir(dep.File), path)
			}
		}
		paths = []string{path}
	}
	var out []string
	incomplete := false
	for _, path := range paths {
		if strings.HasPrefix(path, "package:") {
			pkg, tail, ok := strings.Cut(strings.TrimPrefix(path, "package:"), "/")
			if !ok {
				incomplete = true
				continue
			}
			roots := r.packageRoots[pkg]
			if len(roots) != 1 {
				incomplete = true
				continue
			}
			for root := range roots {
				path = factpath.Join(root, "lib", tail)
			}
		}
		if strings.Contains(path, ":") {
			incomplete = true
			continue
		}
		if _, known := r.library[path]; !known {
			incomplete = true
			continue
		}
		if !slices.Contains(out, path) {
			out = append(out, path)
		}
	}
	if dep.PropBool("conditional_import") && incomplete {
		out = nil
	}
	r.importFiles[dep] = out
	return out
}

func dartCoreExtensionType(name string) bool {
	switch name {
	case "String", "int", "double", "num", "bool", "Object", "Function", "Symbol", "Type", "Null", "Never", "List", "Map", "Set", "Iterable", "Iterator":
		return true
	}
	return false
}

// Cache complete export traversals, never partial DFS results from a cycle.
func (r *dartMemberResolver) exportedTypeFacts(file, name string) []*facts.Fact {
	key := dartTypeKey{name, r.libraryFile(file)}
	if cached, ok := r.exportedTypeCache[key]; ok {
		return cached
	}
	result := r.exportedTypes(file, name, map[string]bool{})
	r.exportedTypeCache[key] = result
	return result
}
func (r *dartMemberResolver) exportedFunctionFacts(file, name string) []*facts.Fact {
	key := dartTypeKey{name, r.libraryFile(file)}
	if cached, ok := r.exportedFunctionCache[key]; ok {
		return cached
	}
	result := r.exportedFunctions(file, name, map[string]bool{})
	r.exportedFunctionCache[key] = result
	return result
}

func (r *dartMemberResolver) constructorNamespace(target, file string) bool {
	prefix, _, qualified := strings.Cut(target, ".")
	if !qualified {
		return true
	}
	for _, dep := range r.imports[r.libraryFile(file)] {
		if !dep.PropBool("reexport") && dep.PropString("import_prefix") == prefix {
			return true
		}
	}
	return false
}

func (r *dartMemberResolver) sameDartLibrary(a, b *facts.Fact) bool {
	return a != nil && b != nil && r.libraryFile(a.File) == r.libraryFile(b.File)
}

// Dart's import scope gives non-SDK declarations precedence over conflicting
// SDK declarations, even when both imports are explicit.
func (r *dartMemberResolver) preferNonSDKFacts(in []*facts.Fact) []*facts.Fact {
	hasSDK, hasOther := false, false
	for _, f := range in {
		if r.sdkHosts[r.libraryFile(f.File)] {
			hasSDK = true
		} else {
			hasOther = true
		}
	}
	if !hasSDK || !hasOther {
		return in
	}
	out := make([]*facts.Fact, 0, len(in))
	for _, f := range in {
		if !r.sdkHosts[r.libraryFile(f.File)] {
			out = append(out, f)
		}
	}
	return out
}
