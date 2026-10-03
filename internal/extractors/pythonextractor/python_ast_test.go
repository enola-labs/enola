package pythonextractor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// astExtract is a helper that writes src to a temp file and runs extractFileAST.
func astExtract(t *testing.T, filename, src string, isDjango bool) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, filename)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ff, _ := extractFileAST([]byte(src), filename, isDjango, false, false, nil)
	return ff
}

// astExtractFrameworks is astExtract with explicit Flask/FastAPI project hints,
// for the verb-shorthand framework-labeling tests.
func astExtractFrameworks(t *testing.T, filename, src string, isDjango, isFlask, isFastAPI bool) []facts.Fact {
	t.Helper()
	ff, _ := extractFileAST([]byte(src), filename, isDjango, isFlask, isFastAPI, nil)
	return ff
}

// astExtractWithIndex runs the index pass over all provided sources, then
// extracts facts from targetFile using that index.
func astExtractWithIndex(t *testing.T, files map[string]string, targetFile string, isDjango bool) []facts.Fact {
	t.Helper()
	idx := &pySymbolIndex{classes: make(map[string]*pyClassInfo), moduleDefs: make(map[string]map[string]bool)}
	for filename, src := range files {
		buildFileIndex([]byte(src), filename, idx)
	}
	finalizeImplMap(idx)
	src, ok := files[targetFile]
	if !ok {
		t.Fatalf("targetFile %q not in files map", targetFile)
	}
	ff, _ := extractFileAST([]byte(src), targetFile, isDjango, false, false, idx)
	return ff
}

// relsByKind returns all relations of a given kind from a fact.
func relsByKind(f facts.Fact, kind string) []string {
	var out []string
	for _, r := range f.Relations {
		if r.Kind == kind {
			out = append(out, r.Target)
		}
	}
	return out
}

// --- Call graph tests ---

func TestAST_SameModuleFunctionCall(t *testing.T) {
	src := `
def helper():
    pass

def main():
    helper()
`
	result := astExtract(t, "svc.py", src, false)
	idx := byName(result)

	mainFact, ok := idx["svc.main"]
	if !ok {
		t.Fatalf("missing svc.main; keys: %v", keys(idx))
	}
	calls := relsByKind(mainFact, facts.RelCalls)
	if len(calls) == 0 {
		t.Fatal("svc.main: expected RelCalls to svc.helper, got none")
	}
	found := false
	for _, c := range calls {
		if c == "svc.helper" {
			found = true
		}
	}
	if !found {
		t.Errorf("svc.main: RelCalls = %v, want svc.helper", calls)
	}
}

func TestAST_SelfMethodCall(t *testing.T) {
	src := `
class Service:
    def _do_work(self):
        pass

    def run(self):
        self._do_work()
`
	result := astExtract(t, "svc.py", src, false)
	idx := byName(result)

	runFact, ok := idx["svc.Service.run"]
	if !ok {
		t.Fatalf("missing svc.Service.run; keys: %v", keys(idx))
	}
	calls := relsByKind(runFact, facts.RelCalls)
	found := false
	for _, c := range calls {
		if c == "svc.Service._do_work" {
			found = true
		}
	}
	if !found {
		t.Errorf("Service.run: RelCalls = %v, want svc.Service._do_work", calls)
	}
}

func TestAST_Constructor_RelInstantiates(t *testing.T) {
	src := `
class Order:
    pass

def create():
    o = Order()
    return o
`
	result := astExtract(t, "models.py", src, false)
	idx := byName(result)

	createFact, ok := idx["models.create"]
	if !ok {
		t.Fatalf("missing models.create; keys: %v", keys(idx))
	}
	insts := relsByKind(createFact, facts.RelInstantiates)
	found := false
	for _, i := range insts {
		if i == "Order" {
			found = true
		}
	}
	if !found {
		t.Errorf("create: RelInstantiates = %v, want Order", insts)
	}
}

func TestAST_NoEdgeForBuiltins(t *testing.T) {
	src := `
def process(items):
    result = list(map(str, items))
    print(len(result))
    return sorted(result)
`
	result := astExtract(t, "util.py", src, false)
	idx := byName(result)

	fn, ok := idx["util.process"]
	if !ok {
		t.Fatalf("missing util.process")
	}
	calls := relsByKind(fn, facts.RelCalls)
	for _, c := range calls {
		if c == "util.list" || c == "util.print" || c == "util.sorted" || c == "util.map" || c == "util.str" || c == "util.len" {
			t.Errorf("process: should not emit call edge to builtin, got %q", c)
		}
	}
}

func TestAST_ReturnType_FromAST(t *testing.T) {
	// tree-sitter reads the return type node directly — no regex needed.
	src := `
def get_user(user_id: int) -> Optional[str]:
    pass

def create_order(
    items: list,
    total: float,
) -> dict[str, Any]:
    pass
`
	result := astExtract(t, "api.py", src, false)
	idx := byName(result)

	cases := []struct{ name, want string }{
		{"api.get_user", "Optional[str]"},
		{"api.create_order", "dict[str, Any]"},
	}
	for _, tc := range cases {
		fn, ok := idx[tc.name]
		if !ok {
			t.Fatalf("missing %q; keys: %v", tc.name, keys(idx))
		}
		if fn.Props["return_type"] != tc.want {
			t.Errorf("%s: return_type = %v, want %q", tc.name, fn.Props["return_type"], tc.want)
		}
	}
}

func TestAST_NestedClass(t *testing.T) {
	src := `
class Outer:
    class Inner:
        def method(self):
            pass
`
	result := astExtract(t, "nested.py", src, false)
	idx := byName(result)

	if _, ok := idx["nested.Outer"]; !ok {
		t.Errorf("missing nested.Outer; keys: %v", keys(idx))
	}
	if _, ok := idx["nested.Outer.Inner"]; !ok {
		t.Errorf("missing nested.Outer.Inner; keys: %v", keys(idx))
	}
	if _, ok := idx["nested.Outer.Inner.method"]; !ok {
		t.Errorf("missing nested.Outer.Inner.method; keys: %v", keys(idx))
	}
}

func TestAST_AsyncFunction(t *testing.T) {
	src := `
async def fetch_data(url: str) -> bytes:
    pass
`
	result := astExtract(t, "client.py", src, false)
	idx := byName(result)

	fn, ok := idx["client.fetch_data"]
	if !ok {
		t.Fatalf("missing client.fetch_data")
	}
	if fn.Props["async"] != true {
		t.Errorf("fetch_data: async = %v, want true", fn.Props["async"])
	}
	if fn.Props["return_type"] != "bytes" {
		t.Errorf("fetch_data: return_type = %v, want bytes", fn.Props["return_type"])
	}
}

func TestAST_DecoratorProps(t *testing.T) {
	src := `
class Repo:
    @staticmethod
    def from_dict(d):
        pass

    @classmethod
    def create(cls):
        pass

    @property
    def name(self):
        return self._name
`
	result := astExtract(t, "repo.py", src, false)
	idx := byName(result)

	cases := []struct {
		name string
		prop string
		want any
	}{
		{"repo.Repo.from_dict", "static", true},
		{"repo.Repo.create", "class_method", true},
		{"repo.Repo.name", "property", true},
	}
	for _, tc := range cases {
		fn, ok := idx[tc.name]
		if !ok {
			t.Fatalf("missing %q; keys: %v", tc.name, keys(idx))
		}
		if fn.Props[tc.prop] != tc.want {
			t.Errorf("%s: %s = %v, want %v", tc.name, tc.prop, fn.Props[tc.prop], tc.want)
		}
	}
}

