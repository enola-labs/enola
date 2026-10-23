package goextractor

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// --- helpers ---

// setupGoProject creates a temp directory with go.mod and Go source files.
// Returns the repo path and a cleanup function.
func setupGoProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()

	// Create go.mod
	gomod := "module testmod\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create source files
	for relPath, content := range files {
		absPath := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func extractAll(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := setupGoProject(t, files)

	// Collect relative file paths
	var relFiles []string
	for f := range files {
		relFiles = append(relFiles, f)
	}

	ext := New()
	result, err := ext.Extract(context.Background(), dir, relFiles)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return result
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
	var result []facts.Fact
	for _, f := range ff {
		if f.Kind == kind {
			result = append(result, f)
		}
	}
	return result
}

func hasRelation(f facts.Fact, relKind, target string) bool {
	for _, r := range f.Relations {
		if r.Kind == relKind && r.Target == target {
			return true
		}
	}
	return false
}

// --- tests ---

func TestExtract_FunctionAndMethod(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/foo.go": `package pkg

func Foo() {}

type Store struct{}

func (s *Store) Bar() {}
`,
	})

	// Check exported function
	foo, ok := findFact(ff, "pkg.Foo")
	if !ok {
		t.Fatal("expected fact for pkg.Foo")
	}
	if foo.Props["symbol_kind"] != facts.SymbolFunc {
		t.Errorf("Foo symbol_kind = %v, want function", foo.Props["symbol_kind"])
	}
	if foo.Props["exported"] != true {
		t.Errorf("Foo exported = %v, want true", foo.Props["exported"])
	}

	// Check method with pointer receiver
	bar, ok := findFact(ff, "pkg.Store.Bar")
	if !ok {
		t.Fatal("expected fact for pkg.Store.Bar")
	}
	if bar.Props["symbol_kind"] != facts.SymbolMethod {
		t.Errorf("Bar symbol_kind = %v, want method", bar.Props["symbol_kind"])
	}
	if bar.Props["receiver"] != "Store" {
		t.Errorf("Bar receiver = %v, want Store (star should be stripped)", bar.Props["receiver"])
	}
	if bar.Props["exported"] != true {
		t.Errorf("Bar exported = %v, want true", bar.Props["exported"])
	}
}

func TestExtract_UnexportedSymbols(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/internal.go": `package pkg

func helper() {}

type myType struct{}
`,
	})

	fn, ok := findFact(ff, "pkg.helper")
	if !ok {
		t.Fatal("expected fact for pkg.helper")
	}
	if fn.Props["exported"] != false {
		t.Errorf("helper exported = %v, want false", fn.Props["exported"])
	}

	ty, ok := findFact(ff, "pkg.myType")
	if !ok {
		t.Fatal("expected fact for pkg.myType")
	}
	if ty.Props["exported"] != false {
		t.Errorf("myType exported = %v, want false", ty.Props["exported"])
	}
}

// TestExtract_ExportedConstsAndVarsAreSymbols pins the declared-surface rule:
// exported package-level consts and vars emit symbols — a const-only file
// (a shared vocabulary, a sentinel-error set) is measured, not invisible —
// while unexported bindings and function-local declarations stay out.
func TestExtract_ExportedConstsAndVarsAreSymbols(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/contract.go": `package pkg

const (
	PropSource = "source"
	viaDefault = "http"
)

var ErrNotFound = mkErr("not found")

func mkErr(s string) error { return nil }

func withLocal() {
	const local = 1
	_ = local
}
`,
	})

	c, ok := findFact(ff, "pkg.PropSource")
	if !ok {
		t.Fatal("expected fact for pkg.PropSource — a const-only surface must be measured")
	}
	if c.Props["symbol_kind"] != facts.SymbolConstant {
		t.Errorf("PropSource symbol_kind = %v, want constant", c.Props["symbol_kind"])
	}
	if c.File != "pkg/contract.go" {
		t.Errorf("PropSource file = %v, want pkg/contract.go", c.File)
	}

	v, ok := findFact(ff, "pkg.ErrNotFound")
	if !ok {
		t.Fatal("expected fact for pkg.ErrNotFound")
	}
	if v.Props["symbol_kind"] != facts.SymbolVariable {
		t.Errorf("ErrNotFound symbol_kind = %v, want variable", v.Props["symbol_kind"])
	}

	if _, ok := findFact(ff, "pkg.viaDefault"); ok {
		t.Error("unexported const must not emit a symbol")
	}
	if _, ok := findFact(ff, "pkg.local"); ok {
		t.Error("function-local const must not emit a symbol")
	}
}

func TestExtract_StructWithEmbedding(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/embed.go": `package pkg

import "io"

type Foo struct {
	io.Reader
	Bar
	Name string
}

type Bar struct{}
`,
	})

	foo, ok := findFact(ff, "pkg.Foo")
	if !ok {
		t.Fatal("expected fact for pkg.Foo")
	}
	if foo.Props["symbol_kind"] != facts.SymbolStruct {
		t.Errorf("Foo symbol_kind = %v, want struct", foo.Props["symbol_kind"])
	}

	// Should have implements relations for embedded types
	if !hasRelation(foo, facts.RelImplements, "io.Reader") {
		t.Error("Foo should have implements relation for io.Reader")
	}
	if !hasRelation(foo, facts.RelImplements, "Bar") {
		t.Error("Foo should have implements relation for Bar")
	}
}

func TestExtract_InterfaceDeclaration(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/iface.go": `package pkg

type Fooer interface {
	Foo() error
}
`,
	})

	iface, ok := findFact(ff, "pkg.Fooer")
	if !ok {
		t.Fatal("expected fact for pkg.Fooer")
	}
	if iface.Props["symbol_kind"] != facts.SymbolInterface {
		t.Errorf("Fooer symbol_kind = %v, want interface", iface.Props["symbol_kind"])
	}
	if iface.Props["exported"] != true {
		t.Errorf("Fooer exported = %v, want true", iface.Props["exported"])
	}
}

func TestExtract_InterfaceMethodSymbols(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/iface.go": `package pkg

import "io"

type Renderer interface {
	io.Reader
	Instance(name string, data any) error
	reset()
}
`,
	})

	instance, ok := findFact(ff, "pkg.Renderer.Instance")
	if !ok {
		t.Fatal("expected a symbol fact for the interface method pkg.Renderer.Instance")
	}
	if instance.Props["symbol_kind"] != facts.SymbolMethod {
		t.Errorf("Instance symbol_kind = %v, want method", instance.Props["symbol_kind"])
	}
	if instance.Props["exported"] != true {
		t.Errorf("Instance exported = %v, want true", instance.Props["exported"])
	}
	if instance.Props["receiver"] != "Renderer" {
		t.Errorf("Instance receiver = %v, want Renderer", instance.Props["receiver"])
	}
	if !hasRelation(instance, facts.RelDeclares, "pkg") {
		t.Error("Instance should declare into its package")
	}

	unexported, ok := findFact(ff, "pkg.Renderer.reset")
	if !ok {
		t.Fatal("expected a symbol fact for the unexported interface method pkg.Renderer.reset")
	}
	if unexported.Props["exported"] != false {
		t.Errorf("reset exported = %v, want false", unexported.Props["exported"])
	}

	if _, ok := findFact(ff, "pkg.Renderer.Read"); ok {
		t.Error("an embedded interface's methods must not be expanded into symbol facts")
	}
}

func TestExtract_GenericType(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/generic.go": `package pkg

type Set[T comparable] struct {
	items map[T]struct{}
}
`,
	})

	// The generic type should be named without the type parameter
	set, ok := findFact(ff, "pkg.Set")
	if !ok {
		t.Fatal("expected fact for pkg.Set (without type params)")
	}
	if set.Props["symbol_kind"] != facts.SymbolStruct {
		t.Errorf("Set symbol_kind = %v, want struct", set.Props["symbol_kind"])
	}
}

func TestExtract_CallExtraction(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/calls.go": `package pkg

import "fmt"

func DoWork() {
	helper()
	fmt.Println("hello")
}

func helper() {}
`,
	})

	doWork, ok := findFact(ff, "pkg.DoWork")
	if !ok {
		t.Fatal("expected fact for pkg.DoWork")
	}

	// Bare call resolves to pkgDir.funcName.
	if !hasRelation(doWork, facts.RelCalls, "pkg.helper") {
		t.Error("DoWork should have calls relation for pkg.helper")
	}
	// stdlib alias "fmt" resolves to "fmt" (no module prefix to strip), so target is "fmt.Println".
	if !hasRelation(doWork, facts.RelCalls, "fmt.Println") {
		t.Error("DoWork should have calls relation for fmt.Println")
	}
}

func TestExtract_CallResolution_ImportQualified(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/server.go": `package server

import "testmod/internal/auth"

func Handle() {
	auth.CreateUser()
}
`,
		"internal/auth/auth.go": `package auth

func CreateUser() {}
`,
	})

	handle, ok := findFact(ff, "internal/server.Handle")
	if !ok {
		t.Fatal("expected fact for internal/server.Handle")
	}
	// "auth" alias maps to "internal/auth", so call resolves to "internal/auth.CreateUser".
	if !hasRelation(handle, facts.RelCalls, "internal/auth.CreateUser") {
		t.Errorf("Handle should call internal/auth.CreateUser; relations: %v", handle.Relations)
	}
}

func TestExtract_CallResolution_ExplicitAlias(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/server/server.go": `package server

import authlib "testmod/internal/auth"

func Handle() {
	authlib.NewClient()
}
`,
	})

	handle, ok := findFact(ff, "internal/server.Handle")
	if !ok {
		t.Fatal("expected fact for internal/server.Handle")
	}
	// Explicit alias "authlib" maps to "internal/auth".
	if !hasRelation(handle, facts.RelCalls, "internal/auth.NewClient") {
		t.Errorf("Handle should call internal/auth.NewClient; relations: %v", handle.Relations)
	}
}

func TestExtract_CallResolution_ReceiverMethod(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/handler.go": `package pkg

type Handler struct{}

func (h *Handler) DoSomething() {
	h.OtherMethod()
}

func (h *Handler) OtherMethod() {}
`,
	})

	do, ok := findFact(ff, "pkg.Handler.DoSomething")
	if !ok {
		t.Fatal("expected fact for pkg.Handler.DoSomething")
	}
	// h → *Handler receiver; h.OtherMethod() → pkg.Handler.OtherMethod
	if !hasRelation(do, facts.RelCalls, "pkg.Handler.OtherMethod") {
		t.Errorf("DoSomething should call pkg.Handler.OtherMethod; relations: %v", do.Relations)
	}
}

func TestExtract_CallResolution_FieldChain(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/server.go": `package pkg

type Server struct {
	auth AuthService
}

type AuthService struct{}

func (s *Server) Handle() {
	s.auth.Validate()
}

func (a *AuthService) Validate() {}
`,
	})

	handle, ok := findFact(ff, "pkg.Server.Handle")
	if !ok {
		t.Fatal("expected fact for pkg.Server.Handle")
	}
	// s → *Server; s.auth → field type AuthService (same pkg); s.auth.Validate() → pkg.AuthService.Validate
	if !hasRelation(handle, facts.RelCalls, "pkg.AuthService.Validate") {
		t.Errorf("Handle should call pkg.AuthService.Validate; relations: %v", handle.Relations)
	}
}

func TestExtract_CallResolution_Fallback(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/fallback.go": `package pkg

func DoStuff(a interface{}) {
	// a.b.c.d() — unresolvable chain
}
`,
	})

	// Just verify extraction completes without panic (no fact with an unresolvable call target).
	_, ok := findFact(ff, "pkg.DoStuff")
	if !ok {
		t.Fatal("expected fact for pkg.DoStuff")
	}
}

func TestExtract_CallResolution_SelfImport(t *testing.T) {
	// Subpackage imports the module root (exact-match self-import). The resolver
	// must map the import alias to "." so that field-chain resolution produces
	// root-package fact names like "..LocalService.DoWork".
	ff := extractAll(t, map[string]string{
		"auth.go": `package testmod

type AuthLibrary struct {
	Service LocalService
}

type LocalService struct{}
`,
		"adapters/handler.go": `package adapters

import auth "testmod"

type Handler struct {
	authLib *auth.AuthLibrary
}

func (h *Handler) Register() {
	h.authLib.Service.DoWork()
}

func (s *auth.LocalService) DoWork() {}
`,
	})

	register, ok := findFact(ff, "adapters.Handler.Register")
	if !ok {
		t.Fatal("expected fact for adapters.Handler.Register")
	}
	// h.authLib → "..AuthLibrary" (root pkg), .Service → "..LocalService", .DoWork → "..LocalService.DoWork"
	if !hasRelation(register, facts.RelCalls, "..LocalService.DoWork") {
		t.Errorf("Register should call ..LocalService.DoWork; relations: %v", register.Relations)
	}
}

func TestExtract_CallResolution_CrossPackageFieldChain(t *testing.T) {
	// Two packages in the same module. The server package has a struct with a
	// field typed from the auth package; handler method calls through that field.
	ff := extractAll(t, map[string]string{
		"pkg/auth/auth.go": `package auth

type Service struct{}

func (s *Service) ValidateToken() {}
`,
		"pkg/server/server.go": `package server

import "testmod/pkg/auth"

type Handler struct {
	auth auth.Service
}

func (h *Handler) Handle() {
	h.auth.ValidateToken()
}
`,
	})

	handle, ok := findFact(ff, "pkg/server.Handler.Handle")
	if !ok {
		t.Fatal("expected fact for pkg/server.Handler.Handle")
	}
	// h.auth → "pkg/auth.Service", .ValidateToken() → "pkg/auth.Service.ValidateToken"
	if !hasRelation(handle, facts.RelCalls, "pkg/auth.Service.ValidateToken") {
		t.Errorf("Handle should call pkg/auth.Service.ValidateToken; relations: %v", handle.Relations)
	}
}

func TestExtract_CallResolution_LocalConstructor(t *testing.T) {
	// A local variable assigned from a New<Type> constructor in another package;
	// a method call on it should resolve to the canonical method fact name.
	ff := extractAll(t, map[string]string{
		"internal/auth/auth.go": `package auth

type Service struct{}

func NewService() *Service { return &Service{} }

func (s *Service) Validate() {}
`,
		"internal/server/server.go": `package server

import "testmod/internal/auth"

func Handle() {
	svc := auth.NewService()
	svc.Validate()
}
`,
	})

	handle, ok := findFact(ff, "internal/server.Handle")
	if !ok {
		t.Fatal("expected fact for internal/server.Handle")
	}
	// svc := auth.NewService() infers type internal/auth.Service; svc.Validate()
	// resolves to internal/auth.Service.Validate.
	if !hasRelation(handle, facts.RelCalls, "internal/auth.Service.Validate") {
		t.Errorf("Handle should call internal/auth.Service.Validate; relations: %v", handle.Relations)
	}
}

func TestExtract_CallResolution_LocalCompositeLit(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/handler.go": `package pkg

type Service struct{}

func (s *Service) Do() {}

func Run() {
	svc := &Service{}
	svc.Do()
}
`,
	})

	run, ok := findFact(ff, "pkg.Run")
	if !ok {
		t.Fatal("expected fact for pkg.Run")
	}
	if !hasRelation(run, facts.RelCalls, "pkg.Service.Do") {
		t.Errorf("Run should call pkg.Service.Do; relations: %v", run.Relations)
	}
}

func TestExtract_CallResolution_LocalVarDecl(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/handler.go": `package pkg

type Service struct{}

func (s *Service) Do() {}

func Run() {
	var svc Service
	svc.Do()
}
`,
	})

	run, ok := findFact(ff, "pkg.Run")
	if !ok {
		t.Fatal("expected fact for pkg.Run")
	}
	if !hasRelation(run, facts.RelCalls, "pkg.Service.Do") {
		t.Errorf("Run should call pkg.Service.Do; relations: %v", run.Relations)
	}
}

func TestExtract_CallResolution_SkipsBuiltins(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/calls.go": `package pkg

func DoWork() {
	_ = len(items())
	_ = make([]int, 0)
	_ = string(raw())
	helper()
}

func items() []int { return nil }
func raw() []byte  { return nil }
func helper()      {}
`,
	})

	doWork, ok := findFact(ff, "pkg.DoWork")
	if !ok {
		t.Fatal("expected fact for pkg.DoWork")
	}
	// Real same-package calls are still emitted.
	if !hasRelation(doWork, facts.RelCalls, "pkg.helper") {
		t.Error("DoWork should call pkg.helper")
	}
	if !hasRelation(doWork, facts.RelCalls, "pkg.items") {
		t.Error("DoWork should call pkg.items")
	}
	// Builtins and conversions must NOT produce dangling edges.
	for _, name := range []string{"pkg.len", "pkg.make", "pkg.string"} {
		if hasRelation(doWork, facts.RelCalls, name) {
			t.Errorf("DoWork should not have a calls edge for builtin %q", name)
		}
	}
}

func TestExtract_Imports(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/imports.go": `package pkg

import (
	"fmt"
	"io"
)

func Foo() {
	fmt.Println("hello")
	_ = io.EOF
}
`,
	})

	deps := findFactsByKind(ff, facts.KindDependency)
	// Should have imports for "fmt" and "io"
	hasFmt := false
	hasIO := false
	for _, d := range deps {
		for _, r := range d.Relations {
			if r.Kind == facts.RelImports && r.Target == "fmt" {
				hasFmt = true
			}
			if r.Kind == facts.RelImports && r.Target == "io" {
				hasIO = true
			}
		}
	}
	if !hasFmt {
		t.Error("expected dependency fact with imports→fmt")
	}
	if !hasIO {
		t.Error("expected dependency fact with imports→io")
	}
}

func TestExtract_PackageGrouping(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/a/a.go": `package a

func FuncA() {}
`,
		"pkg/b/b.go": `package b

func FuncB() {}
`,
	})

	modules := findFactsByKind(ff, facts.KindModule)
	if len(modules) != 2 {
		t.Fatalf("expected 2 module facts, got %d", len(modules))
	}

	modNames := map[string]bool{}
	for _, m := range modules {
		modNames[m.Name] = true
	}
	if !modNames["pkg/a"] {
		t.Error("expected module fact for pkg/a")
	}
	if !modNames["pkg/b"] {
		t.Error("expected module fact for pkg/b")
	}
}

func TestDetect(t *testing.T) {
	ext := New()

	// With go.mod: should detect
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	detected, err := ext.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !detected {
		t.Error("expected Detect=true for directory with go.mod")
	}

	// Without go.mod: should not detect
	dir2 := t.TempDir()
	detected2, err := ext.Detect(dir2)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if detected2 {
		t.Error("expected Detect=false for directory without go.mod")
	}
}

// TestExtract_StructAndMethodAreSeparateFacts documents the contract the graph's
// has_method synthesis relies on: a struct and each of its methods are emitted as
// distinct sibling facts ("pkg.Type" and "pkg.Type.Method"), with no edge between
// them at extraction time. If this ever changes, internal/facts.NewGraph's
// has_method third pass must be revisited.
func TestExtract_StructAndMethodAreSeparateFacts(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/handler.go": `package pkg

type AuthHandler struct{}

func (h *AuthHandler) Login() {}
`,
	})

	st, ok := findFact(ff, "pkg.AuthHandler")
	if !ok {
		t.Fatal("expected struct fact pkg.AuthHandler")
	}
	if st.Props["symbol_kind"] != facts.SymbolStruct {
		t.Errorf("AuthHandler symbol_kind = %v, want struct", st.Props["symbol_kind"])
	}
	// The struct must NOT carry an edge to its method (the graph synthesizes it).
	for _, rel := range st.Relations {
		if rel.Target == "pkg.AuthHandler.Login" {
			t.Errorf("struct should not declare its method directly, found relation %+v", rel)
		}
	}

	m, ok := findFact(ff, "pkg.AuthHandler.Login")
	if !ok {
		t.Fatal("expected method fact pkg.AuthHandler.Login as a separate fact")
	}
	if m.Props["symbol_kind"] != facts.SymbolMethod {
		t.Errorf("Login symbol_kind = %v, want method", m.Props["symbol_kind"])
	}
}

// TestExtract_HTTPHandlerProp is the v111 prop, and the enabling half of new/18.
//
// A route's `handler` prop is built by exprToString from the REGISTRATION SITE, so it
// is the receiver VARIABLE chain ("h.aiCoachHandler.GetInsight"), while the symbol is
// named by its receiver TYPE ("internal/adapters/http/aicoach.HandlerV2.GetInsight").
// The two key spaces are disjoint: on fairwayhub/golf, 1397 distinct handler props
// intersect 13482 symbol names in exactly TWO places, and both are Python.
//
// Binding them by name alone mis-binds: the naive rule bound /api/weather/daily-range
// to internal/bootstrap.NullWeatherService.GetDailyWeatherRange — a stub in the WIRING
// package — because it shares the method name with the real handler. A wrong handled_by
// edge feeds impact_analysis and find_path and is worse than no edge at all.
//
// An HTTP handler is exactly func(http.ResponseWriter, *http.Request), which is
// decidable from the AST. That is a STRUCTURAL constraint, not a name heuristic, and it
// is what lets the binder reject NullWeatherService (signature (ctx, string)) by
// construction rather than by guessing.
func TestExtract_HTTPHandlerProp(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"api/handler.go": `package api

import (
	"context"
	"net/http"
)

type HandlerV2 struct{}

// A real handler — method form, and the param names are deliberately NOT w/r.
func (h *HandlerV2) GetThing(rw http.ResponseWriter, req *http.Request) {}

// A real handler — plain func form, unnamed params.
func Health(http.ResponseWriter, *http.Request) {}

// NOT a handler: the wiring-package stub that the naive binder wrongly bound to.
// It shares a method name with a handler but has a service signature.
type NullService struct{}

func (s *NullService) GetThing(ctx context.Context, date string) error { return nil }

// NOT a handler: takes the request by value, not by pointer.
func ByValue(rw http.ResponseWriter, req http.Request) {}

// NOT a handler: middleware returns a handler, it is not one.
func Middleware(next http.Handler) http.Handler { return next }
`,
	})

	isHandler := func(name string) bool {
		f, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("symbol %q not extracted", name)
		}
		v, _ := f.Props["http_handler"].(bool)
		return v
	}

	for _, name := range []string{"api.HandlerV2.GetThing", "api.Health"} {
		if !isHandler(name) {
			t.Errorf("%s has signature func(http.ResponseWriter, *http.Request) but http_handler is not set", name)
		}
	}
	// The whole point: these must NOT be tagged, or the binder cannot tell them apart.
	for _, name := range []string{"api.NullService.GetThing", "api.ByValue", "api.Middleware"} {
		if isHandler(name) {
			t.Errorf("%s is not an HTTP handler but was tagged http_handler — this is the mis-bind", name)
		}
	}

	// The prop must be absent, not false, on non-handlers: it is a positive marker and
	// the golden would otherwise gain a line for every Go function in every repo.
	f, _ := findFact(ff, "api.Middleware")
	if _, present := f.Props["http_handler"]; present {
		t.Error("http_handler must be omitted on non-handlers, not set to false")
	}
}

// A struct used only as a composite literal must get a RelInstantiates edge so it
// is not mis-reported as dead code (type usage is otherwise not edge-tracked).
func TestExtract_StructUsage_CompositeLiteral(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/app/app.go": `package app

import "bytes"

type Config struct{ Name string }

func build() Config {
	_ = bytes.Buffer{}   // external type: must NOT get an edge
	return Config{Name: "x"}
}
`,
	})

	sym, ok := findFact(ff, "internal/app.build")
	if !ok {
		t.Fatal("missing symbol internal/app.build")
	}
	if !hasRelation(sym, facts.RelInstantiates, "internal/app.Config") {
		t.Errorf("want RelInstantiates internal/app.Config on build; relations = %v", sym.Relations)
	}
	// The external stdlib type must not be emitted as an instantiates target.
	for _, r := range sym.Relations {
		if r.Kind == facts.RelInstantiates && r.Target == "bytes.Buffer" {
			t.Errorf("must not emit instantiates edge to external type bytes.Buffer")
		}
	}
}

// Another module's type, used as a composite literal or a field type through an import
// alias, gets an edge under its import path: a snapshot that loads that module as
// another repository resolves it to the declaration there. A stdlib type still gets none.
func TestExtract_StructUsage_AnotherModulesType(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/auth/setup.go": `package auth

import (
	"bytes"

	goauth "github.com/acme/go-auth"
)

type Holder struct {
	deps goauth.Dependencies
}

func Setup() {
	_ = goauth.LibraryConfig{Name: "x"}
	_ = bytes.Buffer{}
}
`,
	})
	setup, ok := findFact(ff, "pkg/auth.Setup")
	if !ok {
		t.Fatal("missing symbol pkg/auth.Setup")
	}
	if !hasRelation(setup, facts.RelInstantiates, "github.com/acme/go-auth.LibraryConfig") {
		t.Errorf("want RelInstantiates github.com/acme/go-auth.LibraryConfig on Setup; relations = %v", setup.Relations)
	}
	for _, r := range setup.Relations {
		if r.Kind == facts.RelInstantiates && strings.HasSuffix(r.Target, "Buffer") {
			t.Errorf("must not emit an instantiates edge to the stdlib type %s", r.Target)
		}
	}
	holder, ok := findFact(ff, "pkg/auth.Holder")
	if !ok {
		t.Fatal("missing symbol pkg/auth.Holder")
	}
	if !hasRelation(holder, facts.RelInstantiates, "github.com/acme/go-auth.Dependencies") {
		t.Errorf("want RelInstantiates github.com/acme/go-auth.Dependencies on Holder; relations = %v", holder.Relations)
	}
}

// The module's own packages and the standard library are both written as plain paths
// ("config", "net/http"). The module's package list tells them apart: a type from the
// standard library gets no edge, however its path looks, and one from a package at the
// repository root does.
func TestExtract_StructUsage_StdlibAndRootPackages(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"config/config.go": `package config

type Settings struct{ Name string }
`,
		"internal/app/app.go": `package app

import (
	"encoding/json"
	"net/http"

	"testmod/config"
)

type Server struct {
	dec  json.Decoder
	conf config.Settings
}

func run() {
	_ = http.Server{}
	_ = config.Settings{}
}
`,
	})
	run, ok := findFact(ff, "internal/app.run")
	if !ok {
		t.Fatal("missing symbol internal/app.run")
	}
	if !hasRelation(run, facts.RelInstantiates, "config.Settings") {
		t.Errorf("want RelInstantiates config.Settings on run; relations = %v", run.Relations)
	}
	server, ok := findFact(ff, "internal/app.Server")
	if !ok {
		t.Fatal("missing symbol internal/app.Server")
	}
	for _, sym := range []facts.Fact{run, server} {
		for _, r := range sym.Relations {
			if r.Kind == facts.RelInstantiates && (r.Target == "net/http.Server" || r.Target == "encoding/json.Decoder") {
				t.Errorf("%s: must not emit an instantiates edge to the stdlib type %s", sym.Name, r.Target)
			}
		}
	}
}

// A pure-data struct referenced only as another struct's field type must get a
// RelInstantiates edge from the owning struct.
func TestExtract_StructUsage_FieldType(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/app/app.go": `package app

type Inner struct{ V int }

type Outer struct {
	inner Inner
}
`,
	})

	sym, ok := findFact(ff, "internal/app.Outer")
	if !ok {
		t.Fatal("missing symbol internal/app.Outer")
	}
	if !hasRelation(sym, facts.RelInstantiates, "internal/app.Inner") {
		t.Errorf("want RelInstantiates internal/app.Inner on Outer; relations = %v", sym.Relations)
	}
}

// scalingLoopDepthOf returns the scaling_loop_depth prop of a symbol, or -1.
func scalingLoopDepthOf(ff []facts.Fact, name string) int {
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Name == name {
			if v, ok := f.Props["scaling_loop_depth"].(int); ok {
				return v
			}
			return 0 // present symbol, no scaling loops emitted
		}
	}
	return -1
}

// A hierarchical nest (each inner collection reached through the outer loop var)
// visits each element once → scaling_loop_depth 1, not 3.
func TestExtract_ScalingDepth_HierarchicalIsLinear(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/app/walk.go": `package app

type File struct{ Decls []int }
type Pkg struct{ Files []File }

func walk(pkgs []Pkg) int {
	total := 0
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				total += d
			}
		}
	}
	return total
}
`,
	})

	if got := scalingLoopDepthOf(ff, "internal/app.walk"); got != 1 {
		t.Errorf("hierarchical walk scaling_loop_depth = %d, want 1", got)
	}
}

// An all-pairs nest over the same collection is genuinely quadratic → depth 2.
func TestExtract_ScalingDepth_AllPairsIsQuadratic(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/app/pairs.go": `package app

