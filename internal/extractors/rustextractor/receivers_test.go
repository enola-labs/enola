package rustextractor

import (
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func rustSym(t *testing.T, ff []facts.Fact, name string) facts.Fact {
	t.Helper()
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Name == name {
			return f
		}
	}
	t.Fatalf("no symbol %s", name)
	return facts.Fact{}
}

func rustCalls(f facts.Fact) []string {
	var out []string
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls {
			out = append(out, r.Target)
		}
	}
	return out
}

// A method called on a receiver whose type the source states resolves to that
// type's method, across files: a parameter seen through &Arc<Self>, a field chain,
// what an associated function returns (through a turbofish), a let's written type,
// and self's method from another impl block. A receiver of unstated type keeps the
// bare name.
func TestRustCallOnAStatedReceiverResolves(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"rt\"\n",
		"src/lib.rs": "mod tasks;\nmod sched;\n",
		"src/tasks.rs": `
pub struct Registry;
impl Registry {
    pub fn register(&self) {}
}
pub struct Frame<T> { cell: T }
impl<T> Frame<T> {
    pub fn from_raw(ptr: usize) -> Frame<T> { todo!() }
    pub fn poll(self) {}
}
`,
		"src/sched.rs": `
use std::sync::Arc;
use crate::tasks::{Frame, Registry};

pub struct Shared { registry: Registry }
pub struct Handle { shared: Shared }

impl Handle {
    fn enqueue(me: &Arc<Self>, other: Thing) {
        me.schedule();
        me.shared.registry.register();
        let frame = Frame::<u8>::from_raw(0);
        frame.poll();
        let reg: &Registry = &me.shared.registry;
        reg.register();
        other.register();
        let value = Default::default();
    }
}

impl Handle {
    fn schedule(&self) {
        self.notify();
    }
}

impl Handle {
    fn notify(&self) {}
}
`,
	})
	bind := rustSym(t, ff, "src.Handle.enqueue")
	calls := rustCalls(bind)
	for _, want := range []string{
		"src.Handle.schedule",   // &Arc<Self>
		"src.Registry.register", // field chain, and a let's written type
		"src.Frame.from_raw",    // an associated function through a turbofish
		"src.Frame.poll",        // what from_raw returns
	} {
		if !slices.Contains(calls, want) {
			t.Errorf("enqueue calls %v, want %s", calls, want)
		}
	}
	if !slices.Contains(calls, "register") {
		t.Errorf("enqueue calls %v, want other.register kept by its bare name", calls)
	}
	if bind.HasRelation(facts.RelInstantiates, "Default") {
		t.Errorf("Default::default() instantiates the trait: %v", bind.Relations)
	}
	if sched := rustSym(t, ff, "src.Handle.schedule"); !slices.Contains(rustCalls(sched), "src.Handle.notify") {
		t.Errorf("schedule calls %v, want self.notify in another impl block", rustCalls(sched))
	}
	for _, f := range ff {
		for _, p := range []string{propTypedCalls, propFieldTypes, propReturns} {
			if _, kept := f.Prop(p); kept {
				t.Errorf("%s: the walker's note %s leaked into the facts", f.Name, p)
			}
		}
	}
}

// A type named through a file module that also has a directory of submodules
// (`worker.rs` beside `worker/`) is declared in the file's directory.
func TestRustFieldTypedThroughAFileModule(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"rt\"\n",
		"src/lib.rs": "mod worker;\nmod handle;\n",
		"src/worker.rs": `
mod metrics;
pub struct Shared;
impl Shared {
    pub fn close(&self) {}
}
`,
		"src/worker/metrics.rs": "pub struct Counters;\n",
		"src/handle.rs": `
use crate::worker;

pub struct Handle { shared: worker::Shared }

impl Handle {
    fn shutdown(&self) {
        self.shared.close();
    }
}
`,
	})
	if sd := rustSym(t, ff, "src.Handle.shutdown"); !slices.Contains(rustCalls(sd), "src.Shared.close") {
		t.Errorf("shutdown calls %v, want src.Shared.close", rustCalls(sd))
	}
}

func TestRustExternalTraitImplMethodsAreDispatchedByTheTrait(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"rt\"\n",
		"src/lib.rs": `
pub trait Local { fn run(&self); }
pub struct Sock;
impl quinn::AsyncUdpSocket for Sock {
    type Outgoing = u8;
    const LIMIT: usize = 1;
    fn poll_send(&self) {}
}
impl Local for Sock {
    type Own = u8;
    fn run(&self) {}
}
impl Sock {
    fn plain(&self) {}
}
`,
	})
	for name, want := range map[string]bool{"Sock.poll_send": true, "Sock.run": false, "Sock.plain": false, "Sock.Outgoing": true, "Sock.LIMIT": true, "Sock.Own": false} {
		got, _ := rustSym(t, ff, "src."+name).Prop("override")
		if (got == true) != want {
			t.Errorf("%s override = %v, want %v", name, got, want)
		}
	}
}