// TestAST_InitReexports verifies that from-imports in __init__.py record the
// imported short names in the dependency fact's "reexports" prop, and that
// non-__init__ files do not.
func TestAST_InitReexports(t *testing.T) {
	src := "from .sub import PublicThing, OtherThing\n"

	initFacts := astExtract(t, "pkg/__init__.py", src, false)
	var dep *facts.Fact
	for i := range initFacts {
		if initFacts[i].Kind == facts.KindDependency {
			dep = &initFacts[i]
			break
		}
	}
	if dep == nil {
		t.Fatal("no dependency fact emitted for from-import")
	}
	names, ok := dep.Props["reexports"].([]string)
	if !ok {
		t.Fatalf("reexports prop missing or wrong type: %#v", dep.Props["reexports"])
	}
	want := map[string]bool{"PublicThing": true, "OtherThing": true}
	if len(names) != 2 || !want[names[0]] || !want[names[1]] {
		t.Errorf("reexports = %v, want PublicThing & OtherThing", names)
	}

	// A non-__init__ file must NOT record reexports.
	modFacts := astExtract(t, "pkg/mod.py", src, false)
	for _, f := range modFacts {
		if f.Kind == facts.KindDependency {
			if _, ok := f.Props["reexports"]; ok {
				t.Errorf("non-__init__ file should not record reexports, got %v", f.Props["reexports"])
			}
		}
	}
}

func TestAST_AbstractClassDetection(t *testing.T) {
	src := `
from abc import ABC, ABCMeta, abstractmethod
from typing import Protocol

class FromABC(ABC):
    pass

class FromProtocol(Protocol):
    def run(self): ...

class FromMeta(metaclass=ABCMeta):
    pass

class HasAbstractMethod:
    @abstractmethod
    def do(self):
        ...

class Concrete:
    def do(self):
        return 1
`
	idx := byName(astExtract(t, "svc.py", src, false))

	for _, name := range []string{"svc.FromABC", "svc.FromProtocol", "svc.FromMeta", "svc.HasAbstractMethod"} {
		fn, ok := idx[name]
		if !ok {
			t.Fatalf("missing %q; keys: %v", name, keys(idx))
		}
		if fn.Props["abstract"] != true {
			t.Errorf("%s: abstract = %v, want true", name, fn.Props["abstract"])
		}
	}
	if c := idx["svc.Concrete"]; c.Props["abstract"] == true {
		t.Error("svc.Concrete should not be abstract")
	}
}

// TestAST_DataClassAndEnumProps verifies enum classes and DTO/schema classes get
// the structural props package-metrics relies on: `enum` (excluded from N like
// Kotlin enums) and `data_class` (a value carrier, not a "rigid" abstraction).
func TestAST_DataClassAndEnumProps(t *testing.T) {
	src := `
from dataclasses import dataclass
from enum import Enum, IntEnum
from typing import NamedTuple, TypedDict
from pydantic import BaseModel, RootModel
import attrs

class Color(Enum):
    RED = 1

class Level(IntEnum):
    LOW = 1

@dataclass
class Point:
    x: int
    y: int

@attrs.define
class Box:
    w: int

class UserModel(BaseModel):
    name: str

# Project-local Pydantic base (StrictBaseModel(BaseModel)): matched by the
# "*BaseModel" suffix rule even though BaseModel isn't a direct base here.
class VariableResponse(StrictBaseModel):
    key: str

class XComSlice(RootModel):
    root: list

class Pair(NamedTuple):
    a: int
    b: int

class Config(TypedDict):
    debug: bool

class Plain:
    def do(self):
        return 1
`
	idx := byName(astExtract(t, "svc.py", src, false))

	for _, name := range []string{"svc.Color", "svc.Level"} {
		if idx[name].Props["enum"] != true {
			t.Errorf("%s: enum = %v, want true", name, idx[name].Props["enum"])
		}
	}
	for _, name := range []string{"svc.Point", "svc.Box", "svc.UserModel", "svc.VariableResponse", "svc.XComSlice", "svc.Pair", "svc.Config"} {
		if idx[name].Props["data_class"] != true {
			t.Errorf("%s: data_class = %v, want true", name, idx[name].Props["data_class"])
		}
	}
	if p := idx["svc.Plain"]; p.Props["data_class"] == true || p.Props["enum"] == true {
		t.Errorf("svc.Plain should be a plain class (no enum/data_class props): %v", p.Props)
	}
}

// TestAST_InformalAbstractDetection verifies the idiomatic duck-typed abstract
// pattern — a method whose whole body is `raise NotImplementedError` (optionally
// after a docstring) — marks a class abstract, while conservative bare `...`/`pass`
// stub bodies do NOT.
func TestAST_InformalAbstractDetection(t *testing.T) {
	src := `
class BaseOperator:
    """Base."""
    def execute(self, context):
        raise NotImplementedError()

class BaseHook:
    def get_conn(self):
        """Return a connection."""
        raise NotImplementedError

class StubOnly:
    def maybe(self):
        ...

class Concrete:
    def execute(self, context):
        return 1
`
	idx := byName(astExtract(t, "svc.py", src, false))

	for _, name := range []string{"svc.BaseOperator", "svc.BaseHook"} {
		if idx[name].Props["abstract"] != true {
			t.Errorf("%s: abstract = %v, want true (raise NotImplementedError)", name, idx[name].Props["abstract"])
		}
	}
	if idx["svc.StubOnly"].Props["abstract"] == true {
		t.Error("svc.StubOnly (bare ... body) should NOT be abstract")
	}
	if idx["svc.Concrete"].Props["abstract"] == true {
		t.Error("svc.Concrete should not be abstract")
	}
}

// TestAST_ExportedProp verifies the leading-underscore export convention.
func TestAST_ExportedProp(t *testing.T) {
	src := `
class PublicClass:
    pass

class _PrivateClass:
    pass

def public_fn():
    pass

def _private_fn():
    pass
`
	idx := byName(astExtract(t, "svc.py", src, false))
	cases := map[string]bool{
		"svc.PublicClass":   true,
		"svc._PrivateClass": false,
		"svc.public_fn":     true,
		"svc._private_fn":   false,
	}
	for name, want := range cases {
		fn, ok := idx[name]
		if !ok {
			t.Fatalf("missing %q; keys: %v", name, keys(idx))
		}
		if fn.Props["exported"] != want {
			t.Errorf("%s: exported = %v, want %v", name, fn.Props["exported"], want)
		}
	}
}

func TestAST_SQLAlchemyTable(t *testing.T) {
	src := `
from sqlalchemy import Column, Integer, String
from sqlalchemy.orm import DeclarativeBase

class Base(DeclarativeBase):
    pass

class Product(Base):
    __tablename__ = "products"
    id = Column(Integer, primary_key=True)
    name = Column(String)
`
	result := astExtract(t, "models.py", src, false)
	storages := factsByKind(result, facts.KindStorage)
	if len(storages) != 1 {
		t.Fatalf("expected 1 storage fact, got %d: %v", len(storages), storages)
	}
	if storages[0].Name != "products" {
		t.Errorf("storage name = %q, want products", storages[0].Name)
	}
	if storages[0].Props["framework"] != "sqlalchemy" {
		t.Errorf("storage framework = %v, want sqlalchemy", storages[0].Props["framework"])
	}
}

func TestAST_ImportEdges(t *testing.T) {
	src := `
import os
from pathlib import Path
from . import utils
`
	result := astExtract(t, "mymod.py", src, false)
	deps := factsByKind(result, facts.KindDependency)
	if len(deps) < 3 {
		t.Errorf("expected >= 3 dependency facts, got %d", len(deps))
	}
	// Each dep must carry a RelImports relation.
	for _, d := range deps {
		found := false
		for _, r := range d.Relations {
			if r.Kind == facts.RelImports {
				found = true
			}
		}
		if !found {
			t.Errorf("dependency %q missing RelImports relation", d.Name)
		}
	}
}

// --- Symbol resolution tests ---

func TestAST_TypedParam_AttributeCall(t *testing.T) {
	src := `
from .queue import Queue

def consume(q: Queue):
    q.pop()
`
	result := astExtract(t, "app/worker.py", src, false)
	idx := byName(result)

	fn, ok := idx["app/worker.consume"]
	if !ok {
		t.Fatalf("missing app/worker.consume; keys: %v", keys(idx))
	}
	calls := relsByKind(fn, facts.RelCalls)
	found := false
	for _, c := range calls {
		if c == "app/queue.Queue.pop" {
			found = true
		}
	}
	if !found {
		t.Errorf("consume: RelCalls = %v, want app/queue.Queue.pop", calls)
	}
}

