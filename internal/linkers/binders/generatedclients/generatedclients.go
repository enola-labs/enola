// Package generatedclients turns calls through a client generated from an OpenAPI spec
// into client routes at the code that makes them.
//
// A repo that consumes a service through generated code keeps the service's spec, and
// the OpenAPI extractor reads every operation in it as a client call. That is two
// errors at once. Operations nothing calls count as calls, so a repo vendoring a
// 119-operation spec to use one of them depends, as far as the graph can tell, on all
// 119. And the operations it does call are attributed to the spec file rather than to
// a function, so nothing walks from an endpoint to the code, or the screens, that
// send it.
//
// The binder finds the code calling each generated client and emits a client route
// there, with the caller named, then marks the spec's operations as declarations
// (facts.PropSpecDeclaration) so they stop standing in for calls. A spec whose client
// no code is seen calling is left as it is: absence of evidence is not evidence of
// absence, and those operations remain the only record of the dependency.
//
// Two shapes of generated client are recognized, both by structure:
//
//   - Generated outside the checkout, at build time (an Android app): the generator
//     names the client's package after the spec file, so a type imported from a
//     package with a segment equal to the spec's normalized name belongs to it. A
//     method in a file importing it and calling one of the spec's operationIds by
//     name calls that operation.
//   - Generated into the checkout (a Go service): the generated methods carry
//     generated=true and are named after the operationIds, with the generator's
//     suffixes. The package whose generated types implement most of a spec's
//     operations is that spec's client, and a hand-written call to one of its
//     methods calls that operation.
//
// It runs pre-link, after vendored-specs has settled which specs are clients: the
// routes it emits are what the cross-repo linker matches.
package generatedclients

import (
	"context"
	"log"
	"path"
	"sort"
	"strings"
	"unicode"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// RouteSource is the PropSource of the routes this binder emits.
const RouteSource = facts.RouteSourceGeneratedClient

type Binder struct{}

func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "generated-clients" }

// Pre-link: the routes this emits are what the cross-repo linker matches on.
func (b *Binder) Stage() plugin.BindStage { return plugin.StagePreLink }

// spec is one client spec of a repo: its operations by operationId, and by the
// PascalCase spelling generators give method names.
type spec struct {
	file     string
	key      string
	ops      map[string]facts.Fact
	byPascal map[string]string
}

// copiedProps are the operation props a call-site route carries over from the spec.
var copiedProps = []string{"method", "operationId", "spec_file", "gateway_path", "gateway_prefix", "exposed", "auth_mode"}

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	all := store.FactsRef()
	specs := clientSpecs(all)
	if len(specs) == 0 {
		return nil
	}

	var out []facts.Fact
	used := map[[2]string]bool{} // {repo, spec file}
	existing := map[string]bool{}
	for _, f := range all {
		if f.Kind == facts.KindRoute && f.PropString(facts.PropSource) == RouteSource {
			existing[siteKey(f)] = true
		}
	}
	emit := func(caller facts.Fact, s *spec, opID string) {
		op := s.ops[opID]
		props := map[string]any{
			facts.PropRole:   facts.RoleClient,
			facts.PropSource: RouteSource,
			"framework":      "openapi-client",
			facts.PropCaller: caller.Name,
		}
		if lang := caller.PropString("language"); lang != "" {
			props["language"] = lang
		}
		for _, k := range copiedProps {
			if v := op.PropAny(k); v != nil {
				props[k] = v
			}
		}
		route := facts.Fact{Kind: facts.KindRoute, Name: op.Name, File: caller.File, Line: caller.Line, Repo: caller.Repo, Props: props}
		used[[2]string{caller.Repo, s.file}] = true
		if k := siteKey(route); !existing[k] {
			existing[k] = true
			out = append(out, route)
		}
	}

	importedClientCalls(all, specs, emit)
	inRepoClientCalls(all, specs, emit)

	marked := 0
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindRoute || f.PropString(facts.PropSource) != facts.RouteSourceOpenAPI ||
			f.PropString(facts.PropRole) != facts.RoleClient || !used[[2]string{f.Repo, f.PropString("spec_file")}] {
			return
		}
		if !f.PropBool(facts.PropSpecDeclaration) {
			f.SetProp(facts.PropSpecDeclaration, true)
			marked++
		}
	})
	sort.Slice(out, func(i, j int) bool { return siteKey(out[i]) < siteKey(out[j]) })
	if len(out) > 0 {
		store.Add(out...)
	}
	if len(out) > 0 || marked > 0 {
		log.Printf("[binder:generated-clients] %d call(s) through generated clients; %d spec operation(s) marked as declarations", len(out), marked)
	}
	return nil
}