func TestRustTraitImplOverrideLandsOnTheTraitMemberOnly(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"rt\"\n",
		"src/lib.rs": `
pub struct Both;
impl Both { pub fn fmt(&self) {} }
impl std::fmt::Display for Both {
    fn fmt(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result { Ok(()) }
}

pub struct Same;
impl std::fmt::Display for Same { fn fmt(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result { Ok(()) } } impl Same { pub fn fmt(&self) {} }

pub struct N;
impl Default for N { fn default() -> N { fn inner_helper() {} N } }
`,
	})
	var fmts []facts.Fact
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Name == "src.Both.fmt" {
			fmts = append(fmts, f)
		}
	}
	if len(fmts) != 2 {
		t.Fatalf("want 2 fmt facts, got %d", len(fmts))
	}
	for _, f := range fmts {
		isTrait := f.Line == 5
		if got := f.PropAny("override") == true; got != isTrait {
			t.Errorf("fmt at line %d: override=%v", f.Line, got)
		}
	}
	var same []facts.Fact
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Name == "src.Same.fmt" {
			same = append(same, f)
		}
	}
	if len(same) != 2 {
		t.Fatalf("want 2 same-line fmt facts, got %d", len(same))
	}
	if n := countOverrides(same); n != 1 {
		t.Errorf("same-line fmt: %d overrides, want exactly the trait one", n)
	}
	if same[0].PropAny("override") != true || same[1].PropAny("override") == true {
		t.Error("same-line fmt: override must be on the Display impl's fmt (declared first)")
	}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, "inner_helper") && f.PropAny("override") == true {
			t.Errorf("%s is a nested helper, not a trait member", f.Name)
		}
	}
}

func countOverrides(ff []facts.Fact) int {
	n := 0
	for _, f := range ff {
		if f.PropAny("override") == true {
			n++
		}
	}
	return n
}

func TestRustTypePositionsNameTheirTypes(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"rt\"\n",
		"src/lib.rs": `
pub struct Cfg;
pub struct Resp;
pub struct Local;
pub struct Field;
pub struct Param;
pub struct Ret;
pub struct Bound;
pub struct Alias;
pub struct Unused;

pub struct Holder { f: Vec<Field> }
pub type Shorthand = Option<Alias>;

fn sig(p: &Param) -> Result<Ret, ()> where Bound: Sized { todo!() }

fn body() {
    let l: Local = todo!();
    let r = read::<Resp>();
    let c = x as Cfg;
}
`,
	})
	names := func(sym string) []string {
		var out []string
		for _, r := range rustSym(t, ff, sym).Relations {
			if r.Kind == facts.RelNames {
				out = append(out, r.Target)
			}
		}
		return out
	}
	for sym, want := range map[string][]string{
		"src.Holder":    {"Field"},
		"src.Shorthand": {"Alias"},
		"src.sig":       {"Param", "Ret", "Bound"},
		"src.body":      {"Local"},
	} {
		got := names(sym)
		for _, w := range want {
			if !slices.Contains(got, w) {
				t.Errorf("%s names %v, want %s", sym, got, w)
			}
		}
	}
	if got := names("src.body"); !slices.Contains(got, "Resp") {
		t.Errorf("turbofish argument not named: %v", got)
	}
}

func TestRustTypePositionsSkipSelfAndGenericParameters(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"rt\"\n",
		"src/lib.rs": `
pub struct T;
pub struct Item;

pub struct Wrap<T> { f: T }

impl<T: Clone> Wrap<T> {
    pub fn get(&self, other: Self, x: T) -> Item { todo!() }
}

fn generic<T>(x: T) -> Vec<T> { todo!() }
pub trait Bound {}
pub trait Tp<T> { fn dflt(&self, x: T) -> T where T: Copy { x } }
pub trait Sub: Bound {}
pub struct Sig;
pub trait HasSig { fn make(&self) -> Sig; }
fn bounded<U: Bound>(x: U) {}
pub trait WhereS {}
pub trait WhereE {}
pub struct Holder<U> where U: WhereS { value: U }
pub enum Choice<U> where U: WhereE { A(U) }
`,
	})
	for sym, banned := range map[string][]string{
		"src.Wrap":     {"T"},
		"src.Wrap.get": {"T", "Self"},
		"src.generic":  {"T"},
		"src.Tp.dflt":  {"T"},
		"src.Tp":       {"T"},
	} {
		for _, r := range rustSym(t, ff, sym).Relations {
			if r.Kind == facts.RelNames && slices.Contains(banned, r.Target) {
				t.Errorf("%s names %s", sym, r.Target)
			}
		}
	}
	found := false
	for _, r := range rustSym(t, ff, "src.Wrap.get").Relations {
		found = found || (r.Kind == facts.RelNames && r.Target == "Item")
	}
	if !found {
		t.Error("src.Wrap.get does not name Item")
	}
	var sig bool
	for _, r := range rustSym(t, ff, "src.HasSig.make").Relations {
		sig = sig || (r.Kind == facts.RelNames && r.Target == "Sig")
	}
	if !sig {
		t.Error("a trait method signature does not name its return type")
	}
	var supers bool
	for _, r := range rustSym(t, ff, "src.Sub").Relations {
		supers = supers || (r.Kind == facts.RelNames && r.Target == "Bound")
	}
	if !supers {
		t.Error("a trait does not name its supertrait")
	}
	var bounded bool
	for _, r := range rustSym(t, ff, "src.bounded").Relations {
		bounded = bounded || (r.Kind == facts.RelNames && r.Target == "Bound")
	}
	if !bounded {
		t.Error("src.bounded does not name its inline bound")
	}
	for sym, want := range map[string]string{"src.Holder": "WhereS", "src.Choice": "WhereE"} {
		var ok bool
		for _, r := range rustSym(t, ff, sym).Relations {
			ok = ok || (r.Kind == facts.RelNames && r.Target == want)
		}
		if !ok {
			t.Errorf("%s does not name %s from its where clause", sym, want)
		}
	}
}

