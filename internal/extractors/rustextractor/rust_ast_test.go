package rustextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// extractAST runs the tree-sitter walker over src as if it were "pkg/lib.rs"
// belonging to a single crate rooted at "pkg" (a single-crate repo has no
// Cargo.toml prefix directory, so crateDir == dir).
func extractAST(t *testing.T, src string) []facts.Fact {
	t.Helper()
	ff, _ := extractFileAST([]byte(src), "pkg/lib.rs", []crateInfo{{name: "pkg", dir: "pkg"}}, map[string]bool{"pkg": true})
	return ff
}

func findFact(ff []facts.Fact, name string) (facts.Fact, bool) {
	for _, f := range ff {
		if f.Name == name {
			return f, true
		}
	}
	return facts.Fact{}, false
}

func findFactsByKind(ff []facts.Fact, kind string) []facts.Fact {
	var out []facts.Fact
	for _, f := range ff {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func hasRelation(f facts.Fact, relKind, target string) bool {
	for _, r := range f.Relations {
		if r.Kind == relKind && r.Target == target {
			return true
		}
	}
	return false
}

func TestAST_Struct(t *testing.T) {
	ff := extractAST(t, `pub struct User { name: String }`)
	f, ok := findFact(ff, "pkg.User")
	if !ok {
		t.Fatal("expected fact for pkg.User")
	}
	if f.Props["symbol_kind"] != facts.SymbolStruct {
		t.Errorf("symbol_kind = %v, want struct", f.Props["symbol_kind"])
	}
	if f.Props["exported"] != true {
		t.Errorf("exported = %v, want true (pub)", f.Props["exported"])
	}
}

func TestAST_StructNotExported(t *testing.T) {
	ff := extractAST(t, `struct Internal;`)
	f, ok := findFact(ff, "pkg.Internal")
	if !ok {
		t.Fatal("expected fact for pkg.Internal")
	}
	if f.Props["exported"] != false {
		t.Errorf("exported = %v, want false (no pub)", f.Props["exported"])
	}
}

func TestAST_Enum(t *testing.T) {
	ff := extractAST(t, `pub enum Direction { North, South }`)
	f, ok := findFact(ff, "pkg.Direction")
	if !ok {
		t.Fatal("expected fact for pkg.Direction")
	}
	if f.Props["symbol_kind"] != facts.SymbolEnum {
		t.Errorf("symbol_kind = %v, want enum", f.Props["symbol_kind"])
	}
}

func TestAST_Trait(t *testing.T) {
	ff := extractAST(t, `
pub trait Greeter {
    fn greet(&self) -> String;
    fn shout(&self) -> String {
        self.greet()
    }
}
`)
	f, ok := findFact(ff, "pkg.Greeter")
	if !ok {
		t.Fatal("expected fact for pkg.Greeter")
	}
	if f.Props["symbol_kind"] != facts.SymbolInterface {
		t.Errorf("symbol_kind = %v, want interface", f.Props["symbol_kind"])
	}
	// The signature-only method still gets a symbol fact.
	sig, ok := findFact(ff, "pkg.Greeter.greet")
	if !ok {
		t.Fatal("expected fact for pkg.Greeter.greet (signature)")
	}
	if sig.Props["symbol_kind"] != facts.SymbolMethod {
		t.Errorf("trait method symbol_kind = %v, want method", sig.Props["symbol_kind"])
	}
	// The default method's self.greet() call resolves to its trait sibling.
	shout, ok := findFact(ff, "pkg.Greeter.shout")
	if !ok {
		t.Fatal("expected fact for pkg.Greeter.shout")
	}
	if !hasRelation(shout, facts.RelCalls, "pkg.Greeter.greet") {
		t.Errorf("expected RelCalls -> pkg.Greeter.greet for self.greet(), got %+v", shout.Relations)
	}
}

func TestAST_TypeAliasAndConstStatic(t *testing.T) {
	ff := extractAST(t, `
pub type UserId = u64;
pub const MAX_USERS: u32 = 100;
static COUNTER: u32 = 0;
`)
	ta, ok := findFact(ff, "pkg.UserId")
	if !ok || ta.Props["symbol_kind"] != facts.SymbolType {
		t.Errorf("expected pkg.UserId symbol_kind=type, got %+v ok=%v", ta.Props, ok)
	}
	c, ok := findFact(ff, "pkg.MAX_USERS")
	if !ok || c.Props["symbol_kind"] != facts.SymbolConstant || c.Props["exported"] != true {
		t.Errorf("expected pkg.MAX_USERS symbol_kind=constant exported=true, got %+v ok=%v", c.Props, ok)
	}
	s, ok := findFact(ff, "pkg.COUNTER")
	if !ok || s.Props["symbol_kind"] != facts.SymbolVariable || s.Props["exported"] != false {
		t.Errorf("expected pkg.COUNTER symbol_kind=variable exported=false, got %+v ok=%v", s.Props, ok)
	}
}

func TestAST_ImplInherentMethods(t *testing.T) {
	ff := extractAST(t, `
pub struct Counter { n: u32 }
impl Counter {
    pub fn new() -> Self {
        Counter { n: 0 }
    }
    pub fn increment(&mut self) {
        self.n += 1;
    }
}
`)
	newFn, ok := findFact(ff, "pkg.Counter.new")
	if !ok {
		t.Fatal("expected fact for pkg.Counter.new")
	}
	if newFn.Props["symbol_kind"] != facts.SymbolMethod {
		t.Errorf("symbol_kind = %v, want method", newFn.Props["symbol_kind"])
	}
	if newFn.Props["static"] != true {
		t.Errorf("static = %v, want true (no self param)", newFn.Props["static"])
	}
	incr, ok := findFact(ff, "pkg.Counter.increment")
	if !ok {
		t.Fatal("expected fact for pkg.Counter.increment")
	}
	if incr.Props["static"] == true {
		t.Errorf("increment has &mut self, should not be static")
	}
	if incr.Props["receiver"] != "Counter" {
		t.Errorf("receiver = %v, want Counter", incr.Props["receiver"])
	}
}

func TestAST_ImplTraitForType_EmitsImplements(t *testing.T) {
	ff := extractAST(t, `
pub struct Wrapper { n: u32 }
impl std::fmt::Display for Wrapper {
    fn fmt(&self) {}
}
`)
	w, ok := findFact(ff, "pkg.Wrapper")
	if !ok {
		t.Fatal("expected fact for pkg.Wrapper")
	}
	// The type/impl are on the same fact only after applyImplements runs (a
	// rust.go-level post-pass); extractFileAST alone returns the observation
	// via the second return value, not yet attached.
	if hasRelation(w, facts.RelImplements, "Display") {
		t.Error("extractFileAST alone must not attach implements; that's applyImplements' job")
	}
	_, impls := extractFileAST([]byte(`
pub struct Wrapper { n: u32 }
impl std::fmt::Display for Wrapper {
    fn fmt(&self) {}
}
`), "pkg/lib.rs", []crateInfo{{name: "pkg", dir: "pkg"}}, map[string]bool{"pkg": true})
	if len(impls) != 1 || impls[0].typeName != "pkg.Wrapper" || impls[0].traitName != "Display" {
		t.Errorf("impls = %+v, want [{pkg.Wrapper Display}]", impls)
	}
}

func TestApplyImplements_AttachesAcrossFiles(t *testing.T) {
	crates := []crateInfo{{name: "pkg", dir: "pkg"}}
	dirs := map[string]bool{"pkg": true}
	typeFacts, _ := extractFileAST([]byte(`pub struct Wrapper;`), "pkg/types.rs", crates, dirs)
	_, impls := extractFileAST([]byte(`
impl std::fmt::Display for Wrapper {
    fn fmt(&self) {}
}
`), "pkg/display.rs", crates, dirs)

	all := append([]facts.Fact{}, typeFacts...)
	applyImplements(all, impls)

	w, ok := findFact(all, "pkg.Wrapper")
	if !ok {
		t.Fatal("expected fact for pkg.Wrapper")
	}
	if !hasRelation(w, facts.RelImplements, "Display") {
		t.Errorf("expected RelImplements -> Display attached from a different file's impl block, got %+v", w.Relations)
	}
	// Exactly one Wrapper symbol fact — no duplicate created for the impl.
	if got := len(findFactsByKind(all, facts.KindSymbol)); got != 1 {
		t.Errorf("expected exactly 1 symbol fact (no duplicate from the impl block), got %d", got)
	}
}

func TestAST_BareCallSameDir(t *testing.T) {
	ff := extractAST(t, `
fn caller() {
    helper();
}
fn helper() {}
`)
	c, ok := findFact(ff, "pkg.caller")
	if !ok {
		t.Fatal("expected fact for pkg.caller")
	}
	if !hasRelation(c, facts.RelCalls, "pkg.helper") {
		t.Errorf("expected RelCalls -> pkg.helper, got %+v", c.Relations)
	}
}

func TestAST_CapitalizedBareCall_Instantiates(t *testing.T) {
	ff := extractAST(t, `
pub struct Bar;
fn make() {
    let b = Bar();
}
`)
	m, ok := findFact(ff, "pkg.make")
	if !ok {
		t.Fatal("expected fact for pkg.make")
	}
	if !hasRelation(m, facts.RelInstantiates, "Bar") {
		t.Errorf("expected RelInstantiates -> Bar, got %+v", m.Relations)
	}
}

func TestAST_ScopedAssociatedFnCall_InstantiatesType(t *testing.T) {
	ff := extractAST(t, `
pub struct Foo { n: u32 }
fn make() -> Foo {
    Foo::new()
}
`)
	m, ok := findFact(ff, "pkg.make")
	if !ok {
		t.Fatal("expected fact for pkg.make")
	}
	if !hasRelation(m, facts.RelInstantiates, "Foo") {
		t.Errorf("expected RelInstantiates -> Foo, got %+v", m.Relations)
	}
}

func TestAST_ScopedAssociatedFnCall_SelfNotInstantiated(t *testing.T) {
	ff := extractAST(t, `
pub struct Foo { n: u32 }
impl Foo {
    fn make() -> Self {
        Self::new()
    }
    fn new() -> Self { Foo { n: 0 } }
}
`)
	m, ok := findFact(ff, "pkg.Foo.make")
	if !ok {
		t.Fatal("expected fact for pkg.Foo.make")
	}
	if hasRelation(m, facts.RelInstantiates, "Self") {
		t.Errorf("Self:: should not be recorded as an instantiate target: %+v", m.Relations)
	}
}

func TestAST_StructLiteral_Instantiates(t *testing.T) {
	ff := extractAST(t, `
pub struct User { id: u32, name: String }
fn make(id: u32) -> User {
    User { id, name: String::from("x") }
}
`)
	m, ok := findFact(ff, "pkg.make")
	if !ok {
		t.Fatal("expected fact for pkg.make")
	}
	if !hasRelation(m, facts.RelInstantiates, "User") {
		t.Errorf("expected RelInstantiates -> User for the struct literal, got %+v", m.Relations)
	}
	// The field value's nested call is still walked and credited normally.
	if !hasRelation(m, facts.RelCalls, "from") {
		t.Errorf("expected RelCalls -> from for String::from(...) inside the literal, got %+v", m.Relations)
	}
}

func TestAST_StructLiteralWithUpdateSyntax_Instantiates(t *testing.T) {
	// `..base` (functional update syntax) construction still counts.
	ff := extractAST(t, `
pub struct Config { a: u32, b: u32 }
fn make(base: Config) -> Config {
    Config { a: 1, ..base }
}
`)
	m, ok := findFact(ff, "pkg.make")
	if !ok {
		t.Fatal("expected fact for pkg.make")
	}
	if !hasRelation(m, facts.RelInstantiates, "Config") {
		t.Errorf("expected RelInstantiates -> Config, got %+v", m.Relations)
	}
}

func TestAST_QualifiedStructLiteral_Instantiates(t *testing.T) {
	// A struct literal reached through a module/crate path still credits the
	// simple type name (mirrors the scoped_identifier call-callee handling).
	ff := extractAST(t, `
fn make() -> other::Wrapper {
    other::Wrapper { n: 0 }
}
`)
	m, ok := findFact(ff, "pkg.make")
	if !ok {
		t.Fatal("expected fact for pkg.make")
	}
	if !hasRelation(m, facts.RelInstantiates, "Wrapper") {
		t.Errorf("expected RelInstantiates -> Wrapper, got %+v", m.Relations)
	}
}

func TestAST_SelfMethodCall(t *testing.T) {
	ff := extractAST(t, `
pub struct Service;
impl Service {
    fn a(&self) {
        self.b();
    }
    fn b(&self) {}
}
`)
	a, ok := findFact(ff, "pkg.Service.a")
	if !ok {
		t.Fatal("expected fact for pkg.Service.a")
	}
	if !hasRelation(a, facts.RelCalls, "pkg.Service.b") {
		t.Errorf("expected RelCalls -> pkg.Service.b for self.b(), got %+v", a.Relations)
	}
}

func TestAST_SelfStaticCall(t *testing.T) {
	ff := extractAST(t, `
pub struct Service;
impl Service {
    fn a() {
        Self::b();
    }
    fn b() {}
}
`)
	a, ok := findFact(ff, "pkg.Service.a")
	if !ok {
		t.Fatal("expected fact for pkg.Service.a")
	}
	if !hasRelation(a, facts.RelCalls, "pkg.Service.b") {
		t.Errorf("expected RelCalls -> pkg.Service.b for Self::b(), got %+v", a.Relations)
	}
}

func TestAST_MethodCallOnOtherReceiver_ShortNameEdge(t *testing.T) {
	ff := extractAST(t, `
fn run(repo: Repo) {
    repo.save();
}
`)
	r, ok := findFact(ff, "pkg.run")
	if !ok {
		t.Fatal("expected fact for pkg.run")
	}
	if !hasRelation(r, facts.RelCalls, "save") {
		t.Errorf("expected short-name RelCalls -> save for repo.save(), got %+v", r.Relations)
	}
}

func TestAST_UnrelatedTypeMethodCall_NoFalseSelfEdge(t *testing.T) {
	// A sibling-name collision across two different impl blocks (both declare
	// "run") must not resolve Other::run() to the wrong type's method.
	ff := extractAST(t, `
struct A;
impl A {
    fn run(&self) {}
}
struct B;
impl B {
    fn call(&self) {
        A::run();
    }
    fn run(&self) {}
}
`)
	c, ok := findFact(ff, "pkg.B.call")
	if !ok {
		t.Fatal("expected fact for pkg.B.call")
	}
	if hasRelation(c, facts.RelCalls, "pkg.B.run") {
		t.Errorf("A::run() must not resolve to pkg.B.run, got %+v", c.Relations)
	}
	if hasRelation(c, facts.RelCalls, "pkg.A.run") {
		t.Errorf("A::run() must not be resolved without type inference, got %+v", c.Relations)
	}
	// Best-effort short-name fallback only.
	if !hasRelation(c, facts.RelCalls, "run") {
		t.Errorf("expected short-name RelCalls -> run, got %+v", c.Relations)
	}
}

func TestAST_NestedModQualifiesSymbolName(t *testing.T) {
	ff := extractAST(t, `
mod inner {
    pub struct Nested;
}
`)
	if _, ok := findFact(ff, "pkg.inner.Nested"); !ok {
		t.Errorf("expected fact for pkg.inner.Nested, got %+v", ff)
	}
}

func TestAST_ModDeclarationNoBody_NoFact(t *testing.T) {
	// `mod foo;` declares another file; it must not itself produce a fact.
	ff := extractAST(t, `mod foo;`)
	if len(ff) != 0 {
		t.Errorf("expected no facts for a bodyless mod declaration, got %+v", ff)
	}
}

func TestAST_UseDependencyFacts(t *testing.T) {
	ff := extractAST(t, `
use std::collections::HashMap;
use serde::{Deserialize, Serialize};
use self::helper::run;
use super::shared::Config;
`)
	deps := findFactsByKind(ff, facts.KindDependency)
	if len(deps) != 5 {
		t.Fatalf("expected 5 dependency facts (serde::{Deserialize,Serialize} expands to two), got %d: %+v", len(deps), deps)
	}

	check := func(raw, wantSource string) {
		t.Helper()
		for _, d := range deps {
			if d.Name == "pkg -> "+raw {
				if d.Props["source"] != wantSource {
					t.Errorf("%s: source = %v, want %s", raw, d.Props["source"], wantSource)
				}
				return
			}
		}
		t.Errorf("no dependency fact found for %q", raw)
	}
	check("std::collections::HashMap", "stdlib")
	check("serde::Deserialize", "external")
	check("serde::Serialize", "external")
	check("self::helper::run", "internal")
	check("super::shared::Config", "internal")
}

func TestAST_UseSelfResolvesToKnownSubdirectory(t *testing.T) {
	ff, _ := extractFileAST([]byte(`use self::helper::run;`), "pkg/lib.rs",
		[]crateInfo{{name: "pkg", dir: "pkg"}},
		map[string]bool{"pkg": true, "pkg/helper": true})
	deps := findFactsByKind(ff, facts.KindDependency)
	if len(deps) != 1 {
		t.Fatalf("expected 1 dependency fact, got %d", len(deps))
	}
	if !hasRelation(deps[0], facts.RelImports, "pkg/helper") {
		t.Errorf("expected RelImports -> pkg/helper (known submodule dir), got %+v", deps[0].Relations)
	}
}

func TestAST_ExternCrate(t *testing.T) {
	ff := extractAST(t, `extern crate serde;`)
	deps := findFactsByKind(ff, facts.KindDependency)
	if len(deps) != 1 || deps[0].Props["source"] != "external" {
		t.Errorf("expected 1 external dependency fact for extern crate serde, got %+v", deps)
	}
}

// --- Import-based bare-call resolution ---

func TestAST_BareCallResolvesThroughInternalImport(t *testing.T) {
	ff, _ := extractFileAST([]byte(`
use self::helper::run;

fn caller() {
    run();
}
`), "pkg/lib.rs",
		[]crateInfo{{name: "pkg", dir: "pkg"}},
		map[string]bool{"pkg": true, "pkg/helper": true})
	c, ok := findFact(ff, "pkg.caller")
	if !ok {
		t.Fatal("expected fact for pkg.caller")
	}
	if !hasRelation(c, facts.RelCalls, "pkg/helper.run") {
		t.Errorf("expected RelCalls -> pkg/helper.run via the use import, got %+v", c.Relations)
	}
}

func TestAST_BareCallToExternalImport_NoEdge(t *testing.T) {
	ff := extractAST(t, `
use serde::Deserialize;

fn caller() {
    Deserialize();
}
`)
	c, ok := findFact(ff, "pkg.caller")
	if !ok {
		t.Fatal("expected fact for pkg.caller")
	}
	// Capitalized bare call -> instantiates, not calls; either way it must not
	// resolve to a phantom local fact name.
	for _, r := range c.Relations {
		if r.Kind == facts.RelCalls && strings.HasPrefix(r.Target, "pkg.") {
			t.Errorf("external import must not resolve to a local fact name, got %+v", r)
		}
	}
}

func TestAST_ImportedNameShadowedBySiblingMethod(t *testing.T) {
	// A sibling method of the same name takes priority over an import — the
	// import map is only consulted when nothing closer resolves.
	ff, _ := extractFileAST([]byte(`
use self::other::run;

struct Service;
impl Service {
    fn call(&self) {
        run();
    }
    fn run(&self) {}
}
`), "pkg/lib.rs",
		[]crateInfo{{name: "pkg", dir: "pkg"}},
		map[string]bool{"pkg": true, "pkg/other": true})
	c, ok := findFact(ff, "pkg.Service.call")
	if !ok {
		t.Fatal("expected fact for pkg.Service.call")
	}
	if !hasRelation(c, facts.RelCalls, "pkg.Service.run") {
		t.Errorf("expected the sibling method to win over the import, got %+v", c.Relations)
	}
	if hasRelation(c, facts.RelCalls, "pkg/other.run") {
		t.Errorf("import must not shadow a closer sibling method, got %+v", c.Relations)
	}
}

// --- #[cfg(test)] mod isolation ---

func TestAST_CfgTestMod_NoProductionFacts(t *testing.T) {
	ff := extractAST(t, `
pub fn add(a: i32, b: i32) -> i32 {
    a + b
}

#[cfg(test)]
mod tests {
    use super::*;

    struct Fixture;

    #[test]
    fn it_adds() {
        assert_eq!(add(1, 2), 3);
    }
}
`)
	if _, ok := findFact(ff, "pkg.tests.it_adds"); ok {
		t.Error("test function must not become a production symbol fact")
	}
	if _, ok := findFact(ff, "pkg.tests.Fixture"); ok {
		t.Error("test-only fixture struct must not become a production symbol fact")
	}
	// The production function is unaffected.
	if _, ok := findFact(ff, "pkg.add"); !ok {
		t.Error("expected the production function pkg.add to still be emitted")
	}
}

func TestAST_CfgTestMod_CreditsProductionCallsAsTestRef(t *testing.T) {
	ff := extractAST(t, `
pub fn add(a: i32, b: i32) -> i32 {
    a + b
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn it_adds() {
        // Deliberately not wrapped in assert_eq!(...) — macro arguments are
        // raw token trees to tree-sitter, not a parsed call_expression, so a
        // call made only inside a macro invocation is invisible to the walker.
        let result = add(1, 2);
        assert_eq!(result, 3);
    }
}
`)
	refs := findFactsByKind(ff, facts.KindTestRef)
	if len(refs) != 1 {
		t.Fatalf("expected exactly 1 KindTestRef fact, got %d: %+v", len(refs), refs)
	}
	if refs[0].Name != "pkg/lib.rs" || refs[0].File != "pkg/lib.rs" {
		t.Errorf("KindTestRef fact should be keyed by file path, got Name=%q File=%q", refs[0].Name, refs[0].File)
	}
	if !hasRelation(refs[0], facts.RelCalls, "pkg.add") {
		t.Errorf("expected RelCalls -> pkg.add from the test body, got %+v", refs[0].Relations)
	}
}

func TestAST_NonCfgTestMod_StillProduction(t *testing.T) {
	// A mod without #[cfg(test)] (even if named "tests") is ordinary code.
	ff := extractAST(t, `
mod tests {
    pub fn helper() {}
}
`)
	if _, ok := findFact(ff, "pkg.tests.helper"); !ok {
		t.Error("expected pkg.tests.helper to be a normal production symbol (no #[cfg(test)] gate)")
	}
}

func TestAST_CfgTestMod_NestedInsideMod(t *testing.T) {
	ff := extractAST(t, `
mod inner {
    pub fn helper() {}

    #[cfg(test)]
    mod tests {
        use super::*;

        #[test]
        fn it_works() {
            helper();
        }
    }
}
`)
	if _, ok := findFact(ff, "pkg.inner.tests.it_works"); ok {
		t.Error("nested #[cfg(test)] mod must still be suppressed")
	}
	refs := findFactsByKind(ff, facts.KindTestRef)
	if len(refs) != 1 || !hasRelation(refs[0], facts.RelCalls, "pkg.inner.helper") {
		t.Errorf("expected a KindTestRef -> pkg.inner.helper, got %+v", refs)
	}
}

func TestAST_CfgTestMod_LocalFunctionsStayTestOnly(t *testing.T) {
	ff := extractAST(t, `
pub fn production() {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn callback_fixture() {
        fn no_env(_: &str) -> Option<String> { None }
        fn run_callback() { production(); }
        let _env = &no_env;
        let _run = &run_callback;
    }
}
`)
	for _, name := range []string{"pkg.tests.callback_fixture", "pkg.no_env", "pkg.run_callback"} {
		if _, ok := findFact(ff, name); ok {
			t.Errorf("test-only function %s must not become a production symbol", name)
		}
	}
	refs := findFactsByKind(ff, facts.KindTestRef)
	if len(refs) != 1 || !hasRelation(refs[0], facts.RelCalls, "pkg.production") {
		t.Errorf("expected production call to survive as a test reference, got %+v", refs)
	}
}

func TestAST_InstrumentAttributeRecordsCalls(t *testing.T) {
	ff := extractAST(t, `
fn parse_asset_count_total() -> u64 { 0 }

#[tracing::instrument(skip_all, fields(total = parse_asset_count_total()))]
// An attribute still belongs to the item across comments.
fn resolve() {}

struct Resolver;
impl Resolver {
    #[instrument(fields(total = parse_asset_count_total()))]
    // Same rule inside an impl body.
    fn resolve_method(&self) {}
}

#[instrument(fields(recurse = recursive()))]
fn recursive() {}
`)
	for _, name := range []string{"pkg.resolve", "pkg.Resolver.resolve_method"} {
		resolve, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("expected %s symbol", name)
		}
		if !hasRelation(resolve, facts.RelCalls, "pkg.parse_asset_count_total") {
			t.Errorf("instrument attribute call was not recorded on %s: %+v", name, resolve.Relations)
		}
	}
	recursive, ok := findFact(ff, "pkg.recursive")
	if !ok || recursive.Props["recursive_self"] != true {
		t.Errorf("attribute call must be measured in the annotated function's scope, got %+v", recursive)
	}
}

func TestAST_RustRuntimeEntrypointsAreFrameworkRegistered(t *testing.T) {
	ff := extractAST(t, `
#[pymodule]
#[pyo3(name = "_core")]
// Comments do not detach attributes from their item.
fn python_module() {}

#[wasm_bindgen]
pub fn wasm_start() {}

#[unsafe(no_mangle)]
pub extern "C" fn rust_2024_export() {}

fn ordinary() {}
`)
	for _, name := range []string{"pkg.python_module", "pkg.wasm_start", "pkg.rust_2024_export"} {
		f, ok := findFact(ff, name)
		if !ok || f.Props["framework_registered"] != true {
			t.Errorf("%s must be framework_registered, got %+v", name, f)
		}
	}
	ordinary, ok := findFact(ff, "pkg.ordinary")
	if !ok {
		t.Fatal("expected ordinary symbol")
	}
	if ordinary.Props["framework_registered"] != nil {
		t.Errorf("ordinary function must remain eligible for dead-code analysis: %+v", ordinary.Props)
	}
}

func TestAST_RuntimeEntrypointsUseStructuralAttributePaths(t *testing.T) {
	ff := extractAST(t, `
#[doc = "example: #[pymodule]"]
fn documented() {}

#[allow(dead_code, reason = "#[tauri::command]")]
fn string_argument() {}

#[my_pymodule]
fn lookalike() {}

#[unsafe(other(no_mangle))]
fn nested_lookalike() {}

// #[ctor::ctor]
fn commented() {}

#[ctor::ctor]
fn actual_entrypoint() {}
`)
	for _, name := range []string{
		"pkg.documented",
		"pkg.string_argument",
		"pkg.lookalike",
		"pkg.nested_lookalike",
		"pkg.commented",
	} {
		f, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("expected %s symbol", name)
		}
		if f.Props["framework_registered"] != nil {
			t.Errorf("attribute text must not register %s: %+v", name, f.Props)
		}
	}
	entrypoint, ok := findFact(ff, "pkg.actual_entrypoint")
	if !ok || entrypoint.Props["framework_registered"] != true {
		t.Errorf("exact attribute path must register actual_entrypoint: %+v", entrypoint)
	}
}