// clientSpecs returns each repo's client specs, keyed repo -> spec file.
func clientSpecs(all []facts.Fact) map[string]map[string]*spec {
	out := map[string]map[string]*spec{}
	for _, f := range all {
		if f.Kind != facts.KindRoute || f.PropString(facts.PropSource) != facts.RouteSourceOpenAPI ||
			f.PropString(facts.PropRole) != facts.RoleClient {
			continue
		}
		file, id := f.PropString("spec_file"), f.PropString("operationId")
		if id == "" {
			id = defaultOperationID(f.PropString("method"), f.Name)
		}
		if file == "" || id == "" {
			continue
		}
		if out[f.Repo] == nil {
			out[f.Repo] = map[string]*spec{}
		}
		s := out[f.Repo][file]
		if s == nil {
			s = &spec{file: file, key: specKey(file), ops: map[string]facts.Fact{}, byPascal: map[string]string{}}
			out[f.Repo][file] = s
		}
		if _, dup := s.ops[id]; !dup {
			s.ops[id] = f
			s.byPascal[pascal(id)] = id
		}
	}
	return out
}

// importedClientCalls finds calls through clients generated outside the checkout.
func importedClientCalls(all []facts.Fact, specs map[string]map[string]*spec, emit func(facts.Fact, *spec, string)) {
	// Spec files by normalized name, per repo.
	byKey := map[string]map[string][]*spec{}
	for repo, ss := range specs {
		for _, s := range sortedSpecs(ss) {
			if s.key == "" {
				continue
			}
			if byKey[repo] == nil {
				byKey[repo] = map[string][]*spec{}
			}
			byKey[repo][s.key] = append(byKey[repo][s.key], s)
		}
	}
	// The specs whose generated API types each file imports: repo\x00file -> specs.
	imported := map[string][]*spec{}
	for _, f := range all {
		if f.Kind != facts.KindDependency || byKey[f.Repo] == nil || f.File == "" || facts.IsTestPath(f.File) {
			continue
		}
		_, target, ok := strings.Cut(f.Name, " -> ")
		if !ok {
			continue
		}
		segs := strings.Split(target, ".")
		for i := 0; i+2 < len(segs); i++ {
			if !generatedLayers[segs[i+1]] {
				continue
			}
			if ss := specsNamed(byKey[f.Repo], strings.ToLower(segs[i])); len(ss) > 0 {
				k := f.Repo + "\x00" + f.File
				imported[k] = append(imported[k], closest(ss, f.File))
			}
		}
	}
	if len(imported) == 0 {
		return
	}
	// Methods in those files calling an operation by name. The receiver's type is not
	// read: imports are often aliased, and the client reaches the class through a
	// constructor parameter, a property or a provider alike.
	for _, f := range all {
		if f.Kind != facts.KindSymbol || f.PropString("symbol_kind") != facts.SymbolMethod {
			continue
		}
		ss := imported[f.Repo+"\x00"+f.File]
		if len(ss) == 0 {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelCalls || strings.ContainsAny(r.Target, "./#") {
				continue
			}
			// Generators name the method after the operationId in their own case:
			// GetOrder in the spec is getOrder in Kotlin.
			for _, s := range ss {
				if id := s.byPascal[pascal(r.Target)]; id != "" {
					emit(f, s, id)
				}
			}
		}
	}
}

// specsNamed returns the specs a generated package segment is named after: the spec
// whose normalized name it is, else the one with the longest name it extends by a
// word, an overlay variant applied at build time ("ordersapi" + "edge").
func specsNamed(byKey map[string][]*spec, seg string) []*spec {
	if ss := byKey[seg]; len(ss) > 0 {
		return ss
	}
	best := ""
	for k := range byKey {
		if len(k) > len(best) && len(k) < len(seg) && strings.HasPrefix(seg, k) && isWord(seg[len(k):]) {
			best = k
		}
	}
	return byKey[best]
}

func isWord(s string) bool {
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return s != ""
}

// generatedLayers are the packages a generator writes a client's API and model types
// into, directly below the package it names after the spec.
var generatedLayers = map[string]bool{"apis": true, "api": true, "models": true, "model": true, "client": true}

// closest returns the spec whose file shares the longest directory prefix with file:
// two modules vendoring one spec each generate their own client.
func closest(ss []*spec, file string) *spec {
	best, bestN := ss[0], -1
	for _, s := range ss {
		n := 0
		a, b := strings.Split(path.Dir(s.file), "/"), strings.Split(path.Dir(file), "/")
		for n < len(a) && n < len(b) && a[n] == b[n] {
			n++
		}
		if n > bestN {
			best, bestN = s, n
		}
	}
	return best
}