func TestAST_ConstructorAssignment_AttributeCall(t *testing.T) {
	src := `
from .repo import UserRepo

def handle():
    repo = UserRepo()
    repo.find(1)
`
	result := astExtract(t, "app/handler.py", src, false)
	idx := byName(result)

	fn, ok := idx["app/handler.handle"]
	if !ok {
		t.Fatalf("missing app/handler.handle; keys: %v", keys(idx))
	}
	calls := relsByKind(fn, facts.RelCalls)
	found := false
	for _, c := range calls {
		if c == "app/repo.UserRepo.find" {
			found = true
		}
	}
	if !found {
		t.Errorf("handle: RelCalls = %v, want app/repo.UserRepo.find", calls)
	}
}

func TestAST_AnnotatedAssignment_AttributeCall(t *testing.T) {
	src := `
from .logger import FileLogger

def process():
    log: FileLogger = FileLogger()
    log.write("done")
`
	result := astExtract(t, "app/svc.py", src, false)
	idx := byName(result)

	fn, ok := idx["app/svc.process"]
	if !ok {
		t.Fatalf("missing app/svc.process; keys: %v", keys(idx))
	}
	calls := relsByKind(fn, facts.RelCalls)
	found := false
	for _, c := range calls {
		if c == "app/logger.FileLogger.write" {
			found = true
		}
	}
	if !found {
		t.Errorf("process: RelCalls = %v, want app/logger.FileLogger.write", calls)
	}
}

func TestAST_NoPhantomEdge_UnresolvableReceiver(t *testing.T) {
	src := `
def handle(req):
    req.method()
`
	result := astExtract(t, "app/h.py", src, false)
	idx := byName(result)

	fn, ok := idx["app/h.handle"]
	if !ok {
		t.Fatalf("missing app/h.handle")
	}
	calls := relsByKind(fn, facts.RelCalls)
	for _, c := range calls {
		if strings.Contains(c, ".method") {
			t.Errorf("handle: unexpected call edge to unresolvable receiver: %q", c)
		}
	}
}

func TestAST_AbstractClass_ConcreteImplementorCalls(t *testing.T) {
	files := map[string]string{
		"app/interfaces.py": `
from abc import ABC, abstractmethod

class Logger(ABC):
    @abstractmethod
    def write(self, msg):
        pass
`,
		"app/loggers.py": `
from .interfaces import Logger

class FileLogger(Logger):
    def write(self, msg):
        pass
`,
		"app/service.py": `
from .interfaces import Logger

class OrderService:
    def process(self, logger: Logger):
        logger.write("order processed")
`,
	}

	result := astExtractWithIndex(t, files, "app/service.py", false)
	idx := byName(result)

	fn, ok := idx["app/service.OrderService.process"]
	if !ok {
		t.Fatalf("missing app/service.OrderService.process; keys: %v", keys(idx))
	}
	calls := relsByKind(fn, facts.RelCalls)

	wantDirect := "app/interfaces.Logger.write"
	wantConcrete := "app/loggers.FileLogger.write"
	foundDirect, foundConcrete := false, false
	for _, c := range calls {
		if c == wantDirect {
			foundDirect = true
		}
		if c == wantConcrete {
			foundConcrete = true
		}
	}
	if !foundDirect {
		t.Errorf("process: missing RelCalls to abstract %q; got %v", wantDirect, calls)
	}
	if !foundConcrete {
		t.Errorf("process: missing RelCalls to concrete %q; got %v", wantConcrete, calls)
	}
}

func TestAST_IsAbstract_Detection(t *testing.T) {
	src := `
from abc import ABC, abstractmethod

class MyInterface(ABC):
    @abstractmethod
    def execute(self):
        pass

class ConcreteImpl(MyInterface):
    def execute(self):
        pass
`
	idx := &pySymbolIndex{classes: make(map[string]*pyClassInfo)}
	buildFileIndex([]byte(src), "app/types.py", idx)

	iface, ok := idx.classes["app/types.MyInterface"]
	if !ok {
		t.Fatal("missing app/types.MyInterface in index")
	}
	if !iface.isAbstract {
		t.Error("MyInterface: expected isAbstract=true")
	}

	impl, ok := idx.classes["app/types.ConcreteImpl"]
	if !ok {
		t.Fatal("missing app/types.ConcreteImpl in index")
	}
	if impl.isAbstract {
		t.Error("ConcreteImpl: expected isAbstract=false")
	}
}

// firstOfKind returns the first fact of the given kind, or a zero fact.
func firstOfKind(ff []facts.Fact, kind string) (facts.Fact, bool) {
	for _, f := range ff {
		if f.Kind == kind {
			return f, true
		}
	}
	return facts.Fact{}, false
}

// --- Absolute-import call-edge emission (pre-resolution dotted targets) ---

func TestAST_AbsoluteImportEmitsCallEdge(t *testing.T) {
	src := `
from airflow.api.common.airflow_health import get_airflow_health

def get_health():
    return get_airflow_health()
`
	result := astExtract(t, "monitor.py", src, false)
	idx := byName(result)
	fn, ok := idx["monitor.get_health"]
	if !ok {
		t.Fatalf("missing monitor.get_health; keys: %v", keys(idx))
	}
	calls := relsByKind(fn, facts.RelCalls)
	want := "airflow.api.common.airflow_health.get_airflow_health"
	found := false
	for _, c := range calls {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("get_health: RelCalls = %v, want a call to %q (absolute import must emit an edge)", calls, want)
	}
}

func TestAST_TryExceptImportRegisters(t *testing.T) {
	// Module-level imports guarded by try/except ImportError (the
	// package-or-script dual-import idiom) must register like unguarded ones —
	// previously walkStatement had no try_statement case, so every call through
	// the guarded names was unresolvable and the callee read as dead (a Python MCP
	// server's whole server-utils surface). The except-branch fallback must NOT clobber the
	// try-branch binding: the relative form resolves to a real slash path at walk
	// time, while the bare fallback resolves to nothing and its edge would be
	// dropped by resolveCallTargets.
	src := `
try:
    from .server_utils import format_search_results
except ImportError:
    from server_utils import format_search_results

async def search(q):
    return format_search_results(q)
`
	relFile := "pkg/server.py"
	result := astExtract(t, relFile, src, false)
	idx := byName(result)
	if _, ok := idx[mod(relFile)+" -> .server_utils"]; !ok {
		t.Errorf("missing dependency fact for the try-branch relative import; keys: %v", keys(idx))
	}
	fn, ok := idx[mod(relFile)+".search"]
	if !ok {
		t.Fatalf("missing %s.search; keys: %v", mod(relFile), keys(idx))
	}
	calls := relsByKind(fn, facts.RelCalls)
	want := "pkg/server_utils.format_search_results"
	found := false
	for _, c := range calls {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("search: RelCalls = %v, want %q (try-branch relative binding must win over the bare except fallback)", calls, want)
	}
}

func TestAST_MainGuardImportRegisters(t *testing.T) {
	// A module-level `if __name__ == "__main__":` block is module scope at
	// runtime: its imports must register so the calls in the block (already
	// collected by walkTopLevelCalls) resolve to edges.
	src := `
if __name__ == "__main__":
    from airflow.jobs.runner import run_job

    run_job()
`
	result := astExtract(t, "main.py", src, false)
	fr, ok := firstOfKind(result, facts.KindFileRef)
	if !ok {
		t.Fatal("expected a KindFileRef fact for the main-guard run_job() call, got none")
	}
	calls := relsByKind(fr, facts.RelCalls)
	want := "airflow.jobs.runner.run_job"
	found := false
	for _, c := range calls {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("file_ref: RelCalls = %v, want %q (main-guard import must register)", calls, want)
	}
}

