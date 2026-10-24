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
pub struct OwnedTasks;
impl OwnedTasks {
    pub fn bind(&self) {}
}
pub struct Harness<T> { cell: T }
impl<T> Harness<T> {
    pub fn from_raw(ptr: usize) -> Harness<T> { todo!() }
    pub fn poll(self) {}
}
`,
		"src/sched.rs": `
use std::sync::Arc;
use crate::tasks::{Harness, OwnedTasks};

pub struct Shared { owned: OwnedTasks }
pub struct Handle { shared: Shared }

impl Handle {
    fn bind_new_task(me: &Arc<Self>, other: Thing) {
        me.schedule();
        me.shared.owned.bind();
        let harness = Harness::<u8>::from_raw(0);
        harness.poll();
        let tasks: &OwnedTasks = &me.shared.owned;
        tasks.bind();
        other.bind();
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
	bind := rustSym(t, ff, "src.Handle.bind_new_task")
	calls := rustCalls(bind)
	for _, want := range []string{
		"src.Handle.schedule",  // &Arc<Self>
		"src.OwnedTasks.bind",  // field chain, and a let's written type
		"src.Harness.from_raw", // an associated function through a turbofish
		"src.Harness.poll",     // what from_raw returns
	} {
		if !slices.Contains(calls, want) {
			t.Errorf("bind_new_task calls %v, want %s", calls, want)
		}
	}
	if !slices.Contains(calls, "bind") {
		t.Errorf("bind_new_task calls %v, want other.bind kept by its bare name", calls)
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
