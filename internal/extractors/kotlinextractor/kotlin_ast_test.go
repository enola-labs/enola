package kotlinextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func extractAST(t *testing.T, src string, isAndroid bool) []facts.Fact {
	t.Helper()
	return extractFileAST([]byte(src), "pkg/test.kt", isAndroid, "", "", nil)
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

// --- Parity with the legacy regex extractor (covers TestExtract_* cases) ---

// TestAST_DaggerDI tags Dagger/Hilt DI infrastructure: a @Module class gets
// di_module, a @Subcomponent interface gets di_component.
func TestAST_DaggerDI(t *testing.T) {
	ff := extractAST(t, `
package pkg
import dagger.Module
import dagger.Subcomponent
@Module
class NetModule
@Subcomponent
interface FeatureComponent
`, true) // isAndroid — DI detection runs in addAndroidProps

	mod, ok := findFact(ff, "pkg.NetModule")
	if !ok {
		t.Fatal("expected fact pkg.NetModule")
	}
	if mod.Props["di_module"] != true {
		t.Errorf("NetModule should have di_module=true, got %v", mod.Props)
	}

	comp, ok := findFact(ff, "pkg.FeatureComponent")
	if !ok {
		t.Fatal("expected fact pkg.FeatureComponent")
	}
	if comp.Props["di_component"] != true {
		t.Errorf("FeatureComponent should have di_component=true, got %v", comp.Props)
	}
}

// A Hilt module with @InstallIn is installed by Hilt and named by nothing, so it is
// framework-registered; a plain Dagger @Module is listed by its component and is not.
func TestAST_HiltInstallInModuleIsFrameworkRegistered(t *testing.T) {
	hilt := extractAST(t, `
package pkg
import dagger.Module
import dagger.hilt.InstallIn
@Module
@InstallIn(SingletonComponent::class)
abstract class DataModule {
}
`, true)
	if mod, ok := findFact(hilt, "pkg.DataModule"); !ok || mod.Props["framework_registered"] != true {
		t.Errorf("DataModule should be framework_registered, got %v", mod.Props)
	}
	dagger := extractAST(t, `
package pkg
import dagger.Module
@Module
class NetModule {
}
`, true)
	if mod, ok := findFact(dagger, "pkg.NetModule"); !ok || mod.Props["framework_registered"] == true {
		t.Errorf("NetModule has no @InstallIn and should not be framework_registered, got %v", mod.Props)
	}
}

func TestAST_DataClass(t *testing.T) {
	ff := extractAST(t, `
package pkg
data class User(val name: String, val email: String) {
}
`, false)
	f, ok := findFact(ff, "pkg.User")
	if !ok {
		t.Fatal("expected fact for pkg.User")
	}
	if f.Props["data_class"] != true {
		t.Errorf("data_class = %v, want true", f.Props["data_class"])
	}
	if f.Props["symbol_kind"] != facts.SymbolClass {
		t.Errorf("symbol_kind = %v, want class", f.Props["symbol_kind"])
	}
}

func TestAST_SealedClass(t *testing.T) {
	ff := extractAST(t, `
package pkg
sealed class Result {
}
`, false)
	f, ok := findFact(ff, "pkg.Result")
	if !ok {
		t.Fatal("expected fact for pkg.Result")
	}
	if f.Props["sealed"] != true {
		t.Errorf("sealed = %v, want true", f.Props["sealed"])
	}
}

func TestAST_InterfaceDeclaration(t *testing.T) {
	ff := extractAST(t, `
package pkg
interface UserRepository {
    suspend fun getUsers(): List<User>
}
`, false)
	f, ok := findFact(ff, "pkg.UserRepository")
	if !ok {
		t.Fatal("expected fact for pkg.UserRepository")
	}
	if f.Props["symbol_kind"] != facts.SymbolInterface {
		t.Errorf("symbol_kind = %v, want interface", f.Props["symbol_kind"])
	}
}

func TestAST_ObjectDeclaration(t *testing.T) {
	ff := extractAST(t, `
package pkg
object AppModule : Module {
}
`, false)
	f, ok := findFact(ff, "pkg.AppModule")
	if !ok {
		t.Fatal("expected fact for pkg.AppModule")
	}
	if f.Props["object"] != true {
		t.Errorf("object = %v, want true", f.Props["object"])
	}
	if !hasRelation(f, facts.RelImplements, "Module") {
		t.Error("expected implements relation for Module")
	}
}

func TestAST_SuspendFunction(t *testing.T) {
	ff := extractAST(t, `
package pkg
suspend fun fetchUsers() {
}
`, false)
	f, ok := findFact(ff, "pkg.fetchUsers")
	if !ok {
		t.Fatal("expected fact for pkg.fetchUsers")
	}
	if f.Props["suspend"] != true {
		t.Errorf("suspend = %v, want true", f.Props["suspend"])
	}
}

func TestAST_ComposableFunction(t *testing.T) {
	ff := extractAST(t, `
package pkg
@Composable
fun HomeScreen() {
}
`, true)
	f, ok := findFact(ff, "pkg.HomeScreen")
	if !ok {
		t.Fatal("expected fact for pkg.HomeScreen")
	}
	if f.Props["android_component"] != "composable" {
		t.Errorf("android_component = %v, want composable", f.Props["android_component"])
	}
}

func TestAST_HiltViewModel(t *testing.T) {
	ff := extractAST(t, `
package pkg
@HiltViewModel
class HomeViewModel @Inject constructor(
    private val repo: UserRepository
) : ViewModel() {
}
`, true)
	f, ok := findFact(ff, "pkg.HomeViewModel")
	if !ok {
		t.Fatal("expected fact for pkg.HomeViewModel")
	}
	if f.Props["android_component"] != "viewmodel" {
		t.Errorf("android_component = %v, want viewmodel", f.Props["android_component"])
	}
	if !hasRelation(f, facts.RelImplements, "ViewModel") {
		t.Error("expected implements relation for ViewModel")
	}
}

func TestAST_RoomStorage(t *testing.T) {
	ff := extractAST(t, `
package pkg
@Entity
class UserEntity {
}
`, true)
	st := findFactsByKind(ff, facts.KindStorage)
	if len(st) != 1 {
		t.Fatalf("expected 1 storage fact, got %d", len(st))
	}
	if st[0].Props["storage_kind"] != "entity" {
		t.Errorf("storage_kind = %v, want entity", st[0].Props["storage_kind"])
	}
}

func TestAST_Imports(t *testing.T) {
	ff := extractAST(t, `
package pkg
import kotlinx.coroutines.flow.Flow
import android.os.Bundle
`, false)
	deps := findFactsByKind(ff, facts.KindDependency)
	if len(deps) != 2 {
		t.Fatalf("expected 2 dependency facts, got %d (got: %+v)", len(deps), deps)
	}
}

// --- New edge types: RelInjects and RelInstantiates ---

func TestAST_InjectConstructor_EmitsInjects(t *testing.T) {
	ff := extractAST(t, `
package pkg
class JournalRepository @Inject constructor(
    private val api: ApiService,
    private val dao: JournalDao,
) {
}
`, false)
	f, ok := findFact(ff, "pkg.JournalRepository")
	if !ok {
		t.Fatal("expected fact for pkg.JournalRepository")
	}
	if !hasRelation(f, facts.RelInjects, "ApiService") {
		t.Error("expected RelInjects → ApiService")
	}
	if !hasRelation(f, facts.RelInjects, "JournalDao") {
		t.Error("expected RelInjects → JournalDao")
	}
}

func TestAST_HiltViewModel_InjectsParamTypes(t *testing.T) {
	ff := extractAST(t, `
package pkg
@HiltViewModel
class HomeViewModel @Inject constructor(
    private val repo: UserRepository,
    private val useCase: GetUsersUseCase,
) : ViewModel()
`, true)
	f, ok := findFact(ff, "pkg.HomeViewModel")
	if !ok {
		t.Fatal("expected fact for pkg.HomeViewModel")
	}
	if !hasRelation(f, facts.RelInjects, "UserRepository") {
		t.Error("expected RelInjects → UserRepository")
	}
	if !hasRelation(f, facts.RelInjects, "GetUsersUseCase") {
		t.Error("expected RelInjects → GetUsersUseCase")
	}
}

// The motivating case: a ViewModel that bypasses Hilt by direct-instantiating
// a UseCase as a constructor-parameter default. The legacy regex extractor
// missed this entirely; the AST extractor must surface the call site as a
// RelInstantiates edge so reverse-dependency queries on JournalCodecUseCase
// resolve back to JournalHistoryViewModel.
func TestAST_DirectInstantiation_InCtorParamDefault(t *testing.T) {
	ff := extractAST(t, `
package pkg
class JournalHistoryViewModel(
    private val journalCodecUseCase: JournalCodecUseCase = JournalCodecUseCase(),
) {
}
`, false)
	f, ok := findFact(ff, "pkg.JournalHistoryViewModel")
	if !ok {
		t.Fatal("expected fact for pkg.JournalHistoryViewModel")
	}
	if !hasRelation(f, facts.RelInstantiates, "JournalCodecUseCase") {
		t.Errorf("expected RelInstantiates → JournalCodecUseCase, got relations: %+v", f.Relations)
	}
}

func TestAST_DirectInstantiation_InPropertyInitializer(t *testing.T) {
	ff := extractAST(t, `
package pkg
class Foo {
    private val codec = JournalCodecUseCase()
}
`, false)
	f, ok := findFact(ff, "pkg.Foo")
	if !ok {
		t.Fatal("expected fact for pkg.Foo")
	}
	if !hasRelation(f, facts.RelInstantiates, "JournalCodecUseCase") {
		t.Errorf("expected RelInstantiates → JournalCodecUseCase, got %+v", f.Relations)
	}
}

func TestAST_HiltProvidesEmitsInstantiates(t *testing.T) {
	ff := extractAST(t, `
package pkg
@Module
@InstallIn(SingletonComponent::class)
object UseCaseModule {
    @Provides
    @Singleton
    fun provideJournalCodecUseCase(): JournalCodecUseCase = JournalCodecUseCase()
}
`, true)
	f, ok := findFact(ff, "pkg.UseCaseModule.provideJournalCodecUseCase")
	if !ok {
		t.Fatal("expected fact for pkg.UseCaseModule.provideJournalCodecUseCase (function)")
	}
	if !hasRelation(f, facts.RelInstantiates, "JournalCodecUseCase") {
		t.Errorf("expected RelInstantiates → JournalCodecUseCase on Hilt provider, got %+v", f.Relations)
	}
}

// Lowercase-prefix calls (regular function calls) are NOT treated as constructors,
// so they do not produce stray RelInstantiates edges.
func TestAST_LowercaseFunctionCall_NoInstantiate(t *testing.T) {
	ff := extractAST(t, `
package pkg
class Foo {
    private val v = computeSomething()
}
`, false)
	f, ok := findFact(ff, "pkg.Foo")
	if !ok {
		t.Fatal("expected fact for pkg.Foo")
	}
	for _, r := range f.Relations {
		if r.Kind == facts.RelInstantiates {
			t.Errorf("unexpected RelInstantiates edge: %+v", r)
		}
	}
	// A lowercase bare call is a regular function call → RelCalls (same package).
	if !hasRelation(f, facts.RelCalls, "pkg.computeSomething") {
		t.Errorf("expected RelCalls → pkg.computeSomething, got %+v", f.Relations)
	}
}

func TestAST_SamePackageFunctionCall(t *testing.T) {
	ff := extractAST(t, `
package pkg
fun caller() {
    helper()
}
fun helper() {}
`, false)
	caller, ok := findFact(ff, "pkg.caller")
	if !ok {
		t.Fatal("expected fact for pkg.caller")
	}
	if !hasRelation(caller, facts.RelCalls, "pkg.helper") {
		t.Errorf("expected RelCalls → pkg.helper, got %+v", caller.Relations)
	}
}

func TestAST_ImportedFunctionCall(t *testing.T) {
	src := `
package com.example.app
import com.example.util.formatName
fun caller() {
    formatName()
}
`
	// sourceRoot carries a trailing slash in real layouts (e.g. "app/src/main/kotlin/").
	ff := extractFileAST([]byte(src), "src/com/example/app/caller.kt", false, "src/", "com.example", nil)
	caller, ok := findFact(ff, "src/com/example/app.caller")
	if !ok {
		t.Fatal("expected fact for src/com/example/app.caller")
	}
	// formatName is imported from com.example.util → resolves to the canonical
	// symbol fact name in that package's directory.
	if !hasRelation(caller, facts.RelCalls, "src/com/example/util.formatName") {
		t.Errorf("expected RelCalls → src/com/example/util.formatName, got %+v", caller.Relations)
	}
}

func TestAST_ExternalImportedCall_NoEdge(t *testing.T) {
	src := `
package com.example.app
import kotlinx.coroutines.runBlocking
fun caller() {
    runBlocking()
}
`
	ff := extractFileAST([]byte(src), "com/example/app/caller.kt", false, "", "com.example", nil)
	caller, ok := findFact(ff, "com/example/app.caller")
	if !ok {
		t.Fatal("expected fact for com/example/app.caller")
	}
	// runBlocking is imported from an external package — no local fact, so no edge.
	for _, r := range caller.Relations {
		if r.Kind == facts.RelCalls {
			t.Errorf("unexpected RelCalls edge for external import: %+v", r)
		}
	}
}

func TestAST_MethodCallOnReceiver_EmitsShortNameEdge(t *testing.T) {
	// A method call on a receiver (navigation expression) can't be resolved to a
	// canonical fact name without type information, but the short-name-matching
	// dead-code detector still needs the reference — so we emit a RelCalls edge to
	// the bare member name so the target member isn't mis-reported as an orphan.
	ff := extractAST(t, `
package pkg
class Foo {
    fun run() {
        repository.save()
    }
}
`, false)
	run, ok := findFact(ff, "pkg.Foo.run")
	if !ok {
		t.Fatal("expected fact for pkg.Foo.run")
	}
	if !hasRelation(run, facts.RelCalls, "save") {
		t.Errorf("expected short-name RelCalls → save for repository.save(), got %+v", run.Relations)
	}
}

func TestAST_PropertyAccessOnReceiver_EmitsShortNameEdge(t *testing.T) {
	// Property/field access (not a call) also references its member by short name;
	// this rescues overridden properties (e.g. `override val uniqueId`) that are only
	// ever read polymorphically as `slot.uniqueId`.
	ff := extractAST(t, `
package pkg
class Foo {
    fun run() {
        val id = slot.uniqueId
    }
}
`, false)
	run, ok := findFact(ff, "pkg.Foo.run")
	if !ok {
		t.Fatal("expected fact for pkg.Foo.run")
	}
	if !hasRelation(run, facts.RelCalls, "uniqueId") {
		t.Errorf("expected short-name RelCalls → uniqueId for slot.uniqueId, got %+v", run.Relations)
	}
}

func TestAST_CallsOutsideFunctionBody(t *testing.T) {
	// Calls that live outside a function body must still be credited so their
	// targets aren't mis-reported as orphans: (1) supertype constructor-delegation
	// arguments on a class and an object, and (2) function default-parameter values.
	ff := extractAST(t, `
package pkg
data class NpeId(val id: String) : Enrichment(npeEntity(id))
object Single : Base(makeThing())
fun withDefault(x: List<Int> = scoreMessages()) {}
fun npeEntity(id: String) = 1
fun makeThing() = 2
fun scoreMessages() = listOf(1)
`, false)
	cases := []struct{ owner, target string }{
		{"pkg.NpeId", "pkg.npeEntity"},           // class delegation arg
		{"pkg.Single", "pkg.makeThing"},          // object delegation arg
		{"pkg.withDefault", "pkg.scoreMessages"}, // function default-param value
	}
	for _, c := range cases {
		f, ok := findFact(ff, c.owner)
		if !ok {
			t.Errorf("expected fact for %s", c.owner)
			continue
		}
		if !hasRelation(f, facts.RelCalls, c.target) {
			t.Errorf("%s: expected RelCalls → %s, got %+v", c.owner, c.target, f.Relations)
		}
	}
}

func TestAST_CallableReference(t *testing.T) {
	// A function used only via a callable reference (`::foo`) must still be credited
	// so it isn't mis-reported as an orphan.
	ff := extractAST(t, `
package pkg
fun caller() {
    register(onClick = ::doNothing)
    listOf(1).map(::helper)
}
fun doNothing() {}
fun helper(x: Int) = x
`, false)
	c, ok := findFact(ff, "pkg.caller")
	if !ok {
		t.Fatal("expected fact for pkg.caller")
	}
	for _, target := range []string{"doNothing", "helper"} {
		if !hasRelation(c, facts.RelCalls, target) {
			t.Errorf("expected short-name RelCalls → %s for ::%s, got %+v", target, target, c.Relations)
		}
	}
}

func TestAST_MemberFunctionIsMethodKind(t *testing.T) {
	// A `fun` inside a class/object is a method (parity with Go/Java), so the orphan
	// detector grades it low confidence, not the high-confidence bucket reserved for
	// plain functions. A top-level `fun` stays a function.
	ff := extractAST(t, `
package pkg
class Service {
    fun handle() {}
}
fun topLevel() {}
`, false)
	m, ok := findFact(ff, "pkg.Service.handle")
	if !ok {
		t.Fatal("expected fact for pkg.Service.handle")
	}
	if m.Props["symbol_kind"] != facts.SymbolMethod {
		t.Errorf("member fun symbol_kind = %v, want method", m.Props["symbol_kind"])
	}
	if m.Props["receiver"] != "Service" {
		t.Errorf("member fun receiver = %v, want Service", m.Props["receiver"])
	}
	f, ok := findFact(ff, "pkg.topLevel")
	if !ok {
		t.Fatal("expected fact for pkg.topLevel")
	}
	if f.Props["symbol_kind"] != facts.SymbolFunc {
		t.Errorf("top-level fun symbol_kind = %v, want function", f.Props["symbol_kind"])
	}
}

func TestAST_OverrideAndDIProviderProps(t *testing.T) {
	// override → framework/interface entry point; @Provides/@Binds → DI provider.
	// Both are dispatched by the runtime/container and must be tagged so the orphan
	// detector treats them as entry points rather than dead code.
	ff := extractAST(t, `
package pkg
class MyView : BaseView() {
    override fun onCreate() {}
}
class MyModule {
    @Provides
    fun provideThing(): Thing = Thing()
    @Binds
    fun bindOther(impl: OtherImpl): Other = impl
    fun plain() {}
}
`, false)
	oc, ok := findFact(ff, "pkg.MyView.onCreate")
	if !ok {
		t.Fatal("expected fact for pkg.MyView.onCreate")
	}
	if oc.Props["override"] != true {
		t.Errorf("override fun should carry override=true, got %+v", oc.Props)
	}
	prov, ok := findFact(ff, "pkg.MyModule.provideThing")
	if !ok {
		t.Fatal("expected fact for pkg.MyModule.provideThing")
	}
	if prov.Props["di_provider"] != true {
		t.Errorf("@Provides fun should carry di_provider=true, got %+v", prov.Props)
	}
	binds, ok := findFact(ff, "pkg.MyModule.bindOther")
	if !ok {
		t.Fatal("expected fact for pkg.MyModule.bindOther")
	}
	if binds.Props["di_provider"] != true {
		t.Errorf("@Binds fun should carry di_provider=true, got %+v", binds.Props)
	}
	plain, ok := findFact(ff, "pkg.MyModule.plain")
	if !ok {
		t.Fatal("expected fact for pkg.MyModule.plain")
	}
	if plain.Props["override"] == true || plain.Props["di_provider"] == true {
		t.Errorf("plain fun should carry neither override nor di_provider, got %+v", plain.Props)
	}
}

func TestAST_MethodFactIsClassQualified(t *testing.T) {
	ff := extractAST(t, `
package pkg
class Service {
    fun handle() {}
}
`, false)
	if _, ok := findFact(ff, "pkg.Service.handle"); !ok {
		t.Error("expected class-qualified method fact pkg.Service.handle")
	}
	if _, ok := findFact(ff, "pkg.handle"); ok {
		t.Error("method should NOT be named flat pkg.handle")
	}
	f, _ := findFact(ff, "pkg.Service.handle")
	if f.Props["receiver"] != "Service" {
		t.Errorf("receiver prop = %v, want Service", f.Props["receiver"])
	}
}

func TestAST_SameClassBareCall(t *testing.T) {
	ff := extractAST(t, `
package pkg
class Service {
    fun a() {
        b()
    }
    fun b() {}
}
`, false)
	a, ok := findFact(ff, "pkg.Service.a")
	if !ok {
		t.Fatal("expected fact for pkg.Service.a")
	}
	// Bare call b() inside class Service resolves to the qualified sibling method.
	if !hasRelation(a, facts.RelCalls, "pkg.Service.b") {
		t.Errorf("expected RelCalls → pkg.Service.b, got %+v", a.Relations)
	}
	if hasRelation(a, facts.RelCalls, "pkg.b") {
		t.Error("bare same-class call should not resolve to flat pkg.b")
	}
}

func TestAST_ThisMethodCall(t *testing.T) {
	ff := extractAST(t, `
package pkg
class Service {
    fun a() {
        this.b()
    }
    fun b() {}
}
`, false)
	a, ok := findFact(ff, "pkg.Service.a")
	if !ok {
		t.Fatal("expected fact for pkg.Service.a")
	}
	if !hasRelation(a, facts.RelCalls, "pkg.Service.b") {
		t.Errorf("expected RelCalls → pkg.Service.b for this.b(), got %+v", a.Relations)
	}
}

func TestAST_StdlibCalls_NoEdge(t *testing.T) {
	// Auto-imported Kotlin stdlib / scope functions must not produce dangling edges.
	ff := extractAST(t, `
package pkg
fun caller() {
    runCatching { }
    listOf(1, 2)
    println("hi")
    val x = 3.let { it + 1 }
    helper()
}
fun helper() {}
`, false)
	caller, ok := findFact(ff, "pkg.caller")
	if !ok {
		t.Fatal("expected fact for pkg.caller")
	}
	if !hasRelation(caller, facts.RelCalls, "pkg.helper") {
		t.Errorf("expected RelCalls → pkg.helper, got %+v", caller.Relations)
	}
	for _, name := range []string{"pkg.runCatching", "pkg.listOf", "pkg.println", "pkg.let"} {
		if hasRelation(caller, facts.RelCalls, name) {
			t.Errorf("stdlib call should not produce edge %q", name)
		}
	}
}

func TestAST_TopLevelFunctionCall_StillFlat(t *testing.T) {
	// Regression guard: a same-package top-level function call still resolves to
	// the flat "<dir>.fn" name, unaffected by class qualification.
	ff := extractAST(t, `
package pkg
class Service {
    fun a() {
        topLevelHelper()
    }
}
fun topLevelHelper() {}
`, false)
	a, ok := findFact(ff, "pkg.Service.a")
	if !ok {
		t.Fatal("expected fact for pkg.Service.a")
	}
	if !hasRelation(a, facts.RelCalls, "pkg.topLevelHelper") {
		t.Errorf("expected RelCalls → pkg.topLevelHelper, got %+v", a.Relations)
	}
}

// --- Additional parity coverage previously held by the deleted legacy tests ---

func TestAST_MultiLineConstructor(t *testing.T) {
	ff := extractAST(t, `
package pkg
class UserRepository(
    private val api: ApiService,
    private val db: UserDao
) : Repository {
}
`, true)
	f, ok := findFact(ff, "pkg.UserRepository")
	if !ok {
		t.Fatal("expected fact for pkg.UserRepository")
	}
	if !hasRelation(f, facts.RelImplements, "Repository") {
		t.Error("expected implements relation for Repository")
	}
	if f.Props["android_component"] != "repository" {
		t.Errorf("android_component = %v, want repository", f.Props["android_component"])
	}
}

func TestAST_NoRoomStorage_WithoutAndroid(t *testing.T) {
	ff := extractAST(t, `
package pkg
@Entity
class UserEntity {
}
`, false)
	if st := findFactsByKind(ff, facts.KindStorage); len(st) != 0 {
		t.Errorf("expected 0 storage facts when isAndroid=false, got %d", len(st))
	}
}

func TestAST_EnumClass(t *testing.T) {
	ff := extractAST(t, `
package pkg
enum class Direction {
    NORTH, SOUTH, EAST, WEST
}
`, false)
	f, ok := findFact(ff, "pkg.Direction")
	if !ok {
		t.Fatal("expected fact for pkg.Direction")
	}
	if f.Props["enum"] != true {
		t.Errorf("enum = %v, want true", f.Props["enum"])
	}
}

// Constructor calls reached via navigation expressions like `pkg.Foo()` should
// still surface the simple type name.
func TestAST_QualifiedConstructorCall(t *testing.T) {
	ff := extractAST(t, `
package pkg
class Wrapper {
    private val x = some.namespace.Bar()
}
`, false)
	f, ok := findFact(ff, "pkg.Wrapper")
	if !ok {
		t.Fatal("expected fact for pkg.Wrapper")
	}
	if !hasRelation(f, facts.RelInstantiates, "Bar") {
		t.Errorf("expected RelInstantiates → Bar (last segment of qualified callee), got %+v", f.Relations)
	}
}

// TestAST_QualifiedCallableReference covers the v55 qualified form (`Type::foo`),
// which credits the short-named callee just like the bare `::foo` form, so a method
// referenced only via a qualified reference isn't mis-reported as an orphan.
func TestAST_QualifiedCallableReference(t *testing.T) {
	ff := extractAST(t, `
package pkg
fun caller() {
    listOf("a").map(String::length)
    register(Mapper::convert)
}
fun convert() {}
`, false)
	c, ok := findFact(ff, "pkg.caller")
	if !ok {
		t.Fatal("expected fact for pkg.caller")
	}
	for _, target := range []string{"length", "convert"} {
		if !hasRelation(c, facts.RelCalls, target) {
			t.Errorf("expected short-name RelCalls → %s for a qualified callable reference, got %+v", target, c.Relations)
		}
	}
}

// TestAST_SealedClassIsAbstract covers v58's sealed→abstract marker: a sealed class
// is non-instantiable, so it must carry abstract=true so package-metrics abstractness
// (A) counts it.
func TestAST_SealedClassIsAbstract(t *testing.T) {
	ff := extractAST(t, `
package pkg
sealed class Result {
}
`, false)
	f, ok := findFact(ff, "pkg.Result")
	if !ok {
		t.Fatal("expected fact for pkg.Result")
	}
	if f.Props["abstract"] != true {
		t.Errorf("sealed class abstract = %v, want true", f.Props["abstract"])
	}
}