func TestRustTypeNamesKeepPathContext(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"rt\"\n",
		"src/lib.rs": `
pub mod models;
pub mod imp;
pub mod ord;
pub struct String;
pub fn first() { use std::string::String; let _x: Option<String> = None; }
pub fn second(x: String) {}
pub struct Item;
pub struct Vec;
pub trait Has { type Item; }
pub trait OnlyImpl {}
pub struct Holder<T>(T);

pub fn assoc<T: Has>(x: T::Item) {}
pub fn qualified<T: Has>(x: <T as Has>::Item) {}
pub fn local_vec(x: Vec) {}
pub fn std_vec(x: std::vec::Vec<u8>, y: Option<u8>) {}
pub struct OnlyConst;
pub const C: Option<OnlyConst> = None;
pub struct OnlyStatic;
pub static S: Option<OnlyStatic> = None;
pub fn module_path(x: crate::models::Input) {}

impl<T: OnlyImpl> Holder<T> {
    pub fn get(&self) {}
}
`,
		"src/models.rs": "pub struct Input;\n",
		"src/ord.rs":    "pub fn before(x: Vec<u8>) {}\nuse std::vec::Vec;\npub fn after(x: Vec<u8>) {}\n",
		"src/imp.rs":    "use std::vec::Vec;\npub fn imported(x: Vec<u8>) {}\npub fn absolute(x: ::std::vec::Vec<u8>) {}\npub mod local { pub struct Vec; }\npub fn qualified(x: crate::imp::local::Vec) {}\npub mod left { use std::vec::Vec; }\npub mod right { pub struct Vec; pub fn uses(x: Vec) {} }\n",
	})
	names := func(sym string) []string {
		var out []string
		for _, r := range rustSym(t, ff, sym).Relations {
			if r.Kind == facts.RelNames {
				out = append(out, r.Target)
			}
		}
		return out
	}
	for _, sym := range []string{"src.assoc", "src.qualified"} {
		if got := names(sym); slices.Contains(got, "Item") {
			t.Errorf("%s names %v: an associated type is not a declaration called Item", sym, got)
		}
	}
	if got := names("src.local_vec"); !slices.Contains(got, "Vec") {
		t.Errorf("a repository type named Vec must be referenced, got %v", got)
	}
	if got := names("src.std_vec"); slices.Contains(got, "Vec") {
		t.Errorf("std::vec::Vec must not name the repository's Vec, got %v", got)
	}
	if got := names("src.qualified"); !slices.Contains(got, "Vec") {
		t.Errorf("a module-qualified local Vec must be named despite an import of std's Vec, got %v", got)
	}
	if got := names("src.right.uses"); !slices.Contains(got, "Vec") {
		t.Errorf("an import in a sibling module must not hide the local Vec, got %v", got)
	}
	if got := names("src.second"); !slices.Contains(got, "String") {
		t.Errorf("a use inside another function must not hide the local String, got %v", got)
	}
	for _, sym := range []string{"src.before", "src.after"} {
		if got := names(sym); slices.Contains(got, "Vec") {
			t.Errorf("%s names %v: the module imports std's Vec, whatever the order", sym, got)
		}
	}
	for _, sym := range []string{"src.imported", "src.absolute"} {
		if got := names(sym); slices.Contains(got, "Vec") {
			t.Errorf("%s names %v: Vec is the standard library's here", sym, got)
		}
	}
	for _, f := range ff {
		if f.Kind != facts.KindFileRef {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames && r.Target == "Item" {
				t.Errorf("%s names Item: an associated type's declaration is not a use", f.Name)
			}
		}
	}
	for sym, want := range map[string]string{"src.C": "OnlyConst", "src.S": "OnlyStatic"} {
		if got := names(sym); !slices.Contains(got, want) {
			t.Errorf("%s names %v, want %s from its annotation", sym, got, want)
		}
	}
	if got := names("src.std_vec"); slices.Contains(got, "Option") {
		t.Errorf("Option is not declared in the repository, got %v", got)
	}
	if got := names("src.module_path"); !slices.Contains(got, "Input") {
		t.Errorf("a module-qualified type must still be named, got %v", got)
	}
	var onlyImpl bool
	for _, f := range ff {
		if f.Kind != facts.KindFileRef {
			continue
		}
		for _, r := range f.Relations {
			onlyImpl = onlyImpl || (r.Kind == facts.RelNames && r.Target == "OnlyImpl")
		}
	}
	if !onlyImpl {
		t.Error("a bound on an impl header is not referenced")
	}
}