func TestAST_FunctionInsideConstCarriesCompileTimeContext(t *testing.T) {
	ff := extractAST(t, `
const _: () = {
    fn trigger_warning() {}
};

fn ordinary() {}
`)
	trigger, ok := findFact(ff, "pkg.trigger_warning")
	if !ok || trigger.Props["compile_time_context"] != true {
		t.Errorf("const-local function must carry compile_time_context, got %+v", trigger)
	}
	ordinary, ok := findFact(ff, "pkg.ordinary")
	if !ok || ordinary.Props["compile_time_context"] != nil {
		t.Errorf("ordinary function must not carry compile_time_context, got %+v", ordinary)
	}
}

func TestAST_CodexRuntimeRegistrationPatterns(t *testing.T) {
	ff := extractAST(t, `
fn wildcard_callback() {}
fn signal_handler(_: i32) {}
fn host_binding() {}

fn caller(value: Option<()>) {
    value.map(wildcard_callback);
    let _handler = signal_handler as *const ();
    let _provider = Provider { host_binding };
}

struct Provider { host_binding: fn() }

#[ctor::ctor]
fn pre_main() {}

#[divan::bench]
fn benchmark() {}

#[starlark_module]
fn builtins() {
    fn prefix_rule() {}
    fn network_rule() {}
}

unsafe extern "C" {
    fn ForeignFunction(value: i32) -> i32;
}
`)
	caller, ok := findFact(ff, "pkg.caller")
	if !ok {
		t.Fatal("expected caller")
	}
	for _, target := range []string{"pkg.wildcard_callback", "pkg.signal_handler", "pkg.host_binding"} {
		if !hasRelation(caller, facts.RelCalls, target) {
			t.Errorf("caller missing function-value reference to %s: %+v", target, caller.Relations)
		}
	}
	for _, name := range []string{"pkg.pre_main", "pkg.benchmark", "pkg.prefix_rule", "pkg.network_rule"} {
		f, ok := findFact(ff, name)
		if !ok || f.Props["framework_registered"] != true {
			t.Errorf("%s must be framework_registered, got %+v", name, f)
		}
	}
	if _, ok := findFact(ff, "pkg.ForeignFunction"); ok {
		t.Error("extern declaration must not become a removable Rust symbol")
	}
}