func pairs(xs []int) int {
	n := 0
	for i := range xs {
		for j := range xs {
			n += i + j
		}
	}
	return n
}
`,
	})

	if got := scalingLoopDepthOf(ff, "internal/app.pairs"); got != 2 {
		t.Errorf("all-pairs scaling_loop_depth = %d, want 2", got)
	}
}

// Indexing through the outer loop var (pkgs[i].Files) is also hierarchical.
func TestExtract_ScalingDepth_IndexThroughOuterVarIsLinear(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/app/idx.go": `package app

type P struct{ Items []int }

func walkIdx(ps []P) int {
	total := 0
	for i := range ps {
		for _, item := range ps[i].Items {
			total += item
		}
	}
	return total
}
`,
	})

	if got := scalingLoopDepthOf(ff, "internal/app.walkIdx"); got != 1 {
		t.Errorf("index-through-outer-var scaling_loop_depth = %d, want 1", got)
	}
}

// callsOf returns the call targets recorded on one symbol.
func callsOf(t *testing.T, files map[string]string, symbol string) []string {
	t.Helper()
	for _, f := range extractAll(t, files) {
		if f.Kind != facts.KindSymbol || f.Name != symbol {
			continue
		}
		var out []string
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls {
				out = append(out, r.Target)
			}
		}
		return out
	}
	t.Fatalf("no symbol %q extracted", symbol)
	return nil
}

func hasCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

// A generic call's callee is an IndexExpr, not a selector, so every call written
// `api.Request[Task](…)` was dropped without a signal. A Go CLI in this estate
// routes its whole API surface through two such helpers and emitted no call edge
// to either — while `fmt.Sprintf` two lines below in the same closure came
// through.
func TestGoCalls_GenericInstantiationIsACall(t *testing.T) {
	calls := callsOf(t, map[string]string{
		"internal/api/api.go": "package api\n\n" +
			"func Request[T any](path string) (T, error) { var z T; return z, nil }\n" +
			"func Fetch[K comparable, V any](path string) (V, error) { var z V; return z, nil }\n",
		"internal/cli/tasks.go": "package cli\n\n" +
			"import (\n\t\"fmt\"\n\n\t\"testmod/internal/api\"\n)\n\n" +
			"func TasksCmd() string {\n" +
			"\tenv, _ := api.Request[int](\"/me/tasks\")\n" +
			"\tpair, _ := api.Fetch[string, int](\"/me/pairs\")\n" +
			"\treturn fmt.Sprintf(\"%v %v\", env, pair)\n}\n",
	}, "internal/cli.TasksCmd")

	for _, want := range []string{"internal/api.Request", "internal/api.Fetch", "fmt.Sprintf"} {
		if !hasCall(calls, want) {
			t.Errorf("want %q among calls, got %v", want, calls)
		}
	}
}

// The fail-closed half. `a[i](…)` is a call out of a map or slice of funcs and is
// syntactically identical to a single-argument instantiation; resolveChain turns a
// bare identifier into <pkg>.<name>, so unwrapping it would draw a confident edge
// to a real package-level variable rather than dangle.
func TestGoCalls_UnqualifiedIndexCallIsNotAttributed(t *testing.T) {
	calls := callsOf(t, map[string]string{
		"internal/cli/dispatch.go": "package cli\n\n" +
			"var Handlers = map[string]func() int{}\n\n" +
			"func Dispatch(name string) int {\n\treturn Handlers[name]()\n}\n",
	}, "internal/cli.Dispatch")

	if hasCall(calls, "internal/cli.Handlers") {
		t.Errorf("a map-of-funcs call must not become an edge to the map; got %v", calls)
	}
}

func TestExtract_FactSequenceIsIdenticalAcrossRuns(t *testing.T) {
	files := map[string]string{}
	for _, pkg := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		files["internal/"+pkg+"/"+pkg+".go"] = "package " + pkg + "\n\n" +
			"func Do() int {\n\tif true {\n\t\treturn 1\n\t}\n\treturn 0\n}\n"
	}
	dir := setupGoProject(t, files)
	relFiles := make([]string, 0, len(files))
	for f := range files {
		relFiles = append(relFiles, f)
	}
	sort.Strings(relFiles)

	first, err := New().Extract(context.Background(), dir, relFiles)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for i := 0; i < 5; i++ {
		got, err := New().Extract(context.Background(), dir, relFiles)
		if err != nil {
			t.Fatalf("run %d: Extract: %v", i, err)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d: fact sequence differs from first run", i)
		}
	}
}