func TestAST_GuardedDefsNotEmittedAsSymbols(t *testing.T) {
	// A def/class guarded by a conditional (try/except ImportError, platform
	// if/else, if TYPE_CHECKING) is almost always an intentional shim whose name
	// is bound by a sibling branch — emitting a symbol for it only manufactures a
	// dead-code false positive (airflow's setproctitle macOS fallback and
	// lru_cache TYPE_CHECKING stubs were the corpus cases). We deliberately do
	// NOT emit symbols for them; only the guarded imports register.
	src := `
try:
    from functools import lru_cache
except ImportError:
    def lru_cache(maxsize=128):
        def wrap(f):
            return f
        return wrap

if TYPE_CHECKING:
    class _StubOnly:
        pass
`
	result := astExtractIdx(t, "shim.py", src)
	idx := byName(result)
	if _, ok := idx["shim.lru_cache"]; ok {
		t.Errorf("shim.lru_cache should NOT be emitted (except-branch fallback shim); keys: %v", keys(idx))
	}
	if _, ok := idx["shim._StubOnly"]; ok {
		t.Errorf("shim._StubOnly should NOT be emitted (TYPE_CHECKING typing stub); keys: %v", keys(idx))
	}
	// but the guarded import must still register as a dependency
	if _, ok := idx["shim -> functools"]; !ok {
		t.Errorf("missing dependency fact for the guarded functools import; keys: %v", keys(idx))
	}
}

func TestAST_ModuleAssignmentRHSValueRef(t *testing.T) {
	// A module-level assignment whose RHS is a bare module-def name is a use of
	// that def (the click monkeypatch idiom: `click.echo = echo_to_stderr`).
	// Without folding the RHS value-ref, surfacing the guarded def (v119) would
	// flag a live, installed function as dead — a new false positive.
	src := `
def echo_to_stderr(*a, **k):
    pass

import click
click.echo = echo_to_stderr
`
	result := astExtractIdx(t, "installer.py", src)
	fr, ok := firstOfKind(result, facts.KindFileRef)
	if !ok {
		t.Fatal("expected a KindFileRef fact for the module-level assignment RHS, got none")
	}
	calls := relsByKind(fr, facts.RelCalls)
	want := "installer.echo_to_stderr"
	found := false
	for _, c := range calls {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("file_ref: RelCalls = %v, want a value-ref to %q (assignment RHS must fold)", calls, want)
	}
}

func TestAST_TypeCheckingImportEmitsDependency(t *testing.T) {
	// `if TYPE_CHECKING:` imports are real (type-level) coupling and must emit a
	// dependency fact like any other module-level import.
	src := `
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from airflow.models.dag import DAG

def make(d: "DAG"):
    pass
`
	result := astExtract(t, "factory.py", src, false)
	idx := byName(result)
	if _, ok := idx["factory -> airflow.models.dag"]; !ok {
		t.Errorf("missing dependency fact for the TYPE_CHECKING-guarded import; keys: %v", keys(idx))
	}
}

func TestAST_TopLevelCallEmitsFileRef(t *testing.T) {
	src := `
from airflow.api_fastapi.app import cached_app

app = cached_app(apps="all")
`
	result := astExtract(t, "main.py", src, false)
	fr, ok := firstOfKind(result, facts.KindFileRef)
	if !ok {
		t.Fatal("expected a KindFileRef fact for the module-level cached_app() call, got none")
	}
	calls := relsByKind(fr, facts.RelCalls)
	want := "airflow.api_fastapi.app.cached_app"
	found := false
	for _, c := range calls {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("file_ref: RelCalls = %v, want %q", calls, want)
	}
}

func TestAST_DecoratorEmitsRef(t *testing.T) {
	src := `
from airflow.utils.session import provide_session

@provide_session
def do_work(session=None):
    pass
`
	result := astExtract(t, "svc.py", src, false)
	fr, ok := firstOfKind(result, facts.KindFileRef)
	if !ok {
		t.Fatal("expected a KindFileRef fact for the @provide_session decorator, got none")
	}
	calls := relsByKind(fr, facts.RelCalls)
	want := "airflow.utils.session.provide_session"
	found := false
	for _, c := range calls {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("file_ref: RelCalls = %v, want a decorator use of %q", calls, want)
	}
}

// --- Pass 2: lazy imports, value-refs, param defaults, decorator args ---

func TestAST_LazyImportInsideFunctionResolves(t *testing.T) {
	src := `
def load():
    from airflow import plugins_manager
    return plugins_manager.get_priority_weight_strategy_plugins()
`
	result := astExtract(t, "svc.py", src, false)
	fn := byName(result)["svc.load"]
	calls := relsByKind(fn, facts.RelCalls)
	want := "airflow.plugins_manager.get_priority_weight_strategy_plugins"
	found := false
	for _, c := range calls {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("load: RelCalls = %v, want a call to %q via the function-local import", calls, want)
	}
}

func TestAST_CallArgumentValueRefEmitsEdge(t *testing.T) {
	src := `
from airflow.auth.utils import parse_login_body

def register():
    return Depends(parse_login_body)
`
	result := astExtract(t, "svc.py", src, false)
	fn := byName(result)["svc.register"]
	calls := relsByKind(fn, facts.RelCalls)
	want := "airflow.auth.utils.parse_login_body"
	found := false
	for _, c := range calls {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("register: RelCalls = %v, want a value-ref edge to %q", calls, want)
	}
}

func TestAST_ParameterDefaultCallEmitsEdge(t *testing.T) {
	src := `
from airflow.auth.utils import parse_login_body

def handler(body = Depends(parse_login_body)):
    pass
`
	result := astExtract(t, "svc.py", src, false)
	fn := byName(result)["svc.handler"]
	calls := relsByKind(fn, facts.RelCalls)
	want := "airflow.auth.utils.parse_login_body"
	found := false
	for _, c := range calls {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("handler: RelCalls (incl. param defaults) = %v, want %q", calls, want)
	}
}

func TestAST_ParameterDefaultFunctionValueEmitsEdge(t *testing.T) {
	src := `
def list_commits_page(anchor, page):
    return []

def extend_commits(anchor, list_page=list_commits_page):
    return list_page(anchor, 1)
`
	result := astExtractIdx(t, "history.py", src)
	fn := byName(result)["history.extend_commits"]
	want := "history.list_commits_page"
	if !hasCallTo(fn, want) {
		t.Errorf("extend_commits: RelCalls = %v, want default function-value ref to %q", relsByKind(fn, facts.RelCalls), want)
	}
}

func TestAST_DecoratorArgumentCallEmitsFileRef(t *testing.T) {
	src := `
from airflow.security import requires_access_asset

@router.get("/x", dependencies=[Depends(requires_access_asset(method="GET"))])
def endpoint():
    pass
`
	result := astExtract(t, "routes.py", src, false)
	fr, ok := firstOfKind(result, facts.KindFileRef)
	if !ok {
		t.Fatal("expected a KindFileRef fact for the decorator-argument call, got none")
	}
	calls := relsByKind(fr, facts.RelCalls)
	want := "airflow.security.requires_access_asset"
	found := false
	for _, c := range calls {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("file_ref: RelCalls = %v, want the decorator-arg factory call %q", calls, want)
	}
}

func TestAST_ClickCommandTagged(t *testing.T) {
	src := `
import click

@click.command()
def build():
    pass

@click.group()
def cli():
    pass

def plain():
    pass
`
	result := astExtract(t, "commands.py", src, false)
	idx := byName(result)
	for _, name := range []string{"commands.build", "commands.cli"} {
		fn, ok := idx[name]
		if !ok {
			t.Fatalf("missing %q; keys: %v", name, keys(idx))
		}
		if fn.Props["cli_command"] != true {
			t.Errorf("%s: cli_command = %v, want true", name, fn.Props["cli_command"])
		}
	}
	if idx["commands.plain"].Props["cli_command"] == true {
		t.Error("commands.plain: cli_command should be unset for a non-click function")
	}
}