func TestRustScopedVariantLiteralStillInstantiatesTheEnum(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"rt\"\n",
		"src/lib.rs": `
pub enum Notification { Done { id: u32 } }
pub fn build() -> Notification { Notification::Done { id: 1 } }
`,
	})
	var instantiates bool
	for _, r := range rustSym(t, ff, "src.build").Relations {
		instantiates = instantiates || (r.Kind == facts.RelInstantiates && r.Target == "Notification")
	}
	if !instantiates {
		t.Errorf("src.build does not instantiate Notification: %v", rustSym(t, ff, "src.build").Relations)
	}
}

func TestRustCallThroughAnImportAliasReachesTheOriginal(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"rt\"\n",
		"src/lib.rs": "mod audio;\nmod session;\n",
		"src/audio.rs": `
pub fn handle_audio() {}
pub fn handle_text() {}
`,
		"src/session.rs": `
use crate::audio::{handle_audio as handle_realtime_audio, handle_text};

pub fn run() {
    handle_realtime_audio();
    handle_text();
}
`,
	})
	if got := rustCalls(rustSym(t, ff, "src.run")); !slices.Contains(got, "src.handle_audio") {
		t.Errorf("a call through the alias must reach handle_audio, got %v", got)
	}
}

func TestRustImportAliasesBindOnlyTheAlias(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml":     "[package]\nname = \"rt\"\n",
		"src/lib.rs":     "mod a;\nmod b;\nmod user;\nmod foo;\nuse crate::recurse as again;\npub fn recurse() { again(); }\n",
		"src/foo/mod.rs": "use crate::foo::recurse as again;\npub fn recurse() { again(); }\n",
		"src/a.rs":       "pub fn go_fast() {}\npub fn run() {}\n",
		"src/b.rs":       "pub fn run() {}\n",
		"src/user.rs": `
use crate::a::go_fast;
use std::thread::go_fast as ext_fast;
use std::io::{self as sio};
use crate::b::run as run_b;

pub fn caller() {
    go_fast();
    ext_fast();
    sio::stdin();
}

pub fn run() {
    run_b();
}
`,
	})
	caller := rustCalls(rustSym(t, ff, "src.caller"))
	if !slices.Contains(caller, "src.go_fast") {
		t.Errorf("a later alias must not take over the earlier import, got %v", caller)
	}
	for _, c := range caller {
		if strings.HasSuffix(c, ".self") {
			t.Errorf("`self as` produced the phantom target %s", c)
		}
	}
	if rustSym(t, ff, "src.recurse").Props["recursive_self"] != true {
		t.Error("a call through an alias of the function itself is recursion")
	}
	if rustSym(t, ff, "src/foo.recurse").Props["recursive_self"] != true {
		t.Error("an alias of the function itself inside mod.rs is recursion")
	}
	if rustSym(t, ff, "src.run").Props["recursive_self"] == true {
		t.Error("a call through an alias of a same-named function in another file is not recursion")
	}
}

func TestRustImportedTypeThroughAFileModuleWithADirectory(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml":           "[workspace]\nmembers=[\"a\",\"b\"]\n",
		"a/Cargo.toml":         "[package]\nname = \"a\"\n",
		"a/src/lib.rs":         "pub mod rustc;\n",
		"a/src/rustc.rs":       "pub mod print;\npub struct Real { pub tcx: Ctx }\npub struct Ctx;\nimpl Ctx { pub fn dcx(&self) {} }\n",
		"a/src/rustc/print.rs": "pub fn p() {}\n",
		"b/Cargo.toml":         "[package]\nname = \"b\"\n",
		"b/src/lib.rs":         "use a::rustc::{Real as Alias};\nuse a::rustc::Real;\npub fn f(cx: &Alias) { cx.tcx.dcx(); }\npub fn h(cx: &Real) { cx.tcx.dcx(); }\n",
	})
	for _, fn := range []string{"b/src.f", "b/src.h"} {
		if got := rustCalls(rustSym(t, ff, fn)); !slices.Contains(got, "a/src.Ctx.dcx") {
			t.Errorf("%s calls %v, want a/src.Ctx.dcx", fn, got)
		}
	}
}

