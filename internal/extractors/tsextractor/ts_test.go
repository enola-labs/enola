package tsextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// --- helpers ---

func setupTSProject(t *testing.T, files map[string]string, nextjs bool) string {
	t.Helper()
	dir := t.TempDir()

	// Create tsconfig.json for detection
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Optionally create next.config.js for Next.js detection
	if nextjs {
		if err := os.WriteFile(filepath.Join(dir, "next.config.js"), []byte(`module.exports = {}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

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

func extractAll(t *testing.T, files map[string]string, nextjs bool) []facts.Fact {
	t.Helper()
	dir := setupTSProject(t, files, nextjs)

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

// --- Route detection tests ---

func TestDetectRoute_AppRouter(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		wantRoute  string
		wantMethod string
		wantType   string
	}{
		{"root page", "src/app/page.tsx", "/", "GET", "page"},
		{"nested page", "src/app/about/page.tsx", "/about", "GET", "page"},
		{"dynamic segment", "src/app/users/[id]/page.tsx", "/users/[id]", "GET", "page"},
		{"api route", "src/app/api/users/route.tsx", "/api/users", "ALL", "route"},
		{"layout", "src/app/layout.tsx", "/", "GET", "layout"},
		{"loading", "src/app/loading.tsx", "/", "GET", "loading"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectRoute(tt.file)
			if got == nil {
				t.Fatal("expected route fact, got nil")
			}
			if got.Name != tt.wantRoute {
				t.Errorf("route path = %q, want %q", got.Name, tt.wantRoute)
			}
			if got.Props["method"] != tt.wantMethod {
				t.Errorf("method = %v, want %s", got.Props["method"], tt.wantMethod)
			}
			if got.Props["type"] != tt.wantType {
				t.Errorf("type = %v, want %s", got.Props["type"], tt.wantType)
			}
			if got.Props["router"] != "app" {
				t.Errorf("router = %v, want app", got.Props["router"])
			}
		})
	}
}

func TestDetectRoute_PagesRouter(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		wantRoute  string
		wantMethod string
	}{
		{"index page", "pages/index.tsx", "/", "GET"},
		{"about page", "pages/about.tsx", "/about", "GET"},
		{"dynamic", "pages/users/[id].tsx", "/users/[id]", "GET"},
		{"api route", "pages/api/hello.ts", "/api/hello", "ALL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectRoute(tt.file)
			if got == nil {
				t.Fatal("expected route fact, got nil")
			}
			if got.Name != tt.wantRoute {
				t.Errorf("route path = %q, want %q", got.Name, tt.wantRoute)
			}
			if got.Props["method"] != tt.wantMethod {
				t.Errorf("method = %v, want %s", got.Props["method"], tt.wantMethod)
			}
			if got.Props["router"] != "pages" {
				t.Errorf("router = %v, want pages", got.Props["router"])
			}
		})
	}
}

func TestDetectRoute_PagesRouter_SkipsSpecialFiles(t *testing.T) {
	for _, file := range []string{"pages/_app.tsx", "pages/_document.tsx", "pages/_error.tsx"} {
		got := detectRoute(file)
		if got != nil {
			t.Errorf("detectRoute(%q) should return nil for special pages, got %+v", file, got)
		}
	}
}

func TestDetectRoute_NonRoute(t *testing.T) {
	got := detectRoute("src/components/Button.tsx")
	if got != nil {
		t.Error("non-route file should return nil")
	}
}

func TestHasTSMarkers_PlainJSFramework(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"dependencies":{"vue":"^2.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasTSMarkers(dir) {
		t.Error("expected a plain-JS Vue project (no tsconfig, no typescript dep) to be detected")
	}
}

func TestHasTSMarkers_PlainJSNoFramework(t *testing.T) {
	// This used to assert the opposite: a plain-JS Express app with no
	// TS-ecosystem dependency stayed UNDETECTED — which meant the very servers
	// the Express route extraction exists for were invisible unless they also
	// happened to use TypeScript. Any package.json that structurally declares a
	// package (dependencies, bin, main, exports, type, workspaces) now counts;
	// the boundary is pinned by TestDetect_PlainNodePackageWithoutDependencies,
	// whose bare name-only stub still stays out.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"dependencies":{"express":"^4.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasTSMarkers(dir) {
		t.Error("expected a plain-JS Express project to be detected — its routes are the point")
	}
}

// --- Full extraction tests ---

func TestExtract_FunctionDeclaration(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/utils.ts": `export function fetchUsers() { return [] }`,
	}, false)

	f, ok := findFact(ff, "src.fetchUsers")
	if !ok {
		t.Fatal("expected fact for src.fetchUsers")
	}
	if f.Props["symbol_kind"] != facts.SymbolFunc {
		t.Errorf("symbol_kind = %v, want function", f.Props["symbol_kind"])
	}
	if f.Props["exported"] != true {
		t.Errorf("exported = %v, want true", f.Props["exported"])
	}
}

func TestExtract_ArrowFunction(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/handler.ts": `export const handler = () => { return "ok" }`,
	}, false)

	f, ok := findFact(ff, "src.handler")
	if !ok {
		t.Fatal("expected fact for src.handler")
	}
	// Arrow functions should be classified as function, not variable
	if f.Props["symbol_kind"] != facts.SymbolFunc {
		t.Errorf("symbol_kind = %v, want function (arrow function should be detected)", f.Props["symbol_kind"])
	}
}

func TestExtract_ClassWithImplements(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/service.ts": `export class UserService implements Service, Loggable {}`,
	}, false)

	f, ok := findFact(ff, "src.UserService")
	if !ok {
		t.Fatal("expected fact for src.UserService")
	}
	if f.Props["symbol_kind"] != facts.SymbolClass {
		t.Errorf("symbol_kind = %v, want class", f.Props["symbol_kind"])
	}

	if !hasRelation(f, facts.RelImplements, "Service") {
		t.Error("expected implements relation for Service")
	}
	if !hasRelation(f, facts.RelImplements, "Loggable") {
		t.Error("expected implements relation for Loggable")
	}
}

func TestExtract_AbstractClass(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/service.ts": `
export abstract class BaseService { abstract handle(): void }
export class ConcreteService { handle() {} }
`,
	}, false)

	abstractCls, ok := findFact(ff, "src.BaseService")
	if !ok {
		t.Fatal("expected fact for src.BaseService")
	}
	if abstractCls.Props["symbol_kind"] != facts.SymbolClass {
		t.Errorf("BaseService symbol_kind = %v, want class", abstractCls.Props["symbol_kind"])
	}
	if ab, _ := abstractCls.Props["abstract"].(bool); !ab {
		t.Errorf("BaseService abstract = %v, want true", abstractCls.Props["abstract"])
	}

	concreteCls, ok := findFact(ff, "src.ConcreteService")
	if !ok {
		t.Fatal("expected fact for src.ConcreteService")
	}
	if ab, ok := concreteCls.Props["abstract"].(bool); ok && ab {
		t.Errorf("ConcreteService abstract = %v, want unset/false", concreteCls.Props["abstract"])
	}
}

func TestExtract_InterfaceAndTypeAlias(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/types.ts": `
export interface User { name: string }
export type UserId = string
`,
	}, false)

	iface, ok := findFact(ff, "src.User")
	if !ok {
		t.Fatal("expected fact for src.User")
	}
	if iface.Props["symbol_kind"] != facts.SymbolInterface {
		t.Errorf("User symbol_kind = %v, want interface", iface.Props["symbol_kind"])
	}

	typAlias, ok := findFact(ff, "src.UserId")
	if !ok {
		t.Fatal("expected fact for src.UserId")
	}
	if typAlias.Props["symbol_kind"] != facts.SymbolType {
		t.Errorf("UserId symbol_kind = %v, want type", typAlias.Props["symbol_kind"])
	}
}

func TestExtract_ImportExtraction(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/app.ts": `
import { foo } from './utils'
import React from 'react'
`,
	}, false)

	deps := findFactsByKind(ff, facts.KindDependency)
	hasUtils := false
	hasReact := false
	for _, d := range deps {
		for _, r := range d.Relations {
			if r.Target == "src/utils" {
				hasUtils = true
				if d.Props["reexport"] == true {
					t.Error("plain import should not be tagged reexport")
				}
			}
			if r.Target == "react" {
				hasReact = true
			}
		}
	}
	if !hasUtils {
		t.Error("expected import for src/utils")
	}
	if !hasReact {
		t.Error("expected import for react")
	}
}

func TestExtract_Monorepo_NestedTSConfigAlias(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"tsconfig.json":                 `{}`,
		"app/ui/tsconfig.json":          `{"compilerOptions":{"paths":{"~/*":["./src/*"]}}}`,
		"app/ui/src/pages/Home.tsx":     `import { Foo } from '~/components/Foo'`,
		"app/ui/src/components/Foo.tsx": `export function Foo() { return null }`,
	}, false)

	deps := findFactsByKind(ff, facts.KindDependency)
	var found *facts.Fact
	for i := range deps {
		if hasRelation(deps[i], facts.RelImports, "app/ui/src/components/Foo") {
			found = &deps[i]
		}
	}
	if found == nil {
		t.Fatal("expected ~/components/Foo to resolve to app/ui/src/components/Foo")
	}
	if found.Props["source"] != "internal" {
		t.Errorf("source = %v, want internal (paths-less root tsconfig should not short-circuit nested package alias discovery)", found.Props["source"])
	}
}

func TestExtract_Monorepo_SiblingPackagesSameAliasDifferentTarget(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"tsconfig.json":                `{}`,
		"packages/app-a/tsconfig.json": `{"compilerOptions":{"paths":{"~/*":["./src/*"]}}}`,
		"packages/app-a/src/index.ts":  `import { X } from '~/foo'`,
		"packages/app-a/src/foo.ts":    `export const X = 1`,
		"packages/app-b/tsconfig.json": `{"compilerOptions":{"paths":{"~/*":["./lib/*"]}}}`,
		"packages/app-b/index.ts":      `import { Y } from '~/foo'`,
		"packages/app-b/lib/foo.ts":    `export const Y = 2`,
	}, false)

	deps := findFactsByKind(ff, facts.KindDependency)
	wantA, wantB := false, false
	for _, d := range deps {
		if hasRelation(d, facts.RelImports, "packages/app-a/src/foo") {
			wantA = true
		}
		if hasRelation(d, facts.RelImports, "packages/app-b/lib/foo") {
			wantB = true
		}
		// Neither package's ~/foo should ever resolve against the other's mapping.
		if hasRelation(d, facts.RelImports, "packages/app-b/src/foo") ||
			hasRelation(d, facts.RelImports, "packages/app-a/lib/foo") {
			t.Errorf("alias resolved against the wrong package's tsconfig: %+v", d)
		}
	}
	if !wantA {
		t.Error("expected app-a's ~/foo to resolve to packages/app-a/src/foo")
	}
	if !wantB {
		t.Error("expected app-b's ~/foo to resolve to packages/app-b/lib/foo")
	}
}

func TestExtract_BarrelReexports(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/index.ts": `
export * from './client'
export { HomePage } from './HomePage'
export type { Config } from './types'
export * from 'some-external-lib'
`,
		"src/client.ts":    `export function makeClient() {}`,
		"src/HomePage.tsx": `export function HomePage() { return null }`,
		"src/types.ts":     `export type Config = { url: string }`,
	}, false)

	deps := findFactsByKind(ff, facts.KindDependency)

	cases := []struct {
		target     string
		wantSource string
	}{
		{"src/client", "internal"},
		{"src/HomePage", "internal"},
		{"src/types", "internal"},
		{"some-external-lib", "external"},
	}
	for _, tc := range cases {
		var found *facts.Fact
		for i := range deps {
			if hasRelation(deps[i], facts.RelImports, tc.target) {
				found = &deps[i]
			}
		}
		if found == nil {
			t.Fatalf("expected a Dependency fact re-exporting %s", tc.target)
		}
		if found.Props["source"] != tc.wantSource {
			t.Errorf("%s: source = %v, want %s", tc.target, found.Props["source"], tc.wantSource)
		}
		if found.Props["reexport"] != true {
			t.Errorf("%s: reexport = %v, want true", tc.target, found.Props["reexport"])
		}
	}
}

func TestExtract_NonExportedDeclaration(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/internal.ts": `function helper() { return 42 }`,
	}, false)

	f, ok := findFact(ff, "src.helper")
	if !ok {
		t.Fatal("expected fact for src.helper")
	}
	if f.Props["exported"] != false {
		t.Errorf("exported = %v, want false (no export keyword)", f.Props["exported"])
	}
}

func TestExtract_ClassMethods(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/service.ts": `export class ApiClient {
  private baseUrl: string = '/api'

  login(username: string, password: string) {
    return fetch(this.baseUrl + '/login')
  }

  logout() {
    return fetch(this.baseUrl + '/logout')
  }

  private refreshToken() {
    return fetch(this.baseUrl + '/refresh')
  }
}`,
	}, false)

	// Class itself should be extracted
	cls, ok := findFact(ff, "src.ApiClient")
	if !ok {
		t.Fatal("expected fact for src.ApiClient")
	}
	if cls.Props["symbol_kind"] != facts.SymbolClass {
		t.Errorf("class symbol_kind = %v, want class", cls.Props["symbol_kind"])
	}

	// Public methods should be extracted
	login, ok := findFact(ff, "src.ApiClient.login")
	if !ok {
		t.Fatal("expected fact for src.ApiClient.login")
	}
	if login.Props["symbol_kind"] != facts.SymbolMethod {
		t.Errorf("login symbol_kind = %v, want method", login.Props["symbol_kind"])
	}
	if login.Props["exported"] != true {
		t.Errorf("login exported = %v, want true", login.Props["exported"])
	}
	if login.Props["receiver"] != "ApiClient" {
		t.Errorf("login receiver = %v, want ApiClient", login.Props["receiver"])
	}

	// logout should be extracted
	_, ok = findFact(ff, "src.ApiClient.logout")
	if !ok {
		t.Fatal("expected fact for src.ApiClient.logout")
	}

	// Private method should be extracted but marked as not exported
	refresh, ok := findFact(ff, "src.ApiClient.refreshToken")
	if !ok {
		t.Fatal("expected fact for src.ApiClient.refreshToken")
	}
	if refresh.Props["exported"] != false {
		t.Errorf("refreshToken exported = %v, want false (private)", refresh.Props["exported"])
	}

	// Constructor should NOT be extracted
	_, ok = findFact(ff, "src.ApiClient.constructor")
	if ok {
		t.Error("constructor should not be extracted as a method")
	}
}

func TestExtract_CallExtraction_SameModule(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main.ts": `export function doWork() {
  helper()
}

function helper() {}`,
	}, false)

	doWork, ok := findFact(ff, "src.doWork")
	if !ok {
		t.Fatal("expected fact for src.doWork")
	}
	if !hasRelation(doWork, facts.RelCalls, "src.helper") {
		t.Errorf("doWork should call src.helper; relations: %v", doWork.Relations)
	}
}

func TestExtract_CallExtraction_ThisMethod(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/service.ts": `export class ApiClient {
  login() {
    this.refresh()
  }

  refresh() {}
}`,
	}, false)

	login, ok := findFact(ff, "src.ApiClient.login")
	if !ok {
		t.Fatal("expected fact for src.ApiClient.login")
	}
	if !hasRelation(login, facts.RelCalls, "src.ApiClient.refresh") {
		t.Errorf("login should call src.ApiClient.refresh; relations: %v", login.Relations)
	}
}

func TestExtract_CallExtraction_ImportedFunction(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main.ts": `import { formatName } from './utils'

export function render() {
  formatName()
}`,
		"src/utils.ts": `export function formatName() {}`,
	}, false)

	render, ok := findFact(ff, "src.render")
	if !ok {
		t.Fatal("expected fact for src.render")
	}
	// formatName imported from "./utils" → resolves to src.formatName, which is
	// the canonical fact name of the declaration in src/utils.ts.
	if !hasRelation(render, facts.RelCalls, "src.formatName") {
		t.Errorf("render should call src.formatName; relations: %v", render.Relations)
	}
	// Confirm the callee fact actually exists, so the edge is not dangling.
	if _, ok := findFact(ff, "src.formatName"); !ok {
		t.Error("expected callee fact src.formatName to exist")
	}
}

func TestExtract_CallExtraction_ArrowFunction(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main.ts": `const handler = () => {
  process()
}

function process() {}`,
	}, false)

	handler, ok := findFact(ff, "src.handler")
	if !ok {
		t.Fatal("expected fact for src.handler")
	}
	if !hasRelation(handler, facts.RelCalls, "src.process") {
		t.Errorf("handler should call src.process; relations: %v", handler.Relations)
	}
}

func TestExtract_CallExtraction_MethodOnReceiver_NoEdge(t *testing.T) {
	// A method call on a value of unknown type is left unresolved.
	ff := extractAll(t, map[string]string{
		"src/main.ts": `export function run(client: ApiClient) {
  client.login()
}`,
	}, false)

	run, ok := findFact(ff, "src.run")
	if !ok {
		t.Fatal("expected fact for src.run")
	}
	for _, r := range run.Relations {
		if r.Kind == facts.RelCalls {
			t.Errorf("unexpected RelCalls edge for receiver method call: %v", r)
		}
	}
}

// --- React / Next.js semantic classification & coverage tests ---

func TestExtract_DefaultExportFunctionComponent(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/components/Button.tsx": `export default function Button() { return <button/> }`,
	}, true)

	f, ok := findFact(ff, "src/components.Button")
	if !ok {
		t.Fatal("expected fact for src/components.Button")
	}
	if f.Props["symbol_kind"] != facts.SymbolFunc {
		t.Errorf("symbol_kind = %v, want function", f.Props["symbol_kind"])
	}
	if f.Props["exported"] != true {
		t.Errorf("exported = %v, want true", f.Props["exported"])
	}
	if f.Props["web_component"] != "component" {
		t.Errorf("web_component = %v, want component", f.Props["web_component"])
	}
	if f.Props["framework"] != "nextjs" {
		t.Errorf("framework = %v, want nextjs", f.Props["framework"])
	}
}

func TestExtract_AnonymousDefaultExport_NamedByFile(t *testing.T) {
	// Anonymous default exports are named after the file (parent dir for generic
	// Next.js page filenames).
	ff := extractAll(t, map[string]string{
		"src/app/dashboard/page.tsx": `export default function() { return <div/> }`,
		"src/components/Card.tsx":    `export default () => <div/>`,
	}, true)

	page, ok := findFact(ff, "src/app/dashboard.DashboardPage")
	if !ok {
		t.Fatalf("expected fact src/app/dashboard.DashboardPage; got %v", factNames(ff))
	}
	if page.Props["exported"] != true {
		t.Errorf("page exported = %v, want true", page.Props["exported"])
	}
	if page.Props["web_component"] != "component" {
		t.Errorf("page web_component = %v, want component", page.Props["web_component"])
	}

	card, ok := findFact(ff, "src/components.Card")
	if !ok {
		t.Fatalf("expected fact src/components.Card (anon default arrow); got %v", factNames(ff))
	}
	if card.Props["web_component"] != "component" {
		t.Errorf("card web_component = %v, want component", card.Props["web_component"])
	}
}

func TestExtract_MemoWrappedComponent(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/components/Card.tsx": `import { memo } from 'react'
const Card = memo(function Card() { return <div/> })
export default Card`,
	}, true)

	f, ok := findFact(ff, "src/components.Card")
	if !ok {
		t.Fatalf("expected fact src/components.Card; got %v", factNames(ff))
	}
	// memo-wrapped value should be a function/component, not a plain variable.
	if f.Props["symbol_kind"] != facts.SymbolFunc {
		t.Errorf("symbol_kind = %v, want function (memo-wrapped)", f.Props["symbol_kind"])
	}
	if f.Props["web_component"] != "component" {
		t.Errorf("web_component = %v, want component", f.Props["web_component"])
	}
	// Exported via `export default Card`.
	if f.Props["exported"] != true {
		t.Errorf("exported = %v, want true (export default Card)", f.Props["exported"])
	}
}

func TestExtract_ReExportMarksExported(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/utils.ts": `function helper() { return 1 }
const value = 2
export { helper, value }`,
	}, false)

	helper, ok := findFact(ff, "src.helper")
	if !ok {
		t.Fatal("expected fact src.helper")
	}
	if helper.Props["exported"] != true {
		t.Errorf("helper exported = %v, want true (export { helper })", helper.Props["exported"])
	}
	value, ok := findFact(ff, "src.value")
	if !ok {
		t.Fatal("expected fact src.value")
	}
	if value.Props["exported"] != true {
		t.Errorf("value exported = %v, want true (export { value })", value.Props["exported"])
	}
}

func TestExtract_HookClassification(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/hooks/useAuth.ts": `export function useAuth() { return null }
export const useUser = () => null`,
	}, false)

	for _, name := range []string{"src/hooks.useAuth", "src/hooks.useUser"} {
		f, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("expected fact %s", name)
		}
		if f.Props["web_component"] != "hook" {
			t.Errorf("%s web_component = %v, want hook", name, f.Props["web_component"])
		}
		if f.Props["framework"] != "react" {
			t.Errorf("%s framework = %v, want react", name, f.Props["framework"])
		}
	}
}

// extractAllSvelteKit is extractAll's SvelteKit counterpart: it drops a
// svelte.config.js into the temp project so detectSvelteKit(dir) is true, then
// extracts the given plain .ts files (route/hook files, not .svelte SFCs) through
// the normal extractFile path.
func extractAllSvelteKit(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "svelte.config.js"), []byte(`export default {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for relPath, content := range files {
		absPath := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
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

func TestExtract_SvelteKitLoadClassification(t *testing.T) {
	ff := extractAllSvelteKit(t, map[string]string{
		"src/routes/+page.ts": `export const load = async ({ params }) => { return {}; }`,
	})
	f, ok := findFact(ff, "src/routes.load")
	if !ok {
		t.Fatalf("expected fact src/routes.load; got %v", factNames(ff))
	}
	if f.Props["web_component"] != "route_handler" {
		t.Errorf("load web_component = %v, want route_handler", f.Props["web_component"])
	}
	if f.Props["framework"] != "sveltekit" {
		t.Errorf("load framework = %v, want sveltekit", f.Props["framework"])
	}
}

func TestExtract_SvelteKitVirtualDependencyIsFrameworkProvided(t *testing.T) {
	ff := extractAllSvelteKit(t, map[string]string{
		"src/routes/+page.ts": `import { goto } from "$app/navigation"; export const load = () => goto("/");`,
	})
	for _, f := range ff {
		if f.Kind == facts.KindDependency && f.Name == "src/routes -> $app/navigation" {
			if f.Props["source"] != "framework" {
				t.Fatalf("source = %v, want framework", f.Props["source"])
			}
			return
		}
	}
	t.Fatalf("missing $app dependency in %v", factNames(ff))
}

func TestExtract_SvelteKitServerLoadClassification(t *testing.T) {
	ff := extractAllSvelteKit(t, map[string]string{
		"src/routes/+page.server.ts":   `export const load = async () => { return {}; }`,
		"src/routes/+layout.server.ts": `export const load = async () => { return {}; }`,
	})
	for _, name := range []string{"src/routes.load"} {
		f, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("expected fact %s; got %v", name, factNames(ff))
		}
		if f.Props["web_component"] != "route_handler" {
			t.Errorf("%s web_component = %v, want route_handler", name, f.Props["web_component"])
		}
	}
}

func TestExtract_SvelteKitServerRouteClassification(t *testing.T) {
	ff := extractAllSvelteKit(t, map[string]string{
		"src/routes/api/users/+server.ts": `export async function GET() { return new Response(); }
export async function POST() { return new Response(); }`,
	})
	get, ok := findFact(ff, "src/routes/api/users.GET")
	if !ok {
		t.Fatalf("expected fact src/routes/api/users.GET; got %v", factNames(ff))
	}
	if get.Props["web_component"] != "route_handler" {
		t.Errorf("GET web_component = %v, want route_handler", get.Props["web_component"])
	}
	if get.Props["method"] != "GET" {
		t.Errorf("GET method = %v, want GET", get.Props["method"])
	}
	if get.Props["framework"] != "sveltekit" {
		t.Errorf("GET framework = %v, want sveltekit", get.Props["framework"])
	}
	post, ok := findFact(ff, "src/routes/api/users.POST")
	if !ok {
		t.Fatalf("expected fact src/routes/api/users.POST")
	}
	if post.Props["web_component"] != "route_handler" {
		t.Errorf("POST web_component = %v, want route_handler", post.Props["web_component"])
	}
}

func TestExtract_SvelteKitHooksServerClassification(t *testing.T) {
	ff := extractAllSvelteKit(t, map[string]string{
		"src/hooks.server.ts": `export const handle = async ({ event, resolve }) => resolve(event);
export const handleError = async ({ error }) => { console.error(error); };
export const handleFetch = async ({ request, fetch }) => fetch(request);`,
	})
	for _, name := range []string{"src.handle", "src.handleError", "src.handleFetch"} {
		f, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("expected fact %s; got %v", name, factNames(ff))
		}
		if f.Props["web_component"] != "route_handler" {
			t.Errorf("%s web_component = %v, want route_handler", name, f.Props["web_component"])
		}
		if f.Props["framework"] != "sveltekit" {
			t.Errorf("%s framework = %v, want sveltekit", name, f.Props["framework"])
		}
	}
}

// A plain .ts file exporting a function named "load" outside routes/ (or in a
// SvelteKit project that isn't actually routing this file) must NOT be tagged —
// the classification is scoped to the file-name+directory construct, not the name
// alone, or a coincidental helper named "load" would be hidden from find_orphans.
func TestExtract_SvelteKitLoad_ScopedToRoutesDir(t *testing.T) {
	ff := extractAllSvelteKit(t, map[string]string{
		"src/lib/loader.ts": `export const load = async () => { return {}; }`,
	})
	f, ok := findFact(ff, "src/lib.load")
	if !ok {
		t.Fatalf("expected fact src/lib.load; got %v", factNames(ff))
	}
	if f.Props["web_component"] == "route_handler" {
		t.Error("load outside routes/ must not be classified as route_handler")
	}
}

func TestExtract_RouteHandlerClassification(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/app/api/users/route.ts": `export async function GET() { return Response.json([]) }
export async function POST() { return Response.json({}) }`,
	}, true)

	get, ok := findFact(ff, "src/app/api/users.GET")
	if !ok {
		t.Fatalf("expected fact src/app/api/users.GET; got %v", factNames(ff))
	}
	if get.Props["web_component"] != "route_handler" {
		t.Errorf("GET web_component = %v, want route_handler", get.Props["web_component"])
	}
	if get.Props["method"] != "GET" {
		t.Errorf("GET method = %v, want GET", get.Props["method"])
	}
	if _, ok := findFact(ff, "src/app/api/users.POST"); !ok {
		t.Error("expected fact src/app/api/users.POST")
	}
}

func TestExtract_Enum(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/types/colors.ts": `export enum Color { Red, Green, Blue }`,
	}, false)

	f, ok := findFact(ff, "src/types.Color")
	if !ok {
		t.Fatalf("expected fact src/types.Color; got %v", factNames(ff))
	}
	if f.Props["symbol_kind"] != facts.SymbolEnum {
		t.Errorf("symbol_kind = %v, want enum", f.Props["symbol_kind"])
	}
	if f.Props["exported"] != true {
		t.Errorf("exported = %v, want true", f.Props["exported"])
	}
}

func TestExtract_NonComponentClassNotClassified(t *testing.T) {
	// A PascalCase service class with no JSX in a .ts file must not be tagged a component.
	ff := extractAll(t, map[string]string{
		"src/services/ApiClient.ts": `export class ApiClient { fetchAll() { return [] } }`,
	}, false)

	f, ok := findFact(ff, "src/services.ApiClient")
	if !ok {
		t.Fatal("expected fact src/services.ApiClient")
	}
	if _, tagged := f.Props["web_component"]; tagged {
		t.Errorf("ApiClient should not be classified as a component; got %v", f.Props["web_component"])
	}
}

func factNames(ff []facts.Fact) []string {
	var names []string
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			names = append(names, f.Name)
		}
	}
	return names
}

func TestIsTypeScriptFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"src/app.ts", true},
		{"src/app.tsx", true},
		{"src/app.js", true},
		{"src/app.jsx", true},
		{"src/app.vue", true},
		{"src/app.go", false},
	}
	for _, tt := range tests {
		if got := isTypeScriptFile(tt.path); got != tt.want {
			t.Errorf("isTypeScriptFile(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// --- File-scope reference pass (KindFileRef) ---

// fileRefTargets returns the RelCalls targets of the KindFileRef fact emitted for the
// given source file, or nil if none was emitted.
func fileRefTargets(ff []facts.Fact, file string) []string {
	for _, f := range ff {
		if f.Kind == facts.KindFileRef && f.File == file {
			var out []string
			for _, r := range f.Relations {
				if r.Kind == facts.RelCalls {
					out = append(out, r.Target)
				}
			}
			return out
		}
	}
	return nil
}

func hasTarget(targets []string, want string) bool {
	for _, t := range targets {
		if t == want {
			return true
		}
	}
	return false
}

// A component rendered only in JSX (never called) must still produce a usage
// reference, or the dead-code detector flags every React component as dead.
func TestExtract_FileRef_JSXComponentUsage(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/App.tsx":    `import Header from './Header'` + "\n" + `export default function App() { return <Header />; }`,
		"src/Header.tsx": `export default function Header() { return null; }`,
	}, false)

	targets := fileRefTargets(ff, "src/App.tsx")
	if !hasTarget(targets, "src.Header") {
		t.Errorf("App.tsx file_ref should reference src.Header via JSX; got %v", targets)
	}
	// The referenced component must exist as a symbol with the matching short name.
	if _, ok := findFact(ff, "src.Header"); !ok {
		t.Fatal("expected symbol fact src.Header")
	}
}

// A default-imported page referenced only as a value in a top-level route table
// (react-router-manager style) must be marked used.
func TestExtract_FileRef_RouteConfigIdentifier(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/routes.jsx": `import EventCalendar from './event_calendar'` + "\n" +
			`export default [{ path: '/calendar', component: EventCalendar }]`,
		"src/event_calendar/index.jsx": `export default function EventCalendar() { return null; }`,
	}, false)

	targets := fileRefTargets(ff, "src/routes.jsx")
	// Folder-index resolution binds the reference to the exact declaring dir.
	if !hasTarget(targets, "src/event_calendar.EventCalendar") {
		t.Errorf("routes.jsx file_ref should reference the routed component at its folder dir; got %v", targets)
	}
	if _, ok := findFact(ff, "src/event_calendar.EventCalendar"); !ok {
		t.Fatal("expected symbol fact src/event_calendar.EventCalendar")
	}
}

// A namespace import used via member access (utils.foo) marks the member used.
func TestExtract_FileRef_NamespaceMember(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/config.ts": `import * as helpers from './helpers'` + "\n" +
			`export const value = helpers.compute()`,
		"src/helpers.ts": `export function compute() { return 1; }`,
	}, false)

	targets := fileRefTargets(ff, "src/config.ts")
	if !hasTarget(targets, "src.compute") {
		t.Errorf("config.ts file_ref should reference src.compute via namespace member; got %v", targets)
	}
}

// CommonJS require() bindings used at file scope mark the required symbol used.
func TestExtract_FileRef_RequireBinding(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"server/index.js": `const { registerRoutes } = require('./routes')` + "\n" +
			`registerRoutes()`,
		"server/routes.js": `function registerRoutes() {}` + "\n" + `module.exports = { registerRoutes }`,
	}, false)

	targets := fileRefTargets(ff, "server/index.js")
	if !hasTarget(targets, "server.registerRoutes") {
		t.Errorf("index.js file_ref should reference server.registerRoutes via require; got %v", targets)
	}
}

// require() and dynamic import() must produce module dependency edges so CommonJS and
// code-split trees are not invisible to the import graph.
func TestExtract_DynamicAndRequireDependencyEdges(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/loader.ts": `export async function load() {` + "\n" +
			`  const mod = await import('./heavy');` + "\n" +
			`  const cfg = require('./cfg');` + "\n" +
			`  return mod;` + "\n" + `}`,
		"src/heavy.ts": `export function heavy() {}`,
		"src/cfg.ts":   `export const cfg = {}`,
	}, false)

	deps := findFactsByKind(ff, facts.KindDependency)
	wantTargets := []string{"src/heavy", "src/cfg"}
	for _, want := range wantTargets {
		found := false
		for i := range deps {
			if hasRelation(deps[i], facts.RelImports, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a dependency edge importing %s (require/dynamic import)", want)
		}
	}
}

// A same-module function used only at module scope — called at the top level or
// passed as a value to an HOC (connect(mapStateToProps)) — is invisible to the
// per-function call walk. The file-ref pass must still record it, or it is falsely
// reported dead.
func TestExtract_FileRef_SameModuleUsePositions(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"client/index.jsx": `function startSession() {}` + "\n" +
			`function mapStateToProps() {}` + "\n" +
			`startSession()` + "\n" +
			`export default connect(mapStateToProps)(App)`,
	}, false)

	targets := fileRefTargets(ff, "client/index.jsx")
	for _, want := range []string{"client.startSession", "client.mapStateToProps", "client.App"} {
		if !hasTarget(targets, want) {
			t.Errorf("index.jsx file_ref should reference %s (module-scope call / HOC arg); got %v", want, targets)
		}
	}
}

// The pervasive `export default connect(...)(Foo)` HOC pattern: the anonymous default
// export of a folder index is named "<Folder>Index" by fileSymbolName, but consumers
// import it as the component's own name. A default import must reference that
// default-export symbol, or the wrapper is falsely reported dead.
func TestExtract_FileRef_FolderIndexAnonymousDefault(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/app.jsx": `import FeedItem from './feed_item'` + "\n" +
			`export default function App() { return <FeedItem />; }`,
		"src/feed_item/index.jsx": `function FeedItem() { return null; }` + "\n" +
			`export default connect(mapStateToProps)(FeedItem);`,
	}, false)

	// The anonymous connect() wrapper is named FeedItemIndex by fileSymbolName.
	if _, ok := findFact(ff, "src/feed_item.FeedItemIndex"); !ok {
		t.Fatal("expected default-export wrapper symbol src/feed_item.FeedItemIndex")
	}
	targets := fileRefTargets(ff, "src/app.jsx")
	if !hasTarget(targets, "src/feed_item.FeedItemIndex") {
		t.Errorf("app.jsx must reference the module's default export (the wrapper); got %v", targets)
	}
	// The inner component is still referenced too (via JSX), at its folder dir.
	if !hasTarget(targets, "src/feed_item.FeedItem") {
		t.Errorf("app.jsx should also reference the inner component; got %v", targets)
	}
}

// A named import through a folder index resolves to the exact declaring dir (not the
// folder's parent), so the reference matches the declared symbol by full name.
func TestExtract_FileRef_FolderIndexNamedImport(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/consumer.ts": `import { helper } from './lib'` + "\n" +
			`export const x = helper()`,
		"src/lib/index.ts": `export function helper() { return 1; }`,
	}, false)

	targets := fileRefTargets(ff, "src/consumer.ts")
	if !hasTarget(targets, "src/lib.helper") {
		t.Errorf("consumer.ts should reference src/lib.helper at the folder dir; got %v", targets)
	}
}

// `export { default as X } from './folder'` re-exports the folder's default export;
// the literal "default" is resolved to the folder-index default name.
func TestExtract_FileRef_ReexportDefault(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/barrel.jsx": `export { default as FeedItem } from './feed_item'`,
		"src/feed_item/index.jsx": `function FeedItem() { return null; }` + "\n" +
			`export default connect(x)(FeedItem);`,
	}, false)

	targets := fileRefTargets(ff, "src/barrel.jsx")
	if !hasTarget(targets, "src/feed_item.FeedItemIndex") {
		t.Errorf("barrel should reference the re-exported default (FeedItemIndex); got %v", targets)
	}
}

// A class method referenced only as an event-handler value (onClick={this.handleClick})
// is never called by name, so React binds it as a prop — it must still be recorded as
// used via the `this.<member>` reference, or it is falsely reported dead.
func TestExtract_ThisMemberReference_EventHandler(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/widget.tsx": `export class Widget {
  handleClick() { return 1; }
  render() { return <button onClick={this.handleClick} />; }
}`,
	}, false)

	render, ok := findFact(ff, "src.Widget.render")
	if !ok {
		t.Fatal("expected method fact src.Widget.render")
	}
	if !hasRelation(render, facts.RelCalls, "src.Widget.handleClick") {
		t.Errorf("render should reference this.handleClick (event handler value); relations: %v", render.Relations)
	}
}

// TestResolveImportPath_LongestAliasWinsDeterministically pins both halves of the
// overlapping-alias fix: the most specific alias is chosen (tsconfig `paths`
// semantics), and the choice does not depend on Go's randomized map iteration.
//
// The determinism half is the one that bit: a monorepo mapping a package to BOTH its
// source and its built output resolved the same import differently between runs, so
// `enola check` reported edge churn on an untouched tree. Repeating the resolution is
// what makes this a regression test rather than a coin flip that happened to land.
func TestResolveImportPath_LongestAliasWinsDeterministically(t *testing.T) {
	aliases := map[string]tsAlias{
		"@acme/":       {replacement: "public/@acme/"},
		"@acme/schema": {replacement: "packages/acme-schema/src"},
		"@":            {replacement: "src/"},
	}

	for _, tc := range []struct {
		importPath string
		want       string
	}{
		// Three aliases match; the longest is the answer, not whichever came first.
		{"@acme/schema/veneer", "packages/acme-schema/src/veneer"},
		// Only the two shorter ones match here.
		{"@acme/runtime", "public/@acme/runtime"},
		{"@utils/thing", "src/utils/thing"},
	} {
		for i := 0; i < 50; i++ {
			got, external := resolveImportPath(tc.importPath, "src/app", aliases)
			if external {
				t.Fatalf("%s: resolved as external", tc.importPath)
			}
			if got != tc.want {
				t.Fatalf("%s: got %q, want %q (iteration %d — alias choice is order-dependent)",
					tc.importPath, got, tc.want, i)
			}
		}
	}
}

func TestResolveImportPath_NonAliasPathsUnchanged(t *testing.T) {
	aliases := map[string]tsAlias{"@/": {replacement: "src/"}}
	if got, ext := resolveImportPath("./sibling", "src/app", aliases); ext || got != "src/app/sibling" {
		t.Errorf("relative import = %q external=%v", got, ext)
	}
	if got, ext := resolveImportPath("react", "src/app", aliases); !ext || got != "react" {
		t.Errorf("bare package should stay external, got %q external=%v", got, ext)
	}
}

func TestDetect_HotwireStimulusApp(t *testing.T) {
	dir := t.TempDir()
	pkg := `{"dependencies": {"@hotwired/stimulus": "^3.2.0", "@hotwired/turbo-rails": "^8.0.0"}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err := New().Detect(dir)
	if err != nil || !ok {
		t.Fatalf("Detect = %v, %v — a plain-JS Hotwire app must be detected without tsconfig or TS deps", ok, err)
	}
}

// TestCommonJSExportAssignments: `exports.name = function` and
// `module.exports.name = function` declare that name as an exported function
// symbol — the whole public surface of a classic Node module. Everything short
// of that shape (a plain value, a re-exported identifier, whole-object
// module.exports) emits no symbol: there is no member name to carry, or no
// declaration to classify.
func TestCommonJSExportAssignments(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"controllers/main.js": `'use strict'

exports.index = function(req, res){
  res.render('index');
};

module.exports.list = (req, res) => {
  res.json([]);
};

exports.VERSION = '1.0';
exports.helper = someImportedName;
module.exports = function whole(){};
`,
	}, false)

	index, ok := findFact(ff, "controllers.index")
	if !ok {
		t.Fatalf("exports.index did not become a symbol; facts: %+v", ff)
	}
	if index.Props["symbol_kind"] != facts.SymbolFunc || index.Props["exported"] != true {
		t.Errorf("exports.index props = %+v, want an exported function", index.Props)
	}
	list, ok := findFact(ff, "controllers.list")
	if !ok {
		t.Fatalf("module.exports.list did not become a symbol")
	}
	if list.Props["symbol_kind"] != facts.SymbolFunc || list.Props["exported"] != true {
		t.Errorf("module.exports.list props = %+v, want an exported function", list.Props)
	}
	for _, name := range []string{"controllers.VERSION", "controllers.helper", "controllers.whole"} {
		if _, found := findFact(ff, name); found {
			t.Errorf("%s emitted a symbol; only the member-assignment-of-a-function shape declares", name)
		}
	}
}

func TestExtract_MJSModules(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/helper.mjs": `export function formatName(user) {
  return user.first + " " + user.last;
}
`,
		"src/index.mjs": `import { formatName } from "./helper.mjs";

export function greet(user) {
  return "hello " + formatName(user);
}
`,
	}, false)

	formatName, ok := findFact(ff, "src.formatName")
	if !ok {
		t.Fatalf("an .mjs ESM export did not become a symbol; facts: %+v", ff)
	}
	if formatName.Props["symbol_kind"] != facts.SymbolFunc || formatName.Props["exported"] != true {
		t.Errorf("src.formatName props = %+v, want an exported function", formatName.Props)
	}
	greet, ok := findFact(ff, "src.greet")
	if !ok {
		t.Fatal("the importing .mjs file's export did not become a symbol")
	}
	if !hasRelation(greet, facts.RelCalls, "src.formatName") {
		t.Errorf("greet should carry a calls edge to the imported .mjs function; relations = %v", greet.Relations)
	}
}

func TestDetect_ImportmapRailsApp(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	pin := `pin "@hotwired/stimulus", to: "stimulus.min.js"` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config", "importmap.rb"), []byte(pin), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err := New().Detect(dir)
	if err != nil || !ok {
		t.Fatalf("Detect = %v, %v — an importmap-rails app pins in Ruby and has no package.json", ok, err)
	}

	bare := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bare, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	ok, err = New().Detect(bare)
	if err != nil || ok {
		t.Fatalf("Detect = %v, %v — a config/ directory alone is not a JavaScript project", ok, err)
	}
}

func TestDetect_PlainNodePackageWithoutDependencies(t *testing.T) {
	dir := t.TempDir()
	pkg := `{"name": "some-cli", "type": "module", "bin": {"some-cli": "./bin/cli.js"}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err := New().Detect(dir)
	if err != nil || !ok {
		t.Fatalf("Detect = %v, %v — a dependency-free Node package must be detected", ok, err)
	}

	bare := t.TempDir()
	if err := os.WriteFile(filepath.Join(bare, "package.json"), []byte(`{"name": "marker-only"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err = New().Detect(bare)
	if err != nil || ok {
		t.Fatalf("Detect = %v, %v — a bare name-holding stub is a marker file, not a package", ok, err)
	}
}

func TestDetect_DenoProject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deno.jsonc"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err := New().Detect(dir)
	if err != nil || !ok {
		t.Fatalf("Detect = %v, %v — a Deno project has TypeScript and no package.json", ok, err)
	}
}

// A tsconfig `paths` pattern with no `*` matches the whole specifier. Until v134 the
// parser required a `*` on BOTH sides and silently dropped the exact form, which is how
// a monorepo names a sibling package:
//
//	"@excalidraw/common":   ["./common/src/index.ts"]   <- dropped
//	"@excalidraw/common/*": ["./common/src/*"]          <- kept
//
// So `import { randomId } from "@excalidraw/common"` (no subpath) matched nothing, was
// classified external, and its call edge fell back to the CALLER's directory — a phantom
// node per calling package. Measured on excalidraw: 4,847 internal call edges dangling,
// and impact_analysis/traverse blind to every cross-package symbol.
func TestParseTSConfigAliases_ExactPatternIsRecorded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tsconfig.json")
	if err := os.WriteFile(path, []byte(`{"compilerOptions":{"paths":{
		"@acme/common":   ["./packages/common/src/index.ts"],
		"@acme/common/*": ["./packages/common/src/*"]
	}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	aliases, ok := tryParseTSConfigAliases(path)
	if !ok {
		t.Fatal("no aliases parsed")
	}
	exact, found := aliases["@acme/common"]
	if !found {
		t.Fatalf("exact pattern dropped; got %v", aliases)
	}
	if !exact.exact {
		t.Errorf("exact pattern not marked exact: %+v", exact)
	}
	if exact.replacement != "packages/common/src/index.ts" {
		t.Errorf("replacement = %q", exact.replacement)
	}
	if wc, found := aliases["@acme/common/"]; !found || wc.exact {
		t.Errorf("wildcard form should survive as a prefix, got %+v found=%v", wc, found)
	}
}

func TestParseTSConfigAliases_FollowsExtendsAndRebasesDeclaringFile(t *testing.T) {
	dir := t.TempDir()
	generated := filepath.Join(dir, ".svelte-kit")
	if err := os.MkdirAll(generated, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(generated, "tsconfig.json"), []byte(`{
		// Generated aliases are relative to this file, not to its child.
		"compilerOptions": { "paths": { "src/*": ["../src/*"], }, },
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "tsconfig.json")
	if err := os.WriteFile(child, []byte(`{"extends":"./.svelte-kit/tsconfig.json"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	aliases, ok := tryParseTSConfigAliases(child)
	if !ok {
		t.Fatal("no inherited aliases parsed")
	}
	if got := aliases["src/"].replacement; got != "src/" {
		t.Fatalf("inherited replacement = %q, want src/", got)
	}
}

func TestParseTSConfigAliases_ChildPathsReplaceParent(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	if err := os.WriteFile(base, []byte(`{"compilerOptions":{"paths":{"parent/*":["src/parent/*"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "tsconfig.json")
	if err := os.WriteFile(child, []byte(`{"extends":"./base","compilerOptions":{"paths":{"child/*":["src/child/*"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	aliases, ok := tryParseTSConfigAliases(child)
	if !ok {
		t.Fatal("no child aliases parsed")
	}
	if _, found := aliases["parent/"]; found {
		t.Fatalf("parent paths must be replaced: %v", aliases)
	}
	if got := aliases["child/"].replacement; got != "src/child/" {
		t.Fatalf("child replacement = %q", got)
	}
}

func TestParseTSConfigAliases_ExtendsCycleFailsClosed(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	if err := os.WriteFile(a, []byte(`{"extends":"./b.json"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte(`{"extends":"./a.json"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if aliases, ok := tryParseTSConfigAliases(a); ok || aliases != nil {
		t.Fatalf("cycle parsed: %v", aliases)
	}
}

// The reason the match mode is carried explicitly rather than inferred: an exact entry
// stored as a bare prefix would also swallow a DIFFERENT package whose name extends it,
// drawing edges into code that was never imported.
func TestResolveImportPath_ExactAliasDoesNotSwallowSiblingPackage(t *testing.T) {
	aliases := map[string]tsAlias{
		"@acme/common": {replacement: "packages/common/src/index.ts", exact: true},
	}
	got, external := resolveImportPath("@acme/common", "packages/element/src", aliases)
	if external || got != "packages/common/src/index.ts" {
		t.Errorf("exact hit = %q external=%v", got, external)
	}
	// A sibling package sharing the prefix must NOT match the exact entry.
	if got, external := resolveImportPath("@acme/common-utils", "packages/element/src", aliases); !external {
		t.Errorf("@acme/common-utils should be external, got %q", got)
	}
}

// tsconfig resolves most-specific-first, and an exact pattern is maximally specific, so
// it must beat a prefix that also matches rather than being ranked against it by length.
func TestResolveImportPath_ExactBeatsPrefix(t *testing.T) {
	aliases := map[string]tsAlias{
		"@acme/":       {replacement: "public/@acme/"},
		"@acme/common": {replacement: "packages/common/src/index.ts", exact: true},
	}
	if got, _ := resolveImportPath("@acme/common", "src", aliases); got != "packages/common/src/index.ts" {
		t.Errorf("exact should win over prefix, got %q", got)
	}
	if got, _ := resolveImportPath("@acme/other", "src", aliases); got != "public/@acme/other" {
		t.Errorf("prefix still applies to non-exact specifiers, got %q", got)
	}
}

func TestExtract_MemberDecoratorsAndGetterKind(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/panel.ts": `import { cached, tracked } from '@glimmer/tracking';
import { action } from '@ember/object';

@classic
export default class Panel {
  @tracked count = 0;

  @cached
  @cached
  get expensive() {
    return this.compute() + this.helper();
  }

  get plain() {
    return 1;
  }

  set volume(v) {
    this.count = v;
  }

  @action
  submit() {
    this.compute();
  }

  compute() {
    return 1;
  }

  helper() {
    return 2;
  }
}
`,
	}, false)

	panel, ok := findFact(ff, "src.Panel")
	if !ok {
		t.Fatalf("class fact missing; facts: %+v", ff)
	}
	if got, want := panel.Props["decorators"], "classic"; got != want {
		t.Errorf("class decorators = %v, want %v", got, want)
	}

	expensive, ok := findFact(ff, "src.Panel.expensive")
	if !ok {
		t.Fatal("decorated getter emitted no symbol")
	}
	if expensive.Props["symbol_kind"] != facts.SymbolGetter {
		t.Errorf("symbol_kind = %v, want getter", expensive.Props["symbol_kind"])
	}
	if got, want := expensive.Props["decorators"], "cached"; got != want {
		t.Errorf("getter decorators = %v, want deduped %v", got, want)
	}
	if got := expensive.Props["getter_calls"]; got != 2 {
		t.Errorf("getter_calls = %v, want 2", got)
	}

	plain, ok := findFact(ff, "src.Panel.plain")
	if !ok {
		t.Fatal("undecorated getter emitted no symbol")
	}
	if plain.Props["symbol_kind"] != facts.SymbolGetter {
		t.Errorf("plain getter symbol_kind = %v, want getter", plain.Props["symbol_kind"])
	}
	if _, present := plain.Props["decorators"]; present {
		t.Errorf("undecorated getter must carry no decorators prop, got %v", plain.Props["decorators"])
	}
	if got := plain.Props["getter_calls"]; got != 0 {
		t.Errorf("plain getter_calls = %v, want measured 0", got)
	}

	volume, ok := findFact(ff, "src.Panel.volume")
	if !ok {
		t.Fatal("setter emitted no symbol")
	}
	if volume.Props["symbol_kind"] != facts.SymbolMethod {
		t.Errorf("setter symbol_kind = %v, want method — only get accessors are getters", volume.Props["symbol_kind"])
	}

	count, ok := findFact(ff, "src.Panel.count")
	if !ok {
		t.Fatal("decorated field emitted no symbol")
	}
	if got, want := count.Props["decorators"], "tracked"; got != want {
		t.Errorf("field decorators = %v, want %v", got, want)
	}

	submit, ok := findFact(ff, "src.Panel.submit")
	if !ok {
		t.Fatal("decorated method emitted no symbol")
	}
	if submit.Props["symbol_kind"] != facts.SymbolMethod {
		t.Errorf("decorated method symbol_kind = %v, want method", submit.Props["symbol_kind"])
	}
	if got, want := submit.Props["decorators"], "action"; got != want {
		t.Errorf("method decorators = %v, want %v", got, want)
	}
	if _, present := submit.Props["getter_calls"]; present {
		t.Error("a plain method must not carry getter_calls")
	}

	compute, ok := findFact(ff, "src.Panel.compute")
	if !ok {
		t.Fatal("undecorated method emitted no symbol")
	}
	if _, present := compute.Props["decorators"]; present {
		t.Errorf("undecorated method must carry no decorators prop, got %v", compute.Props["decorators"])
	}
}