func TestAST_DottedStringLiteralEmitsRef(t *testing.T) {
	src := `
def register():
    return lazy_load_command("airflow.cli.commands.asset_command.asset_list")

MESSAGE = "just a plain message"
TWO = "airflow.models"
`
	result := astExtract(t, "cli_config.py", src, false)
	// The dotted 4-segment string inside register() is a reference edge on the owner.
	fn := byName(result)["cli_config.register"]
	calls := relsByKind(fn, facts.RelCalls)
	want := "airflow.cli.commands.asset_command.asset_list"
	found := false
	for _, c := range calls {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("register: RelCalls = %v, want a dotted-string ref to %q", calls, want)
	}
	// The plain message and the 2-segment string must not produce edges anywhere.
	for _, f := range result {
		for _, r := range f.Relations {
			if r.Target == "just a plain message" || r.Target == "airflow.models" {
				t.Errorf("unexpected edge from non-qualifying string: %q", r.Target)
			}
		}
	}
}

// --- Pass 4: class-body wiring, same-module value-refs, route-handler tag ---

// astExtractIdx runs the index pass over src, then extracts with that index so
// moduleDefs-based same-module value-ref resolution is exercised.
func astExtractIdx(t *testing.T, filename, src string) []facts.Fact {
	t.Helper()
	idx := &pySymbolIndex{classes: make(map[string]*pyClassInfo), moduleDefs: make(map[string]map[string]bool)}
	buildFileIndex([]byte(src), filename, idx)
	finalizeImplMap(idx)
	ff, _ := extractFileAST([]byte(src), filename, false, false, false, idx)
	return ff
}

func TestAST_ClassBodyCallAndValueRefEmitEdges(t *testing.T) {
	src := `
def _conf_list_factory(section, key):
    return None

def _generate_kid(self):
    return "kid"

class Signer:
    algo = _conf_list_factory("api_auth", "jwt_algorithm")
    kid = attrs.field(default=attrs.Factory(_generate_kid, takes_self=True))
`
	result := astExtractIdx(t, "tokens.py", src)
	cls := byName(result)["tokens.Signer"]
	calls := relsByKind(cls, facts.RelCalls)
	for _, want := range []string{"tokens._conf_list_factory", "tokens._generate_kid"} {
		found := false
		for _, c := range calls {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Signer class body: RelCalls = %v, want an edge to %q", calls, want)
		}
	}
}

func TestAST_SameModuleValueRef(t *testing.T) {
	src := `
def custom_show_warning(message):
    pass

def replace_showwarning(fn):
    pass

original = replace_showwarning(custom_show_warning)
`
	result := astExtractIdx(t, "settings.py", src)
	// The module-level call's arg (a same-module function) must be credited.
	var all []string
	for _, f := range result {
		all = append(all, relsByKind(f, facts.RelCalls)...)
	}
	found := false
	for _, c := range all {
		if c == "settings.custom_show_warning" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a same-module value-ref edge to settings.custom_show_warning; got %v", all)
	}
}

func TestAST_ParamPassedAsValue_NoFalseEdge(t *testing.T) {
	// Regression guard: a parameter/local passed by value must NOT be credited as a
	// same-module symbol, even if a same-named top-level def exists.
	src := `
def user_id():
    return 1

def get_user(user_id):
    return lookup(user_id)
`
	result := astExtractIdx(t, "svc.py", src)
	fn := byName(result)["svc.get_user"]
	for _, c := range relsByKind(fn, facts.RelCalls) {
		if c == "svc.user_id" {
			t.Errorf("param user_id was wrongly credited as a ref to the same-named function svc.user_id")
		}
	}
}

func TestAST_ComputedPathRouteHandlerTagged(t *testing.T) {
	src := `
@task_instances_router.get(
    task_instances_prefix + "/{task_id}/listMapped",
    dependencies=[Depends(requires_access_dag(method="GET"))],
)
def get_mapped_task_instances(dag_id):
    pass
`
	result := astExtractIdx(t, "routes/task_instances.py", src)
	fn := byName(result)["routes/task_instances.get_mapped_task_instances"]
	if fn.Props["web_component"] != "route_handler" {
		t.Errorf("computed-path handler: web_component = %v, want route_handler", fn.Props["web_component"])
	}
}

// --- Pass 5: registration decorators, attribute/collection value-refs ---

func TestAST_RegistrationDecoratorsMarkUsed(t *testing.T) {
	src := `
@compiles(JSONExtract, "postgresql")
def compile_postgres(element, compiler, **kw):
    pass

@handle_event_submit.register
def _(event, **kw):
    pass

@worker_ready.connect
def on_worker_ready(*args, **kwargs):
    pass

@event.listens_for(User.__table__, "after_create")
def _restore_idx(table, conn, **kw):
    pass
`
	result := astExtractIdx(t, "reg.py", src)
	fr, ok := firstOfKind(result, facts.KindFileRef)
	if !ok {
		t.Fatal("expected a KindFileRef fact for registration-decorated functions")
	}
	targets := map[string]bool{}
	for _, r := range relsByKind(fr, facts.RelCalls) {
		targets[r] = true
	}
	for _, want := range []string{"reg.compile_postgres", "reg._", "reg.on_worker_ready", "reg._restore_idx"} {
		if !targets[want] {
			t.Errorf("missing registration self-edge to %q; got %v", want, targets)
		}
	}
}

func TestAST_MCPDecoratorsMarkUsed(t *testing.T) {
	// MCP server handlers (FastMCP @mcp.tool/@mcp.resource/@mcp.prompt/
	// @mcp.custom_route, and bare re-exported wrappers like @tool/@prompt) are
	// registered with the server at import time and dispatched by the framework —
	// they have no in-code caller by construction. Covers single-line, multi-line,
	// and bare decorator forms. The @log_usage-only function is the negative case:
	// a wrapper decorator alone registers nothing, so the function must NOT get a
	// self-edge (it is genuinely dead until an @mcp.tool is added).
	src := `
@mcp.tool()
async def list_datasets_json():
    pass

@mcp.tool(
    name="cognify_file",
    description="Turn a file into a knowledge graph",
)
@log_usage(function_name="MCP cognify_file", log_type="mcp_tool")
async def cognify_file(path: str):
    pass

@mcp.resource("instance://metadata")
def get_instance_metadata_resource() -> str:
    pass

@mcp.custom_route("/health", methods=["GET"])
async def health_check(request):
    pass

@prompt("quickstart")
async def quickstart_prompt(user_type: str = "analyst") -> str:
    pass

@tool(name="list_users", tags=["extension"])
def list_users():
    pass

@log_usage(function_name="MCP save_interaction", log_type="mcp_tool")
async def save_interaction(data: str) -> list:
    pass
`
	result := astExtractIdx(t, "mcp_server.py", src)
	fr, ok := firstOfKind(result, facts.KindFileRef)
	if !ok {
		t.Fatal("expected a KindFileRef fact for MCP-decorated functions")
	}
	targets := map[string]bool{}
	for _, r := range relsByKind(fr, facts.RelCalls) {
		targets[r] = true
	}
	for _, want := range []string{
		"mcp_server.list_datasets_json",
		"mcp_server.cognify_file",
		"mcp_server.get_instance_metadata_resource",
		"mcp_server.health_check",
		"mcp_server.quickstart_prompt",
		"mcp_server.list_users",
	} {
		if !targets[want] {
			t.Errorf("missing MCP registration self-edge to %q; got %v", want, targets)
		}
	}
	if targets["mcp_server.save_interaction"] {
		t.Errorf("@log_usage-only function save_interaction must NOT get a registration self-edge (it is unregistered dead code); got %v", targets)
	}
}

func TestAST_AttributeArgValueRef(t *testing.T) {
	src := `
from airflow.providers.fab.www import views

def init_app(app):
    app.register_error_handler(404, views.not_found)
`
	result := astExtractIdx(t, "init_views.py", src)
	fn := byName(result)["init_views.init_app"]
	calls := relsByKind(fn, facts.RelCalls)
	want := "airflow.providers.fab.www.views.not_found"
	found := false
	for _, c := range calls {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Errorf("init_app: RelCalls = %v, want attribute value-ref to %q", calls, want)
	}
}

