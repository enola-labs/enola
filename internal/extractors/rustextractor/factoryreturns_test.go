package rustextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestRustLetBoundToAFunctionsResultIsTyped(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"fx\"\nversion = \"0.1.0\"\n",
		"src/engine.rs": `pub struct Engine;
impl Engine {
    pub fn run(&self) -> i32 { 1 }
    pub fn session(&self) -> Result<Session, String> { Ok(Session) }
}
pub struct Session;
impl Session { pub fn commit(&self) {} }
`,
		"src/factory.rs": `use crate::engine::Engine;
pub fn make_engine() -> Engine { Engine }
pub fn open() -> Result<Engine, String> { Ok(Engine) }
pub async fn connect() -> Result<Engine, String> { Ok(Engine) }
pub fn maybe() -> Option<Engine> { None }
`,
		"src/api.rs": `use crate::factory::{make_engine, open};
use crate::factory;
pub fn bare() -> i32 { let e = make_engine(); e.run() }
pub fn by_path() -> i32 { let e = factory::make_engine(); e.run() }
pub fn tried() -> Result<i32, String> { let e = open()?; Ok(e.run()) }
pub async fn awaited() -> Result<i32, String> { let e = factory::connect().await?; Ok(e.run()) }
pub fn chained() -> Result<(), String> { make_engine().session()?.commit(); Ok(()) }
pub fn untried() { let o = factory::maybe(); o.run(); }
`,
		"src/main.rs": "mod engine;\nmod factory;\nmod api;\nfn main() {}\n",
	})
	calls := func(name string) []string {
		for _, f := range ff {
			if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, name) {
				var out []string
				for _, r := range f.Relations {
					if r.Kind == facts.RelCalls {
						out = append(out, r.Target)
					}
				}
				return out
			}
		}
		t.Fatalf("no symbol ending in %s", name)
		return nil
	}
	has := func(list []string, target string) bool {
		for _, c := range list {
			if c == target {
				return true
			}
		}
		return false
	}
	for _, name := range []string{".bare", ".by_path", ".tried", ".awaited"} {
		if got := calls(name); !has(got, "src.Engine.run") {
			t.Errorf("%s must call Engine.run; calls: %v", name, got)
		}
	}
	if got := calls(".chained"); !has(got, "src.Session.commit") {
		t.Errorf("a chain through ? is typed step by step; calls: %v", got)
	}
	if got := calls(".untried"); has(got, "src.Engine.run") {
		t.Errorf("an Option<Engine> not unwrapped is no Engine; calls: %v", got)
	}
	for _, f := range ff {
		if f.PropAny(propTryReturns) != nil {
			t.Errorf("%s keeps %s, which must not leave the extractor", f.Name, propTryReturns)
		}
	}
}