// generatedSuffixes are the suffixes generators add to an operation's method name.
var generatedSuffixes = []string{"WithBodyWithResponse", "WithResponse", "WithBody"}

// inRepoClientCalls finds calls through clients generated into the checkout.
func inRepoClientCalls(all []facts.Fact, specs map[string]map[string]*spec, emit func(facts.Fact, *spec, string)) {
	// The operation names each package's generated methods implement.
	pkgOps := map[string]map[string]bool{} // repo\x00package -> base method names
	for _, f := range all {
		// A generated mock implements every operation too; it is not the client.
		if f.Kind != facts.KindSymbol || !f.PropBool("generated") || f.PropString("symbol_kind") != facts.SymbolMethod ||
			specs[f.Repo] == nil || facts.IsTestPath(f.File) {
			continue
		}
		pkg := ownerPackage(f.Name)
		if pkg == "" {
			continue
		}
		k := f.Repo + "\x00" + pkg
		if pkgOps[k] == nil {
			pkgOps[k] = map[string]bool{}
		}
		pkgOps[k][baseOperation(facts.ShortName(f.Name))] = true
	}
	if len(pkgOps) == 0 {
		return
	}
	// Each spec's client package: the one implementing most of its operations, if more
	// than half of them and no other package as many.
	pkgSpec := map[string]*spec{}
	pkgs := make([]string, 0, len(pkgOps))
	for k := range pkgOps {
		pkgs = append(pkgs, k)
	}
	sort.Strings(pkgs)
	for repo, ss := range specs {
		for _, s := range sortedSpecs(ss) {
			best, bestN, tie := "", 0, false
			for _, k := range pkgs {
				if !strings.HasPrefix(k, repo+"\x00") {
					continue
				}
				n := 0
				for p := range s.byPascal {
					if pkgOps[k][p] {
						n++
					}
				}
				switch {
				case n > bestN:
					best, bestN, tie = k, n, false
				case n == bestN && n > 0:
					tie = true
				}
			}
			if best != "" && !tie && 2*bestN > len(s.ops) && pkgSpec[best] == nil {
				pkgSpec[best] = s
			}
		}
	}
	for _, f := range all {
		if f.Kind != facts.KindSymbol || f.PropBool("generated") || facts.IsTestPath(f.File) {
			continue
		}
		switch f.PropString("symbol_kind") {
		case facts.SymbolMethod, facts.SymbolFunc:
		default:
			continue
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelCalls {
				continue
			}
			s := pkgSpec[f.Repo+"\x00"+ownerPackage(r.Target)]
			if s == nil {
				continue
			}
			if id := s.byPascal[baseOperation(facts.ShortName(r.Target))]; id != "" {
				emit(f, s, id)
			}
		}
	}
}

// ownerPackage returns the package of a method's owner type: "a/b.T.M" -> "a/b".
func ownerPackage(method string) string {
	dot := strings.LastIndexByte(method, '.')
	if dot <= 0 {
		return ""
	}
	owner := method[:dot]
	dot = strings.LastIndexByte(owner, '.')
	if dot <= 0 {
		return ""
	}
	return owner[:dot]
}

// baseOperation strips a generator's suffix from a method name.
func baseOperation(name string) string {
	for _, suf := range generatedSuffixes {
		if trimmed := strings.TrimSuffix(name, suf); trimmed != name && trimmed != "" {
			return trimmed
		}
	}
	return name
}

// specKey normalizes a spec file name the way generators derive a package from it:
// "svc-orders-api-edge.yaml" -> "svcordersapiedge".
func specKey(file string) string {
	base := path.Base(file)
	base = strings.TrimSuffix(base, path.Ext(base))
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// defaultOperationID is the id a generator names an operation without an operationId
// by: the verb and the path's segments, "GET /payouts/{id}" -> "get-payouts-{id}",
// which pascal spells GetPayoutsId.
func defaultOperationID(method, p string) string {
	if method == "" {
		return ""
	}
	parts := []string{strings.ToLower(method)}
	for _, seg := range strings.Split(p, "/") {
		if seg != "" {
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, "-")
}

// pascal spells an operationId as generators name its method.
func pascal(id string) string {
	var b strings.Builder
	up := true
	for _, r := range id {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			up = true
			continue
		}
		if up {
			r = unicode.ToUpper(r)
			up = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

func sortedSpecs(m map[string]*spec) []*spec {
	out := make([]*spec, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].file < out[j].file })
	return out
}

func siteKey(f facts.Fact) string {
	return f.Repo + "\x00" + f.File + "\x00" + f.PropString("method") + "\x00" + f.Name + "\x00" + f.PropString("spec_file") + "\x00" + f.PropString(facts.PropCaller)
}