func TestAST_DictValueRef(t *testing.T) {
	src := `
def ds_filter(v):
    return v

def ts_filter(v):
    return v

FILTERS = {
    "ds": ds_filter,
    "ts": ts_filter,
}
`
	result := astExtractIdx(t, "templater.py", src)
	var all []string
	for _, f := range result {
		all = append(all, relsByKind(f, facts.RelCalls)...)
	}
	for _, want := range []string{"templater.ds_filter", "templater.ts_filter"} {
		found := false
		for _, c := range all {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected dict-value ref to %q; got %v", want, all)
		}
	}
}

func TestAST_ListOfLocalsNoFalseRef(t *testing.T) {
	// A list of local variables must not produce edges (valueRefTarget resolves only
	// imported / same-module def / same-class names).
	src := `
def build(a, b):
    items = [a, b]
    return items
`
	result := astExtractIdx(t, "svc.py", src)
	fn := byName(result)["svc.build"]
	for _, c := range relsByKind(fn, facts.RelCalls) {
		if c == "svc.a" || c == "svc.b" {
			t.Errorf("local var wrongly credited as a ref: %q", c)
		}
	}
}

// --- resolveCall must not fabricate edges for params/locals/loop vars (bug 05) ---

// hasCallTo reports whether fact f has a RelCalls edge to target.
func hasCallTo(f facts.Fact, target string) bool {
	for _, c := range relsByKind(f, facts.RelCalls) {
		if c == target {
			return true
		}
	}
	return false
}

// TestAST_ParamCall_NoEdge: a callable parameter invoked by name must not resolve
// to a same-module symbol.
func TestAST_ParamCall_NoEdge(t *testing.T) {
	files := map[string]string{
		"svc.py": `
def wrapper(callback):
    callback()
`,
	}
	result := astExtractWithIndex(t, files, "svc.py", false)
	idx := byName(result)

	fn, ok := idx["svc.wrapper"]
	if !ok {
		t.Fatalf("missing svc.wrapper; keys: %v", keys(idx))
	}
	if hasCallTo(fn, "svc.callback") {
		t.Errorf("param 'callback' wrongly resolved to svc.callback; calls=%v", relsByKind(fn, facts.RelCalls))
	}
}

// TestAST_LocalCallable_NoEdge: a locally-assigned callable invoked by name must
// not resolve to a same-module symbol (it is not a module-level def).
func TestAST_LocalCallable_NoEdge(t *testing.T) {
	files := map[string]string{
		"svc.py": `
def f():
    fn = lambda: None
    fn()
`,
	}
	result := astExtractWithIndex(t, files, "svc.py", false)
	idx := byName(result)

	fn, ok := idx["svc.f"]
	if !ok {
		t.Fatalf("missing svc.f; keys: %v", keys(idx))
	}
	if hasCallTo(fn, "svc.fn") {
		t.Errorf("local 'fn' wrongly resolved to svc.fn; calls=%v", relsByKind(fn, facts.RelCalls))
	}
}

// TestAST_LoopVarCall_NoEdge: a loop variable invoked by name must not resolve to
// a same-module symbol.
func TestAST_LoopVarCall_NoEdge(t *testing.T) {
	files := map[string]string{
		"svc.py": `
def f(handlers):
    for handler in handlers:
        handler()
`,
	}
	result := astExtractWithIndex(t, files, "svc.py", false)
	idx := byName(result)

	fn, ok := idx["svc.f"]
	if !ok {
		t.Fatalf("missing svc.f; keys: %v", keys(idx))
	}
	if hasCallTo(fn, "svc.handler") {
		t.Errorf("loop var 'handler' wrongly resolved to svc.handler; calls=%v", relsByKind(fn, facts.RelCalls))
	}
}

// TestAST_SameModuleCall_StillResolves: a genuine same-module top-level def call
// must still emit a RelCalls edge (regression guard for the bug 05 fix).
func TestAST_SameModuleCall_StillResolves(t *testing.T) {
	files := map[string]string{
		"svc.py": `
def helper():
    pass

def main():
    helper()
`,
	}
	result := astExtractWithIndex(t, files, "svc.py", false)
	idx := byName(result)

	fn, ok := idx["svc.main"]
	if !ok {
		t.Fatalf("missing svc.main; keys: %v", keys(idx))
	}
	if !hasCallTo(fn, "svc.helper") {
		t.Errorf("main: expected RelCalls to svc.helper; got %v", relsByKind(fn, facts.RelCalls))
	}
}

// TestAST_PopOnEmptyStack_NoPanic: popOwner/popType must be no-ops on empty
// stacks rather than panicking with a slice-bounds underflow (defensive hardening).
func TestAST_PopOnEmptyStack_NoPanic(t *testing.T) {
	w := &pyWalker{}
	// Must not panic on empty stacks.
	w.popOwner()
	w.popType()
	if len(w.ownerStack) != 0 || len(w.typeStack) != 0 || len(w.methodSets) != 0 {
		t.Fatalf("expected all stacks to remain empty, got owner=%d type=%d methodSets=%d",
			len(w.ownerStack), len(w.typeStack), len(w.methodSets))
	}
}

// --- Shadow-guard coverage: localBound (params + assigned/iterated/aliased
// locals) extends the param-only guard above to direct calls and value-refs ---

func TestAST_ShadowedParamNotResolvedAsCall(t *testing.T) {
	src := `
def send():
    pass

def register(send):
    send()
`
	result := astExtract(t, "svc.py", src, false)
	idx := byName(result)

	regFact, ok := idx["svc.register"]
	if !ok {
		t.Fatalf("missing svc.register; keys: %v", keys(idx))
	}
	if calls := relsByKind(regFact, facts.RelCalls); len(calls) != 0 {
		t.Errorf("register: expected no RelCalls (send is a param), got %v", calls)
	}
}

func TestAST_ShadowedLoopVarNotResolvedAsCall(t *testing.T) {
	src := `
def helper():
    pass

def process(items):
    for helper in items:
        helper()
`
	result := astExtract(t, "svc.py", src, false)
	idx := byName(result)

	procFact, ok := idx["svc.process"]
	if !ok {
		t.Fatalf("missing svc.process; keys: %v", keys(idx))
	}
	for _, c := range relsByKind(procFact, facts.RelCalls) {
		if c == "svc.helper" {
			t.Errorf("process: unexpected RelCalls to svc.helper (helper is a loop var)")
		}
	}
}

func TestAST_ShadowGuard_UnshadowedCallStillResolves(t *testing.T) {
	src := `
def helper():
    pass

def main(x):
    y = 5
    helper()
`
	result := astExtract(t, "svc.py", src, false)
	idx := byName(result)

	mainFact, ok := idx["svc.main"]
	if !ok {
		t.Fatalf("missing svc.main; keys: %v", keys(idx))
	}
	found := false
	for _, c := range relsByKind(mainFact, facts.RelCalls) {
		if c == "svc.helper" {
			found = true
		}
	}
	if !found {
		t.Error("main: expected RelCalls to svc.helper (unrelated locals x/y must not block it)")
	}
}

func TestAST_KeywordArgumentCallbackEmitsRef(t *testing.T) {
	src := `
def on_done():
    pass

def schedule(callback):
    pass

def start():
    schedule(callback=on_done)
`
	result := astExtractIdx(t, "svc.py", src)
	fn := byName(result)["svc.start"]
	calls := relsByKind(fn, facts.RelCalls)
	found := false
	for _, c := range calls {
		if c == "svc.on_done" {
			found = true
		}
	}
	if !found {
		t.Errorf("start: RelCalls = %v, want a keyword-argument value-ref to svc.on_done", calls)
	}
}

func TestAST_DispatchTable_ListValues(t *testing.T) {
	src := `
def handle_a():
    pass

def handle_b():
    pass

def build():
    handlers = [handle_a, handle_b]
    return handlers
`
	result := astExtractIdx(t, "svc.py", src)
	fn := byName(result)["svc.build"]
	calls := relsByKind(fn, facts.RelCalls)
	for _, want := range []string{"svc.handle_a", "svc.handle_b"} {
		found := false
		for _, c := range calls {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("build: RelCalls = %v, want %s", calls, want)
		}
	}
}