func TestRustMethodsInAFileModulesSubdirectoryResolve(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml":                  "[workspace]\nmembers=[\"s\",\"u\"]\n",
		"s/Cargo.toml":                "[package]\nname = \"s\"\n",
		"s/src/lib.rs":                "pub mod ast;\n",
		"s/src/ast/mod.rs":            "pub mod syntax_factory;\n",
		"s/src/ast/syntax_factory.rs": "mod constructors;\npub struct SyntaxFactory;\n",
		"s/src/ast/syntax_factory/constructors.rs": "use super::SyntaxFactory;\nimpl SyntaxFactory { pub fn expr_paren(&self) {} }\n",
		"u/Cargo.toml": "[package]\nname = \"u\"\n",
		"u/src/lib.rs": "use s::ast::syntax_factory::SyntaxFactory;\npub fn f(make: &SyntaxFactory) { make.expr_paren(); }\n",
	})
	got := rustCalls(rustSym(t, ff, "u/src.f"))
	if !slices.Contains(got, "s/src/ast/syntax_factory.SyntaxFactory.expr_paren") {
		t.Errorf("u/src.f calls %v, want the method declared in the file module's subdirectory", got)
	}
}

func TestRustTypeImportedThroughAReExportResolves(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml":         "[workspace]\nmembers=[\"m\",\"u\"]\n",
		"m/Cargo.toml":       "[package]\nname = \"m\"\n",
		"m/src/lib.rs":       "pub mod value;\npub use value::Value;\n",
		"m/src/value/mod.rs": "pub struct Value;\nimpl Value { pub fn make(&self) {} }\n",
		"u/Cargo.toml":       "[package]\nname = \"u\"\n",
		"u/src/lib.rs":       "use m::Value;\npub fn f(v: &Value) { v.make(); }\n",
	})
	if got := rustCalls(rustSym(t, ff, "u/src.f")); !slices.Contains(got, "m/src/value.Value.make") {
		t.Errorf("u/src.f calls %v, want m/src/value.Value.make", got)
	}
}

func TestRustTypeAliasIsFollowedToItsType(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"rt\"\n",
		"src/lib.rs": "mod ct;\nmod tui;\n",
		"src/ct.rs":  "pub struct Terminal<B> { b: B }\nimpl<B> Terminal<B> { pub fn invalidate(&mut self) {} }\n",
		"src/tui.rs": `
use crate::ct::Terminal as CustomTerminal;
pub type Terminal = CustomTerminal<u8>;
pub struct Tui { terminal: Terminal }
impl Tui { pub fn draw(&mut self) { self.terminal.invalidate(); } }
`,
	})
	if got := rustCalls(rustSym(t, ff, "src.Tui.draw")); !slices.Contains(got, "src.Terminal.invalidate") {
		t.Errorf("src.Tui.draw calls %v, want the method of the aliased type", got)
	}
}

func TestRustAliasedExternalImportDoesNotHideTheLocalName(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"rt\"\n",
		"src/lib.rs": `
use std::vec::Vec as StdVec;
use std::string::String as _;

pub struct Vec;
pub struct StdVecLocal;

pub fn local(x: Vec) {}
pub fn aliased(x: StdVec<u8>) {}
`,
	})
	names := func(sym string) []string {
		var out []string
		for _, r := range rustSym(t, ff, sym).Relations {
			if r.Kind == facts.RelNames {
				out = append(out, r.Target)
			}
		}
		return out
	}
	if got := names("src.local"); !slices.Contains(got, "Vec") {
		t.Errorf("an aliased import of std's Vec must not hide the local Vec, got %v", got)
	}
	if got := names("src.aliased"); slices.Contains(got, "StdVec") {
		t.Errorf("an alias of an outside import names no declaration, got %v", got)
	}
}

func TestRustGenericParametersOfEveryKindAreNotTypes(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"rt\"\n",
		"src/lib.rs": `
use std::sync::Arc;

pub struct N;
pub struct U;
pub struct T;
pub struct K;
pub struct Arr<const M: usize>;

pub fn const_generic<const N: usize>(x: Arr<N>) {}

pub union UU<T: Copy> { a: T }

pub trait Am { type Out<U>; }

pub type Wrap<K> = Arc<K>;
impl K { pub fn k_method(&self) {} }
pub fn through_alias(w: Wrap<u8>) { w.k_method(); }
`,
	})
	names := func(sym string) []string {
		var out []string
		for _, r := range rustSym(t, ff, sym).Relations {
			if r.Kind == facts.RelNames {
				out = append(out, r.Target)
			}
		}
		return out
	}
	if got := names("src.const_generic"); slices.Contains(got, "N") || !slices.Contains(got, "Arr") {
		t.Errorf("a const generic is not a type: src.const_generic names %v, want Arr and not N", got)
	}
	if rustSym(t, ff, "src.UU").PropAny("symbol_kind") == nil {
		t.Error("a union is a declaration")
	}
	if rustSym(t, ff, "src.UU").PropAny("union") != true {
		t.Error("a union is marked union")
	}
	if rustSym(t, ff, "src.N").PropAny("union") != nil {
		t.Error("a struct is not marked union")
	}
	if got := names("src.UU"); slices.Contains(got, "T") {
		t.Errorf("the union's own generic parameter is not a type, got %v", got)
	}
	for _, f := range ff {
		if f.Kind != facts.KindFileRef {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames && (r.Target == "T" || r.Target == "UU" || r.Target == "U" || r.Target == "Copy") {
				t.Errorf("%s names %s: a union or a generic associated type leaks onto the file", f.Name, r.Target)
			}
		}
	}
	if got := rustCalls(rustSym(t, ff, "src.through_alias")); slices.Contains(got, "src.K.k_method") {
		t.Errorf("an alias parameter is not the local struct K, but src.through_alias calls %v", got)
	}
}