func TestAST_CallbackMethodKeepsWildcardImportedFunctionReachable(t *testing.T) {
	ff := extractAST(t, `
mod parent {
    fn callback() {}
    mod child {
        use super::*;
        fn caller(value: Option<()>) { value.map(callback); }
    }
}
`)
	caller, ok := findFact(ff, "pkg.parent.child.caller")
	if !ok {
		t.Fatal("expected caller")
	}
	if !hasRelation(caller, facts.RelCalls, "pkg.parent.child.callback") {
		t.Errorf("wildcard-imported callback must retain a short-name reference: %+v", caller.Relations)
	}
}

func TestAST_CallInsideMacroArgument(t *testing.T) {
	ff := extractAST(t, `
fn caller() {
    bail!(format_missing(&missing));
}
fn format_missing(names: &[String]) -> String { String::new() }
`)
	c, ok := findFact(ff, "pkg.caller")
	if !ok {
		t.Fatal("expected fact for pkg.caller")
	}
	if !hasRelation(c, facts.RelCalls, "pkg.format_missing") {
		t.Errorf("expected RelCalls -> pkg.format_missing, got %+v", c.Relations)
	}
}

func TestAST_MatchesMacroGuardCall(t *testing.T) {
	ff := extractAST(t, `
fn caller(v: Option<u32>) -> bool {
    matches!(v, Some(x) if !is_kw(x))
}
fn is_kw(x: u32) -> bool { false }
`)
	c, ok := findFact(ff, "pkg.caller")
	if !ok {
		t.Fatal("expected fact for pkg.caller")
	}
	if !hasRelation(c, facts.RelCalls, "pkg.is_kw") {
		t.Errorf("expected RelCalls -> pkg.is_kw, got %+v", c.Relations)
	}
}