// --- Assignment/return value-refs: coverage upstream's value-ref pass doesn't
// have (it only walks call args, decorator args, and collection literals) ---

func TestAST_AssignedAndReturnedCallback(t *testing.T) {
	src := `
def handler():
    pass

def register():
    cb = handler
    return cb

def get_handler():
    return handler
`
	result := astExtractIdx(t, "svc.py", src)
	idx := byName(result)

	regFact, ok := idx["svc.register"]
	if !ok {
		t.Fatalf("missing svc.register; keys: %v", keys(idx))
	}
	if calls := relsByKind(regFact, facts.RelCalls); len(calls) == 0 || calls[0] != "svc.handler" {
		t.Errorf("register: RelCalls = %v, want [svc.handler]", calls)
	}

	getFact, ok := idx["svc.get_handler"]
	if !ok {
		t.Fatalf("missing svc.get_handler; keys: %v", keys(idx))
	}
	if calls := relsByKind(getFact, facts.RelCalls); len(calls) == 0 || calls[0] != "svc.handler" {
		t.Errorf("get_handler: RelCalls = %v, want [svc.handler]", calls)
	}
}

func TestAST_TupleAssignedCallbacksEmitEdges(t *testing.T) {
	src := `
def run_codex_once(prompt):
    pass

def run_copilot_once(prompt):
    pass

def choose(agent):
    if agent == "codex":
        run_once, agent_name = run_codex_once, "Codex"
    else:
        run_once, agent_name = run_copilot_once, "Copilot"
    return run_once, agent_name
`
	result := astExtractIdx(t, "review.py", src)
	fn := byName(result)["review.choose"]
	for _, want := range []string{"review.run_codex_once", "review.run_copilot_once"} {
		if !hasCallTo(fn, want) {
			t.Errorf("choose: RelCalls = %v, want tuple-assignment function-value ref to %q", relsByKind(fn, facts.RelCalls), want)
		}
	}
}

func TestAST_MainGuardTupleAssignedCallbacksEmitFileRefs(t *testing.T) {
	src := `
import sys

def run_codex_once(prompt):
    pass

def run_copilot_once(prompt):
    pass

if __name__ == "__main__":
    if "--codex" in sys.argv:
        run_once, agent_name = run_codex_once, "Codex"
    else:
        run_once, agent_name = run_copilot_once, "Copilot"
    run_once("review")
`
	result := astExtractIdx(t, "review.py", src)
	fr, ok := firstOfKind(result, facts.KindFileRef)
	if !ok {
		t.Fatal("expected a file_ref for main-guard callback assignments")
	}
	for _, want := range []string{"review.run_codex_once", "review.run_copilot_once"} {
		if !hasCallTo(fr, want) {
			t.Errorf("file_ref: RelCalls = %v, want tuple-assignment function-value ref to %q", relsByKind(fr, facts.RelCalls), want)
		}
	}
}

func TestAST_AssignedCallback_ShadowGuarded(t *testing.T) {
	src := `
def handler():
    pass

def register(handler):
    cb = handler
    return cb
`
	result := astExtractIdx(t, "svc.py", src)
	idx := byName(result)

	regFact, ok := idx["svc.register"]
	if !ok {
		t.Fatalf("missing svc.register; keys: %v", keys(idx))
	}
	if calls := relsByKind(regFact, facts.RelCalls); len(calls) != 0 {
		t.Errorf("register: expected no RelCalls (handler is a param), got %v", calls)
	}
}

func TestAST_ConstructorParamShadowsSynonymProperty(t *testing.T) {
	src := `
class DagRun:
    @property
    def state(self):
        return self._state

    def __init__(self, state=None):
        self.state = state
`
	result := astExtractIdx(t, "models.py", src)
	idx := byName(result)

	initFact, ok := idx["models.DagRun.__init__"]
	if !ok {
		t.Fatalf("missing models.DagRun.__init__; keys: %v", keys(idx))
	}
	if calls := relsByKind(initFact, facts.RelCalls); len(calls) != 0 {
		t.Errorf("__init__: expected no RelCalls (state is the param, not the property), got %v", calls)
	}
}

func TestAST_ReturnedPlainVariable_NoPhantomRef(t *testing.T) {
	src := `
GREETING = "hi"

def process():
    return GREETING
`
	result := astExtractIdx(t, "svc.py", src)
	idx := byName(result)

	procFact, ok := idx["svc.process"]
	if !ok {
		t.Fatalf("missing svc.process; keys: %v", keys(idx))
	}
	if calls := relsByKind(procFact, facts.RelCalls); len(calls) != 0 {
		t.Errorf("process: expected no RelCalls (GREETING is not a def), got %v", calls)
	}
}

func TestAST_ReturnedForwardReference_Resolves(t *testing.T) {
	src := `
def build():
    return later_handler

def later_handler():
    pass
`
	result := astExtractIdx(t, "svc.py", src)
	idx := byName(result)

	buildFact, ok := idx["svc.build"]
	if !ok {
		t.Fatalf("missing svc.build; keys: %v", keys(idx))
	}
	calls := relsByKind(buildFact, facts.RelCalls)
	if len(calls) == 0 || calls[0] != "svc.later_handler" {
		t.Errorf("build: RelCalls = %v, want [svc.later_handler]", calls)
	}
}

// --- Pass 5: nested function/class scopes (v117) ---
//
// Nested defs get no symbol of their own, so their bodies' references are
// credited to the enclosing symbol (as lambdas always were). Metrics stay
// suppressed — a closure's branches and loops are not the enclosing function's
// complexity — and nested bindings (name, params, locals) shadow same-named
// module defs so bare calls through them cannot fabricate edges.

func TestAST_NestedDefBodyCall_AttributedToEnclosing(t *testing.T) {
	src := `
def format_results(rows):
    return rows

async def search(query):
    async def search_task(q):
        return format_results(q)
    return await search_task(query)
`
	result := astExtractIdx(t, "svc.py", src)
	idx := byName(result)

	searchFact, ok := idx["svc.search"]
	if !ok {
		t.Fatalf("missing svc.search; keys: %v", keys(idx))
	}
	calls := relsByKind(searchFact, facts.RelCalls)
	found := false
	for _, c := range calls {
		if c == "svc.format_results" {
			found = true
		}
	}
	if !found {
		t.Errorf("search: RelCalls = %v, want svc.format_results (called only inside nested def)", calls)
	}
}

func TestAST_NestedDefLazyImportResolves(t *testing.T) {
	src := `
def outer():
    def task():
        from pkg.util import helper
        return helper()
    return task
`
	result := astExtractIdx(t, "svc.py", src)
	idx := byName(result)

	outerFact, ok := idx["svc.outer"]
	if !ok {
		t.Fatalf("missing svc.outer; keys: %v", keys(idx))
	}
	calls := relsByKind(outerFact, facts.RelCalls)
	found := false
	for _, c := range calls {
		if c == "pkg.util.helper" {
			found = true
		}
	}
	if !found {
		t.Errorf("outer: RelCalls = %v, want pkg.util.helper (lazy import inside nested def)", calls)
	}
}

func TestAST_NestedDefDecoratorReference(t *testing.T) {
	src := `
def retry_on_exception(fn):
    return fn

def outer():
    @retry_on_exception
    def attempt():
        pass
    return attempt()
`
	result := astExtractIdx(t, "svc.py", src)
	idx := byName(result)

	outerFact, ok := idx["svc.outer"]
	if !ok {
		t.Fatalf("missing svc.outer; keys: %v", keys(idx))
	}
	rels := append(relsByKind(outerFact, facts.RelCalls), relsByKind(outerFact, "instantiates")...)
	found := false
	for _, c := range rels {
		if c == "svc.retry_on_exception" {
			found = true
		}
	}
	if !found {
		t.Errorf("outer: relations = %v, want svc.retry_on_exception (decorator on nested def)", rels)
	}
}