func TestRustGenericParameterIsNotTheLocalTypeOfTheSameName(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"rt\"\n",
		"src/lib.rs": `
pub struct K;
impl K { pub fn k_method(&self) {} pub fn new() -> K { K } }
pub fn param<K>(x: K) { x.k_method(); }
pub fn assoc<K: Default>() { K::new(); }
pub struct Fac;
impl Fac { pub fn make<K>() -> K { todo!() } }
pub fn caller() { Fac::make().k_method(); }
pub struct W<K>(K);
impl<K> W<K> { pub fn get(&self, x: K) { x.k_method(); } }
pub fn real(x: K) { x.k_method(); }
`,
	})
	for _, sym := range []string{"src.param", "src.assoc", "src.caller", "src.W.get"} {
		if got := rustCalls(rustSym(t, ff, sym)); slices.Contains(got, "src.K.k_method") || slices.Contains(got, "src.K.new") {
			t.Errorf("a generic parameter K is not the local struct K, but %s calls %v", sym, got)
		}
	}
	if got := rustCalls(rustSym(t, ff, "src.real")); !slices.Contains(got, "src.K.k_method") {
		t.Errorf("the local struct K still resolves: src.real calls %v", got)
	}
}

func TestRustItemLevelErrorAttributeCallsAreReferences(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"rt\"\n",
		"src/lib.rs": `
pub fn describe(x: &str) -> String { x.to_string() }
pub fn from_message(x: &str) -> String { x.to_string() }
pub fn never_called() {}

#[derive(Debug, Error)]
#[error(
    "[{}] failed {}",
    self.code(),
    describe(&self.reason)
)]
pub struct StructError { reason: String }

#[derive(Debug, Error)]
#[error("{}", from_message(msg))]
pub enum EnumError { Bad { msg: String } }
`,
	})
	if got := rustCalls(rustSym(t, ff, "src.StructError")); !slices.Contains(got, "src.describe") {
		t.Errorf("src.StructError calls %v, want src.describe from its #[error(..)]", got)
	}
	if got := rustCalls(rustSym(t, ff, "src.EnumError")); !slices.Contains(got, "src.from_message") {
		t.Errorf("src.EnumError calls %v, want src.from_message from its #[error(..)]", got)
	}
	for _, sym := range []string{"src.StructError", "src.EnumError"} {
		if got := rustCalls(rustSym(t, ff, sym)); slices.Contains(got, "src.never_called") {
			t.Errorf("%s calls %v", sym, got)
		}
	}
}

func TestRustFeatureGatedTwinsKeepTheLocalNameReferenced(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"rt\"\n",
		"src/lib.rs": `
#[cfg(feature = "full")]
pub use std::vec::Vec as PropertyValue;
#[cfg(not(feature = "full"))]
pub type PropertyValue = ();

pub struct Settings { extra: Option<PropertyValue> }
pub fn read(x: &PropertyValue) {}
`,
	})
	names := func(sym string) []string {
		var out []string
		for _, r := range rustSym(t, ff, sym).Relations {
			if r.Kind == facts.RelNames {
				out = append(out, r.Target)
			}
		}
		return out
	}
	for _, sym := range []string{"src.Settings", "src.read"} {
		if got := names(sym); !slices.Contains(got, "PropertyValue") {
			t.Errorf("%s names %v: the local twin of an imported name must stay referenced", sym, got)
		}
	}
}

func TestRustCratesAreFoundByLibNameAndByDependencyRename(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml":      "[workspace]\nmembers=[\"deps\",\"core\",\"app\"]\n",
		"deps/Cargo.toml": "[package]\nname = \"my-deps\"\n[lib]\nname = \"fs_deps\"\n",
		"deps/src/lib.rs": "pub fn install() {}\n",
		"core/Cargo.toml": "[package]\nname = \"real-core\"\n",
		"core/src/lib.rs": "pub fn boot() {}\n",
		"app/Cargo.toml":  "[package]\nname = \"app\"\n[dependencies]\nshort = { path = \"../core\", package = \"real-core\" }\n",
		"app/src/lib.rs":  "use fs_deps::install;\nuse short::boot;\npub fn run() { install(); boot(); }\n",
	})
	got := rustCalls(rustSym(t, ff, "app/src.run"))
	for _, want := range []string{"deps/src.install", "core/src.boot"} {
		if !slices.Contains(got, want) {
			t.Errorf("app/src.run calls %v, want %s", got, want)
		}
	}
}