func TestAST_FunctionPassedAsCallbackArgument(t *testing.T) {
	ff := extractAST(t, `
fn caller(v: Result<u32, String>) {
    v.map_err(handle_error);
}
fn handle_error(e: String) -> String { e }
`)
	c, ok := findFact(ff, "pkg.caller")
	if !ok {
		t.Fatal("expected fact for pkg.caller")
	}
	if !hasRelation(c, facts.RelCalls, "pkg.handle_error") {
		t.Errorf("expected RelCalls -> pkg.handle_error, got %+v", c.Relations)
	}
}

func TestAST_OrdinaryArgument_NoPhantomReference(t *testing.T) {
	ff := extractAST(t, `
pub struct User { id: u32 }
impl User {
    pub fn new(id: u32) -> Self { User { id } }
}
pub fn get_user(user_id: u32) -> User {
    User::new(user_id)
}
`)
	f, ok := findFact(ff, "pkg.get_user")
	if !ok {
		t.Fatal("expected fact for pkg.get_user")
	}
	if hasRelation(f, facts.RelCalls, "pkg.user_id") {
		t.Errorf("user_id is a plain parameter, not a function reference: %+v", f.Relations)
	}
}

func TestAST_FunctionReferenceInStructField(t *testing.T) {
	ff := extractAST(t, `
pub struct Config { diff_fn: fn(&str, &str) -> bool }
fn make() -> Config {
    Config { diff_fn: diff_names }
}
fn diff_names(a: &str, b: &str) -> bool { a == b }
`)
	m, ok := findFact(ff, "pkg.make")
	if !ok {
		t.Fatal("expected fact for pkg.make")
	}
	if !hasRelation(m, facts.RelCalls, "pkg.diff_names") {
		t.Errorf("expected RelCalls -> pkg.diff_names, got %+v", m.Relations)
	}
}