func TestAST_NestedDefParamShadow_NoFalseEdge(t *testing.T) {
	src := `
def helper():
    pass

def outer():
    def task(helper):
        return helper()
    return task
`
	result := astExtractIdx(t, "svc.py", src)
	idx := byName(result)

	outerFact, ok := idx["svc.outer"]
	if !ok {
		t.Fatalf("missing svc.outer; keys: %v", keys(idx))
	}
	for _, c := range relsByKind(outerFact, facts.RelCalls) {
		if c == "svc.helper" {
			t.Errorf("outer: fabricated RelCalls to svc.helper — nested param shadows the module def")
		}
	}
}

func TestAST_NestedDefNameShadow_NoFalseEdge(t *testing.T) {
	src := `
def helper():
    pass

def outer():
    def helper():
        return 1
    return helper()
`
	result := astExtractIdx(t, "svc.py", src)
	idx := byName(result)

	outerFact, ok := idx["svc.outer"]
	if !ok {
		t.Fatalf("missing svc.outer; keys: %v", keys(idx))
	}
	for _, c := range relsByKind(outerFact, facts.RelCalls) {
		if c == "svc.helper" {
			t.Errorf("outer: fabricated RelCalls to module-level svc.helper — the nested def shadows it")
		}
	}
}

func TestAST_NestedDefMetricsSuppressed(t *testing.T) {
	src := `
def sink(x):
    return x

def outer():
    def worker(items):
        if items:
            for i in items:
                sink(i)
        return items
    return worker
`
	result := astExtractIdx(t, "svc.py", src)
	idx := byName(result)

	outerFact, ok := idx["svc.outer"]
	if !ok {
		t.Fatalf("missing svc.outer; keys: %v", keys(idx))
	}
	if cyc, _ := outerFact.Props["cyclomatic"].(int); cyc != 1 {
		t.Errorf("outer: cyclomatic = %v, want 1 (nested def's branches are not the enclosing function's complexity)", outerFact.Props["cyclomatic"])
	}
	if _, has := outerFact.Props["loop_count"]; has {
		t.Errorf("outer: loop_count = %v, want absent (loop lives in the nested def)", outerFact.Props["loop_count"])
	}
	if _, has := outerFact.Props["calls_in_loop"]; has {
		t.Errorf("outer: calls_in_loop = %v, want absent (nested-def loops must not seed N+1 candidates)", outerFact.Props["calls_in_loop"])
	}
	// The reference itself must still be credited.
	calls := relsByKind(outerFact, facts.RelCalls)
	found := false
	for _, c := range calls {
		if c == "svc.sink" {
			found = true
		}
	}
	if !found {
		t.Errorf("outer: RelCalls = %v, want svc.sink", calls)
	}
}

func TestAST_NestedClassMethodBodyCall_AttributedToEnclosing(t *testing.T) {
	src := `
def helper():
    pass

def outer():
    class Worker:
        def run(self):
            return helper()
    return Worker
`
	result := astExtractIdx(t, "svc.py", src)
	idx := byName(result)

	outerFact, ok := idx["svc.outer"]
	if !ok {
		t.Fatalf("missing svc.outer; keys: %v", keys(idx))
	}
	calls := relsByKind(outerFact, facts.RelCalls)
	found := false
	for _, c := range calls {
		if c == "svc.helper" {
			found = true
		}
	}
	if !found {
		t.Errorf("outer: RelCalls = %v, want svc.helper (called from a method of a function-nested class)", calls)
	}
}

// A function handed to a decorator as a VALUE is a real use: the decorator stores
// it and the framework invokes it later. The decorator-argument walk only looked
// for nested CALLS, so a bare identifier slipped past and the referenced function
// read as dead code.
func TestAST_DecoratorArgumentFunctionIsReferenced(t *testing.T) {
	src := `from pkg.dist import run_tasks_distributed
from pkg.reg import override_run_tasks


@override_run_tasks(run_tasks_distributed)
async def run_tasks(data):
    return data
`
	ff := astExtract(t, "pkg/ops.py", src, false)

	var targets []string
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls {
				targets = append(targets, r.Target)
			}
		}
	}
	want := "pkg.dist.run_tasks_distributed"
	found := false
	for _, tg := range targets {
		if tg == want {
			found = true
		}
	}
	if !found {
		t.Errorf("decorator argument not referenced: want %q among %v", want, targets)
	}
}

// The decorator's own call is still recorded, and a nested call inside the
// arguments still resolves — the value-reference pass must add to that walk, not
// replace it.
func TestAST_DecoratorArgumentKeepsNestedCalls(t *testing.T) {
	src := `from pkg.deps import Depends, requires_access


@router.get("/x", dependencies=[Depends(requires_access(method="GET"))])
def handler():
    return None
`
	ff := astExtract(t, "pkg/routes.py", src, false)

	var targets []string
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls {
				targets = append(targets, r.Target)
			}
		}
	}
	// requires_access is the inner call and resolves through the import map; Depends
	// is the outer call in the collection and stays a bare name (pre-existing, not
	// affected by the value-reference pass). Both must still be present — the point
	// is that adding value refs did not displace the nested-call walk.
	for _, want := range []string{"Depends", "pkg.deps.requires_access"} {
		found := false
		for _, tg := range targets {
			if tg == want {
				found = true
			}
		}
		if !found {
			t.Errorf("nested decorator-argument call lost: want %q among %v", want, targets)
		}
	}
}

// TestExtract_DeferredImportProp pins which imports run when the module is
// imported. An import-closure walk is only meaningful if it can exclude the ones
// that do not, and indentation cannot draw that line: a module-level
// `try: import x` is indented yet runs, while a `if TYPE_CHECKING:` import is
// indented and never runs at all.
func TestExtract_DeferredImportProp(t *testing.T) {
	src := `import os
from typing import TYPE_CHECKING

try:
    import fast_json as json
except ImportError:
    import json

if TYPE_CHECKING:
    from collections import OrderedDict

def loader():
    import heavy_module
    return heavy_module

class Holder:
    def method(self):
        from decimal import Decimal
        return Decimal
`
	ff, _ := extractFileAST([]byte(src), "pkg/mod.py", false, false, false, nil)

	want := map[string]bool{ // target -> deferred
		"os":           false,
		"typing":       false,
		"fast_json":    false, // module-level try: runs at import
		"json":         false, // except arm: also runs at import
		"collections":  true,  // TYPE_CHECKING: never runs
		"heavy_module": true,  // function-local
		"decimal":      true,  // method-local
	}
	seen := map[string]bool{}
	for _, f := range ff {
		if f.Kind != facts.KindDependency {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelImports {
				continue
			}
			exp, tracked := want[r.Target]
			if !tracked {
				continue
			}
			seen[r.Target] = true
			got, _ := f.Props["deferred"].(bool)
			if got != exp {
				t.Errorf("import %q: deferred = %v, want %v", r.Target, got, exp)
			}
		}
	}
	for target := range want {
		if !seen[target] {
			t.Errorf("no dependency fact emitted for import %q", target)
		}
	}
}

// TestExtract_MethodLocalImportEmittedOnce guards against a method body being
// registered twice — once by handleFunction, once by walkNestedScope reached through
// the class body's call walk — and emitting the import fact both times. Each pass
// needs the importMap binding; only one may emit, or the import is double-counted by
// every consumer that sums dependency facts.
func TestExtract_MethodLocalImportEmittedOnce(t *testing.T) {
	src := `class Holder:
    def build(self):
        from app.registry import registry
        return registry
`
	ff, _ := extractFileAST([]byte(src), "app/holder.py", false, false, false, nil)
	n := 0
	for _, f := range ff {
		if f.Kind != facts.KindDependency {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImports && r.Target == "app.registry" {
				n++
			}
		}
	}
	if n != 1 {
		t.Errorf("method-local import emitted %d dependency facts, want exactly 1", n)
	}
}
