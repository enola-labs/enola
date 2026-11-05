package pythonextractor

import (
	"testing"
)

func TestCallOnImportedFactoryResultResolvesByReturnType(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"app/__init__.py": "",
		"app/engine.py": `from abc import ABC, abstractmethod


class Engine(ABC):
    @abstractmethod
    def add_nodes(self, nodes): ...

    def session(self) -> "Session":
        return Session()


class Session:
    def commit(self):
        return 1


class KuzuEngine(Engine):
    def add_nodes(self, nodes):
        return nodes


class NeoEngine(Engine):
    def add_nodes(self, nodes):
        return nodes
`,
		"app/db/__init__.py": "from app.db.factory import get_engine, Registry\n",
		"app/db/factory.py": `from ..engine import Engine


async def get_engine() -> Engine:
    return KuzuEngine()


def untyped_engine():
    return KuzuEngine()


class Registry:
    @classmethod
    def default(cls) -> Engine:
        return KuzuEngine()
`,
		"app/tasks.py": `from app.db import get_engine, Registry
from app.db.factory import untyped_engine
import app.db.factory as factory


async def store(nodes):
    engine = await get_engine()
    await engine.add_nodes(nodes)


async def chained():
    (await get_engine()).session().commit()


def through_module():
    e = factory.untyped_engine()
    return e.add_nodes([])


def through_classmethod(nodes):
    e = Registry.default()
    return e.add_nodes(nodes)


def unannotated(nodes):
    e = untyped_engine()
    return e.add_nodes(nodes)
`,
	})

	store := pySym(t, ff, "tasks.store")
	for _, want := range []string{
		"app/engine.Engine.add_nodes",
		"app/engine.KuzuEngine.add_nodes",
		"app/engine.NeoEngine.add_nodes",
	} {
		if !pyCallsTo(store, want) {
			t.Errorf("store must call %s through what get_engine returns; relations: %v", want, store.Relations)
		}
	}
	if f := pySym(t, ff, "tasks.chained"); !pyCallsTo(f, "app/engine.Session.commit") {
		t.Errorf("a chain is typed step by step; relations: %v", f.Relations)
	}
	if f := pySym(t, ff, "tasks.through_classmethod"); !pyCallsTo(f, "app/engine.Engine.add_nodes") {
		t.Errorf("a classmethod's return annotation types its result; relations: %v", f.Relations)
	}
	for _, name := range []string{"tasks.unannotated", "tasks.through_module"} {
		f := pySym(t, ff, name)
		for _, r := range f.Relations {
			if isCallMarked(r.Target) || r.Target == "app/engine.Engine.add_nodes" {
				t.Errorf("%s: a factory with no return annotation types nothing; got %v", name, r)
			}
		}
	}
}