func TestAST_FunctionReferenceTakenByAddress(t *testing.T) {
	ff := extractAST(t, `
fn caller() {
    let f = &helper;
}
fn helper(name: &str) -> Option<String> { None }
`)
	c, ok := findFact(ff, "pkg.caller")
	if !ok {
		t.Fatal("expected fact for pkg.caller")
	}
	if !hasRelation(c, facts.RelCalls, "pkg.helper") {
		t.Errorf("expected RelCalls -> pkg.helper, got %+v", c.Relations)
	}
}

func TestAST_SerdeDefaultAttribute_ReferencesFunction(t *testing.T) {
	ff := extractAST(t, `
pub struct Node {
    #[serde(default = "default_kind")]
    kind: String,
}
fn default_kind() -> String { String::from("model") }
`)
	s, ok := findFact(ff, "pkg.Node")
	if !ok {
		t.Fatal("expected fact for pkg.Node")
	}
	if !hasRelation(s, facts.RelCalls, "default_kind") {
		t.Errorf("expected RelCalls -> default_kind, got %+v", s.Relations)
	}
}

func TestAST_SerdeSkipSerializingIfAttribute_ReferencesFunction(t *testing.T) {
	ff := extractAST(t, `
pub struct Node {
    #[serde(default, skip_serializing_if = "is_false")]
    enabled: bool,
}
fn is_false(b: &bool) -> bool { !b }
`)
	s, ok := findFact(ff, "pkg.Node")
	if !ok {
		t.Fatal("expected fact for pkg.Node")
	}
	if !hasRelation(s, facts.RelCalls, "is_false") {
		t.Errorf("expected RelCalls -> is_false, got %+v", s.Relations)
	}
}