func TestRustUseThroughAnImportedModuleResolves(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml":                    "[package]\nname = \"rt\"\n",
		"src/lib.rs":                    "mod runtime;\n",
		"src/runtime/mod.rs":            "pub mod task;\npub mod sched;\n",
		"src/runtime/task/mod.rs":       "pub mod trace;\n",
		"src/runtime/task/trace/mod.rs": "pub fn trace_current_thread() {}\n",
		"src/runtime/sched.rs":          "use crate::runtime::task;\npub fn run() {\n    use task::trace::trace_current_thread;\n    trace_current_thread();\n}\n",
	})
	if got := rustCalls(rustSym(t, ff, "src/runtime.run")); !slices.Contains(got, "src/runtime/task/trace.trace_current_thread") {
		t.Errorf("src/runtime.run calls %v, want the function behind the imported module", got)
	}
}

func TestRustUseThroughAReExportedCrateIsNotAnImportedModule(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml":               "[workspace]\nmembers=[\"tokio\",\"tests-build\",\"wstore\"]\n",
		"tokio/Cargo.toml":         "[package]\nname = \"tokio\"\n",
		"tokio/src/lib.rs":         "pub mod runtime;\n",
		"tokio/src/runtime/mod.rs": "pub fn spawn() {}\n",
		"tests-build/Cargo.toml":   "[package]\nname = \"tests-build\"\n\n[dependencies]\ntokio = { path = \"../tokio\" }\n",
		"tests-build/src/lib.rs":   "pub use tokio;\n",
		"tests-build/tests/a.rs":   "use tests_build::tokio;\nuse tokio::runtime::spawn;\nfn t() { spawn(); }\n",
		"wstore/Cargo.toml":        "[package]\nname = \"wstore\"\n",
		"wstore/src/lib.rs":        "pub use rusqlite;\n",
		"wstore/tests/b.rs":        "use wstore::rusqlite;\nuse rusqlite::types::FromSql;\n",
	})
	imports := func(file string) []string {
		var out []string
		for _, f := range ff {
			if f.Kind == facts.KindDependency && f.File == file {
				for _, r := range f.Relations {
					out = append(out, r.Target)
				}
			}
		}
		return out
	}
	if got := imports("tests-build/tests/a.rs"); !slices.Contains(got, "tokio/src/runtime") {
		t.Errorf("use tokio::runtime through a re-exported crate imports %v, want tokio/src/runtime", got)
	}
	for _, target := range imports("wstore/tests/b.rs") {
		if strings.HasPrefix(target, "wstore") && strings.Contains(target, "types") {
			t.Errorf("an external crate re-exported by wstore is imported as %s", target)
		}
	}
}

func TestRustDependencyRenamesBeatUnrelatedPackagesOfTheSameName(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml":       "[workspace]\nmembers=[\"short\",\"real\",\"tbl\",\"app\"]\n",
		"short/Cargo.toml": "[package]\nname = \"short\"\n",
		"short/src/lib.rs": "pub fn boot() {}\n",
		"real/Cargo.toml":  "[package]\nname = \"real-core\"\n",
		"real/src/lib.rs":  "pub fn boot() {}\n",
		"tbl/Cargo.toml":   "[package]\nname = \"table-core\"\n",
		"tbl/src/lib.rs":   "pub fn spin() {}\n",
		"app/Cargo.toml": `[package]
name = "app"

[dependencies]
short = { path = "../real", package = "real-core" }

[dependencies.viatable]
path = "../tbl"
package = "table-core"
`,
		"app/src/lib.rs": "use short::boot;\nuse viatable::spin;\npub fn run() { boot(); spin(); }\n",
	})
	got := rustCalls(rustSym(t, ff, "app/src.run"))
	if !slices.Contains(got, "real/src.boot") || slices.Contains(got, "short/src.boot") {
		t.Errorf("a rename must win over an unrelated package named short, got %v", got)
	}
	if !slices.Contains(got, "tbl/src.spin") {
		t.Errorf("a [dependencies.viatable] rename is ignored, got %v", got)
	}
}

func TestRustRenamedFacadeKeepsNamingTheRealCrate(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml":        "[workspace]\nmembers=[\"core\",\"facade\",\"std\"]\n",
		"core/Cargo.toml":   "[package]\nname = \"core\"\n",
		"core/src/lib.rs":   "pub fn null_mut() {}\n",
		"facade/Cargo.toml": "[package]\nname = \"workspace-core\"\n[dependencies]\ncore = { path = \"../core\" }\n",
		"facade/src/lib.rs": "pub use core::*;\n",
		"std/Cargo.toml":    "[package]\nname = \"std\"\n[dependencies]\ncore = { path = \"../facade\", package = \"workspace-core\" }\n",
		"std/src/lib.rs":    "use core::null_mut;\npub fn run() { null_mut(); }\n",
	})
	if got := rustCalls(rustSym(t, ff, "std/src.run")); !slices.Contains(got, "core/src.null_mut") {
		t.Errorf("std/src.run calls %v, want the function in the real core crate", got)
	}
}

