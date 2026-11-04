package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestCallOnAFactorysResultResolvesByItsReturnType(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{
		"package.json": `{"name":"fx"}`,
		"src/engine/engine.ts": `export class Engine {
  run(): number { return this.step(); }
  step(): number { return 1; }
  session(): Session { return new Session(); }
}
export class Session { commit(): void {} }
`,
		"src/engine/factory.ts": `import { Engine } from "./engine";
export function makeEngine(): Engine { return new Engine(); }
export async function getEngine(): Promise<Engine> { return new Engine(); }
export function inferred() { return new Engine(); }
export const arrow = () => new Engine();
export function either(flag: boolean) { return flag ? new Engine() : null; }
export class Registry {
  static default(): Engine { return new Engine(); }
}
`,
		"src/api/api.ts": `import { makeEngine, getEngine, inferred, arrow, either, Registry } from "../engine/factory";
import { Engine } from "../engine/engine";

export function imported(): number { const e = makeEngine(); return e.run(); }
export async function awaited(): Promise<number> { const e = await getEngine(); return e.run(); }
export async function notAwaited() { const p = getEngine(); return p.then(); }
export function fromInferred(): number { const e = inferred(); return e.run(); }
export function fromArrow(): number { const e = arrow(); return e.run(); }
export function chained(): void { makeEngine().session().commit(); }
export async function awaitedChain(): Promise<number> { return (await getEngine()).run(); }
export function fromStatic(): number { const e = Registry.default(); return e.run(); }
export function mixed(flag: boolean) { const e = either(flag); return e?.run(); }
function local(): Engine { return new Engine(); }
export function sameFile(): number { const e = local(); return e.run(); }
`,
	})

	run := "src/engine.Engine.run"
	for _, name := range []string{"api.imported", "api.awaited", "api.fromInferred", "api.fromArrow", "api.awaitedChain", "api.fromStatic", "api.sameFile"} {
		f := tsSym(t, ff, name)
		if !f.HasRelation(facts.RelCalls, run) {
			t.Errorf("%s must call Engine.run; relations: %v", name, f.Relations)
		}
	}
	if f := tsSym(t, ff, "api.chained"); !f.HasRelation(facts.RelCalls, "src/engine.Session.commit") {
		t.Errorf("a chain is typed step by step; relations: %v", f.Relations)
	}
	if f := tsSym(t, ff, "api.mixed"); f.HasRelation(facts.RelCalls, run) {
		t.Errorf("a function returning new Engine or null declares no type; relations: %v", f.Relations)
	}
	for _, f := range ff {
		if f.PropAny(propReturnType) != nil || f.PropAny(propAwaitedType) != nil {
			t.Errorf("%s keeps a return-type prop that must not leave the extractor", f.Name)
		}
	}
}