func TestAST_ClapValueParserAttribute_ReferencesFunction(t *testing.T) {
	ff := extractAST(t, `
pub struct Args {
    #[arg(value_parser = check_target)]
    target: String,
}
fn check_target(s: &str) -> Result<String, String> { Ok(s.to_string()) }
`)
	s, ok := findFact(ff, "pkg.Args")
	if !ok {
		t.Fatal("expected fact for pkg.Args")
	}
	if !hasRelation(s, facts.RelCalls, "check_target") {
		t.Errorf("expected RelCalls -> check_target, got %+v", s.Relations)
	}
}

func TestAST_LocalFunctionPassedAsCallbackWithinSameBody(t *testing.T) {
	ff := extractAST(t, `
fn caller(a: Vec<Option<String>>) {
    fn normalize_item(opt: &Option<String>) -> Option<String> { opt.clone() }
    let normalized: Vec<Option<String>> = a.iter().map(normalize_item).collect();
}
`)
	c, ok := findFact(ff, "pkg.caller")
	if !ok {
		t.Fatal("expected fact for pkg.caller")
	}
	if !hasRelation(c, facts.RelCalls, "pkg.normalize_item") {
		t.Errorf("expected RelCalls -> pkg.normalize_item, got %+v", c.Relations)
	}
}

func TestAST_FunctionReferenceNestedInsideMacroArgument(t *testing.T) {
	ff := extractAST(t, `
fn caller() {
    let transforms: Vec<Box<dyn Fn(String) -> String>> = vec![
        Box::new(handle_error),
    ];
}
fn handle_error(s: String) -> String { s }
`)
	c, ok := findFact(ff, "pkg.caller")
	if !ok {
		t.Fatal("expected fact for pkg.caller")
	}
	if !hasRelation(c, facts.RelCalls, "pkg.handle_error") {
		t.Errorf("expected RelCalls -> pkg.handle_error, got %+v", c.Relations)
	}
}

