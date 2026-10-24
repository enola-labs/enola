package rustextractor

import (
	"slices"
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