func TestRustWorkspaceInheritedAndDottedRenamesResolve(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": `[workspace]
members = ["short", "real", "tbl", "dot", "app"]

[workspace.dependencies]
inline = { path = "real", package = "real-core" }
dotted.path = "dot"
dotted.package = "dot-core"

[workspace.dependencies.viatable]
path = "tbl"
package = "table-core"

[package]
name = "root"
`,
		"short/Cargo.toml": "[package]\nname = \"inline\"\n",
		"short/src/lib.rs": "pub fn boot() {}\n",
		"real/Cargo.toml":  "[package]\nname = \"real-core\"\n",
		"real/src/lib.rs":  "pub fn boot() {}\n",
		"tbl/Cargo.toml":   "[package]\nname = \"table-core\"\n",
		"tbl/src/lib.rs":   "pub fn spin() {}\n",
		"dot/Cargo.toml":   "[package]\nname = \"dot-core\"\n",
		"dot/src/lib.rs":   "pub fn dial() {}\n",
		"app/Cargo.toml": `[package]
name = "app"

[dependencies]
inline.workspace = true
dotted = { workspace = true }

[dependencies.viatable]
workspace = true
`,
		"app/src/lib.rs": "use inline::boot;\nuse viatable::spin;\nuse dotted::dial;\npub fn run() { boot(); spin(); dial(); }\n",
	})
	got := rustCalls(rustSym(t, ff, "app/src.run"))
	for _, want := range []string{"real/src.boot", "tbl/src.spin", "dot/src.dial"} {
		if !slices.Contains(got, want) {
			t.Errorf("a workspace rename inherited by a member is not applied: want %s in %v", want, got)
		}
	}
	if slices.Contains(got, "short/src.boot") {
		t.Errorf("the workspace rename must win over the package named inline, got %v", got)
	}
}

func TestRustDottedPackageRenameInADependenciesSection(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml":       "[workspace]\nmembers=[\"short\",\"real\",\"app\"]\n",
		"short/Cargo.toml": "[package]\nname = \"short\"\n",
		"short/src/lib.rs": "pub fn boot() {}\n",
		"real/Cargo.toml":  "[package]\nname = \"real-core\"\n",
		"real/src/lib.rs":  "pub fn boot() {}\n",
		"app/Cargo.toml":   "[package]\nname = \"app\"\n\n[dependencies]\nshort.path = \"../real\"\nshort.package = \"real-core\"\n",
		"app/src/lib.rs":   "use short::boot;\npub fn run() { boot(); }\n",
	})
	got := rustCalls(rustSym(t, ff, "app/src.run"))
	if !slices.Contains(got, "real/src.boot") || slices.Contains(got, "short/src.boot") {
		t.Errorf("short.package = \"real-core\" is not read as a rename, got %v", got)
	}
}

func TestRustAnImportInsideAModuleDoesNotRouteAUseOutsideIt(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml":         "[package]\nname = \"app\"\n",
		"src/lib.rs":         "mod a;\nmod b;\n",
		"src/a/mod.rs":       "pub mod serde;\n",
		"src/a/serde/mod.rs": "pub fn local() {}\n",
		"src/b.rs":           "mod inner {\n    use crate::a::serde;\n    pub fn f() { serde::local(); }\n}\nuse serde::Deserialize;\npub fn g() {}\n",
	})
	for _, f := range ff {
		if f.Kind == facts.KindDependency && f.File == "src/b.rs" && strings.Contains(f.Name, "Deserialize") {
			for _, r := range f.Relations {
				if strings.HasPrefix(r.Target, "src/a") {
					t.Errorf("use serde::Deserialize outside the module that imported crate::a::serde resolved into the repository: %v", f)
				}
			}
		}
	}
}

func TestRustUseThroughAnImportedFileModuleResolves(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml":                "[package]\nname = \"rt\"\n",
		"src/lib.rs":                "mod runtime;\n",
		"src/runtime/mod.rs":        "pub mod task;\npub mod sched;\n",
		"src/runtime/task.rs":       "pub mod trace;\n",
		"src/runtime/task/trace.rs": "pub fn trace_current_thread() {}\n",
		"src/runtime/sched.rs":      "use crate::runtime::task;\npub fn run() {\n    use task::trace::trace_current_thread;\n    trace_current_thread();\n}\n",
	})
	if got := rustCalls(rustSym(t, ff, "src/runtime.run")); !slices.Contains(got, "src/runtime/task.trace_current_thread") {
		t.Errorf("src/runtime.run calls %v, want the function behind the imported file module", got)
	}
}