func TestAST_MergeStrategyAttribute_ReferencesScopedFunction(t *testing.T) {
	ff := extractAST(t, `
pub struct Config {
    #[merge(strategy = merge_strategies_extend::overwrite_always)]
    name: Option<String>,
}
mod merge_strategies_extend {
    pub fn overwrite_always(a: &mut Option<String>, b: Option<String>) { *a = b; }
}
`)
	s, ok := findFact(ff, "pkg.Config")
	if !ok {
		t.Fatal("expected fact for pkg.Config")
	}
	if !hasRelation(s, facts.RelCalls, "overwrite_always") {
		t.Errorf("expected RelCalls -> overwrite_always, got %+v", s.Relations)
	}
}

func TestAST_ThiserrorAttributeCall_ReferencesFunction(t *testing.T) {
	ff := extractAST(t, `
#[derive(Debug, thiserror::Error)]
pub enum ProfileError {
    #[error("{}", not_found_message(.searched, .explicit_profiles_dir))]
    NotFound {
        searched: Vec<String>,
        explicit_profiles_dir: bool,
    },
}
fn not_found_message(searched: &[String], explicit: &bool) -> String { String::new() }
`)
	e, ok := findFact(ff, "pkg.ProfileError")
	if !ok {
		t.Fatal("expected fact for pkg.ProfileError")
	}
	if !hasRelation(e, facts.RelCalls, "pkg.not_found_message") {
		t.Errorf("expected RelCalls -> pkg.not_found_message, got %+v", e.Relations)
	}
}

func TestAST_MacroRulesBodyCall_EmitsFileRef(t *testing.T) {
	ff := extractAST(t, `
fn utf8(name: &str) -> String { name.to_string() }

macro_rules! table_schema {
    (@field $name:expr, utf8) => { utf8($name) };
}
`)
	refs := findFactsByKind(ff, facts.KindFileRef)
	if len(refs) != 1 || !hasRelation(refs[0], facts.RelCalls, "pkg.utf8") {
		t.Errorf("expected a KindFileRef -> pkg.utf8, got %+v", refs)
	}
}

func TestAST_ItemLevelMacroInvocationCall_EmitsFileRef(t *testing.T) {
	ff := extractAST(t, `
fn with_cow(x: i32) {}

ffi_fn! {
    fn my_ffi_func(x: i32) {
        with_cow(x);
    }
}
`)
	refs := findFactsByKind(ff, facts.KindFileRef)
	if len(refs) != 1 || !hasRelation(refs[0], facts.RelCalls, "pkg.with_cow") {
		t.Errorf("expected a KindFileRef -> pkg.with_cow, got %+v", refs)
	}
}

func TestAST_DropImplTagsOverride(t *testing.T) {
	ff := extractAST(t, `
pub struct Guard;
impl Drop for Guard {
    fn drop(&mut self) {}
}
`)
	f, ok := findFact(ff, "pkg.Guard.drop")
	if !ok {
		t.Fatal("expected fact for pkg.Guard.drop")
	}
	if f.Props["override"] != true {
		t.Errorf("Drop::drop override = %v, want true", f.Props["override"])
	}
}

func TestAST_FutureImplTagsOverride(t *testing.T) {
	ff := extractAST(t, `
pub struct MyFuture;
impl Future for MyFuture {
    type Output = ();
    fn poll(self: Pin<&mut Self>, cx: &mut Context) -> Poll<()> {
        Poll::Ready(())
    }
}
`)
	f, ok := findFact(ff, "pkg.MyFuture.poll")
	if !ok {
		t.Fatal("expected fact for pkg.MyFuture.poll")
	}
	if f.Props["override"] != true {
		t.Errorf("Future::poll override = %v, want true", f.Props["override"])
	}
}

func TestAST_InherentDropLikeMethod_NotTaggedOverride(t *testing.T) {
	ff := extractAST(t, `
pub struct Cache;
impl Cache {
    fn drop(&mut self) {}
}
`)
	f, ok := findFact(ff, "pkg.Cache.drop")
	if !ok {
		t.Fatal("expected fact for pkg.Cache.drop")
	}
	if f.Props["override"] == true {
		t.Errorf("inherent (non-trait) drop method should not be tagged override, got %v", f.Props["override"])
	}
}

func TestAST_OtherTraitMethodNamedDrop_NotTaggedOverride(t *testing.T) {
	ff := extractAST(t, `
trait Cleaner {
    fn drop(&mut self);
}
pub struct Sweeper;
impl Cleaner for Sweeper {
    fn drop(&mut self) {}
}
`)
	f, ok := findFact(ff, "pkg.Sweeper.drop")
	if !ok {
		t.Fatal("expected fact for pkg.Sweeper.drop")
	}
	if f.Props["override"] == true {
		t.Errorf("a custom trait's drop method (not std::ops::Drop) should not be tagged override, got %v", f.Props["override"])
	}
}

func TestAST_BareScopedVariantValue_InstantiatesEnum(t *testing.T) {
	ff := extractAST(t, `
pub enum UDFKind { Aggregate, Table }
fn classify(s: &str) -> UDFKind {
    match s {
        "Aggregate" => UDFKind::Aggregate,
        _ => UDFKind::Table,
    }
}
`)
	f, ok := findFact(ff, "pkg.classify")
	if !ok {
		t.Fatal("expected fact for pkg.classify")
	}
	if !hasRelation(f, facts.RelInstantiates, "UDFKind") {
		t.Errorf("expected RelInstantiates -> UDFKind, got %+v", f.Relations)
	}
}

func TestAST_ScopedVariantInMatchPattern_InstantiatesEnum(t *testing.T) {
	ff := extractAST(t, `
pub enum HomebrewCmd { Render, Publish }
pub enum Cmd { Homebrew(HomebrewCmd), Other }
fn dispatch(cmd: Cmd) {
    match cmd {
        Cmd::Homebrew(HomebrewCmd::Render) => render(),
        Cmd::Other => other(),
    }
}
`)
	f, ok := findFact(ff, "pkg.dispatch")
	if !ok {
		t.Fatal("expected fact for pkg.dispatch")
	}
	for _, want := range []string{"Cmd", "HomebrewCmd"} {
		if !hasRelation(f, facts.RelInstantiates, want) {
			t.Errorf("expected RelInstantiates -> %s, got %+v", want, f.Relations)
		}
	}
}

func TestAST_ScopedMethodCall_NotTreatedAsVariant(t *testing.T) {
	ff := extractAST(t, `
pub struct EventRecorder;
impl EventRecorder {
    pub fn new() -> Self { EventRecorder }
}
fn make() -> EventRecorder {
    EventRecorder::new()
}
`)
	f, ok := findFact(ff, "pkg.make")
	if !ok {
		t.Fatal("expected fact for pkg.make")
	}
	count := 0
	for _, r := range f.Relations {
		if r.Kind == facts.RelInstantiates && r.Target == "EventRecorder" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 RelInstantiates -> EventRecorder (not double-counted), got %d: %+v", count, f.Relations)
	}
}

func TestAST_BareUnitStructValue_Instantiates(t *testing.T) {
	ff := extractAST(t, `
pub struct SqlCommentSanitizer;
fn register(v: Vec<Box<dyn Sanitizer>>) -> Vec<Box<dyn Sanitizer>> {
    let mut v = v;
    v.push(Box::new(SqlCommentSanitizer));
    v
}
`)
	f, ok := findFact(ff, "pkg.register")
	if !ok {
		t.Fatal("expected fact for pkg.register")
	}
	if !hasRelation(f, facts.RelInstantiates, "SqlCommentSanitizer") {
		t.Errorf("expected RelInstantiates -> SqlCommentSanitizer, got %+v", f.Relations)
	}
}

func TestAST_BareUnitStructValue_LetBinding_Instantiates(t *testing.T) {
	ff := extractAST(t, `
pub struct SqlCommentSanitizer;
fn make() {
    let sanitizer = SqlCommentSanitizer;
}
`)
	f, ok := findFact(ff, "pkg.make")
	if !ok {
		t.Fatal("expected fact for pkg.make")
	}
	if !hasRelation(f, facts.RelInstantiates, "SqlCommentSanitizer") {
		t.Errorf("expected RelInstantiates -> SqlCommentSanitizer, got %+v", f.Relations)
	}
}

func TestAST_TestFnAtFileRoot_NoSymbolFact(t *testing.T) {
	ff := extractAST(t, `
use super::*;

#[test]
fn approvals_reviewer_serializes_auto_review() {
    assert_eq!(1, 1);
}
`)
	if _, ok := findFact(ff, "pkg.approvals_reviewer_serializes_auto_review"); ok {
		t.Error("a #[test] fn at file scope (e.g. a plain tests.rs) should not become a production symbol fact")
	}
}

func TestAST_TestFnAtFileRoot_CreditsProductionCall(t *testing.T) {
	ff := extractAST(t, `
fn helper() {}

#[test]
fn calls_helper() {
    helper();
}
`)
	if _, ok := findFact(ff, "pkg.calls_helper"); ok {
		t.Error("a #[test] fn should not become a symbol fact")
	}
	refs := findFactsByKind(ff, facts.KindTestRef)
	if len(refs) != 1 || !hasRelation(refs[0], facts.RelCalls, "pkg.helper") {
		t.Errorf("expected a KindTestRef -> pkg.helper, got %+v", refs)
	}
}

func TestAST_FunctionPointerArrayLiteral_ReferencesEach(t *testing.T) {
	ff := extractAST(t, `
type Pass = fn(&mut Value);
const PASSES: &[Pass] = &[strip_a, drop_b];
fn strip_a(v: &mut Value) {}
fn drop_b(v: &mut Value) {}
`)
	c, ok := findFact(ff, "pkg.PASSES")
	if !ok {
		t.Fatal("expected fact for pkg.PASSES")
	}
	for _, want := range []string{"pkg.strip_a", "pkg.drop_b"} {
		if !hasRelation(c, facts.RelCalls, want) {
			t.Errorf("expected RelCalls -> %s, got %+v", want, c.Relations)
		}
	}
}

func TestAST_SchemarsSchemaWithAttribute_ReferencesFunction(t *testing.T) {
	ff := extractAST(t, `
pub struct Event {
    #[schemars(schema_with = "event_name_schema")]
    name: String,
}
fn event_name_schema(g: &mut SchemaGenerator) -> Schema { todo!() }
`)
	s, ok := findFact(ff, "pkg.Event")
	if !ok {
		t.Fatal("expected fact for pkg.Event")
	}
	if !hasRelation(s, facts.RelCalls, "event_name_schema") {
		t.Errorf("expected RelCalls -> event_name_schema, got %+v", s.Relations)
	}
}

func TestAST_SchemarsSchemaWithQualifiedPath_ReferencesFunction(t *testing.T) {
	ff := extractAST(t, `
pub struct Features {
    #[schemars(schema_with = "crate::schema::features_schema")]
    flags: Vec<String>,
}
fn features_schema(g: &mut SchemaGenerator) -> Schema { todo!() }
`)
	s, ok := findFact(ff, "pkg.Features")
	if !ok {
		t.Fatal("expected fact for pkg.Features")
	}
	if !hasRelation(s, facts.RelCalls, "features_schema") {
		t.Errorf("expected RelCalls -> features_schema, got %+v", s.Relations)
	}
}

func TestAST_SelfCallAcrossSeparateImplBlocks_NotDropped(t *testing.T) {
	ff := extractAST(t, `
pub struct Foo;
impl Foo {
    fn helper(&self) {}
}
impl Foo {
    fn caller(&self) {
        self.helper();
    }
}
`)
	f, ok := findFact(ff, "pkg.Foo.caller")
	if !ok {
		t.Fatal("expected fact for pkg.Foo.caller")
	}
	if !hasRelation(f, facts.RelCalls, "helper") {
		t.Errorf("expected RelCalls -> helper, got %+v", f.Relations)
	}
}

func TestAST_SelfCallWithinSameImplBlock_StillQualified(t *testing.T) {
	ff := extractAST(t, `
pub struct Foo;
impl Foo {
    fn helper(&self) {}
    fn caller(&self) {
        self.helper();
    }
}
`)
	f, ok := findFact(ff, "pkg.Foo.caller")
	if !ok {
		t.Fatal("expected fact for pkg.Foo.caller")
	}
	if !hasRelation(f, facts.RelCalls, "pkg.Foo.helper") {
		t.Errorf("expected qualified RelCalls -> pkg.Foo.helper, got %+v", f.Relations)
	}
}
