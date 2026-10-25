package tsextractor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func extractTSFiles(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return extractDir(t, dir)
}

const tsLedgerFixture = `import { Channel } from './channel';
import { Store } from './store';

export class Ledger {
	private _channel: Channel | null;
	private count = 0;
	readonly onDone = () => this.finish();

	constructor(private readonly store: Store) {
		this._channel = null;
	}

	open(): void {
		this._channel = new Channel(this.count);
		const local = new Map<string, number>();
		const host = this.hostFor('a');
		host.start();
		this.hostFor('b').stop();
		this.store.save();
	}

	private hostFor(id: string): Channel {
		return new Channel(1);
	}

	finish(): void {}
}
`

// A field that holds a value is a data member, and reading it is a reference, not
// a call; a field holding an arrow function stays a method. A constructor call of a
// class the file imports is an instantiation, a global's is not. A local assigned
// what a method of the class returns has the method's annotated return type.
func TestTSClassFieldsAndConstructors(t *testing.T) {
	ff := extractTSFiles(t, map[string]string{
		"src/channel.ts": "export class Channel {\n\tconstructor(n: number) {}\n\tstart(): void {}\n\tstop(): void {}\n}\n",
		"src/store.ts":   "export class Store {\n\tsave(): void {}\n}\n",
		"src/ledger.ts":  tsLedgerFixture,
	})
	for name, want := range map[string]string{
		"src.Ledger._channel": facts.SymbolVariable,
		"src.Ledger.count":    facts.SymbolVariable,
		"src.Ledger.onDone":   facts.SymbolMethod,
		"src.Ledger.open":     facts.SymbolMethod,
	} {
		if got := symbolNamed(t, ff, name).PropString("symbol_kind"); got != want {
			t.Errorf("%s symbol_kind = %q, want %q", name, got, want)
		}
	}
	open := symbolNamed(t, ff, "src.Ledger.open")
	for _, want := range []struct{ kind, target string }{
		{facts.RelNames, "src.Ledger._channel"},
		{facts.RelNames, "src.Ledger.count"},
		{facts.RelNames, "src.Ledger.store"},
		{facts.RelInstantiates, "src.Channel"},
		{facts.RelCalls, "src.Channel.start"},
		{facts.RelCalls, "src.Channel.stop"},
		{facts.RelCalls, "src.Store.save"},
		{facts.RelCalls, "src.Ledger.hostFor"},
	} {
		if !open.HasRelation(want.kind, want.target) {
			t.Errorf("open relations = %v, want %s %s", open.Relations, want.kind, want.target)
		}
	}
	for _, r := range open.Relations {
		if r.Kind == facts.RelCalls && (r.Target == "src.Ledger._channel" || r.Target == "src.Ledger.count" || r.Target == "src.Ledger.store") {
			t.Errorf("open calls the field %s", r.Target)
		}
		if r.Kind == facts.RelInstantiates && r.Target != "src.Channel" {
			t.Errorf("open instantiates %s, which the repository does not declare", r.Target)
		}
	}
	if done := symbolNamed(t, ff, "src.Ledger.onDone"); !done.HasRelation(facts.RelCalls, "src.Ledger.finish") {
		t.Errorf("onDone relations = %v, want a call to finish", done.Relations)
	}
}

// An interface's members are declared, so a call through a value of its type lands
// on one: a method signature and a function-typed property are methods, any other
// property a variable, all abstract. An overload is declared once; a computed key
// is not declared.
func TestTSInterfaceMembersAreDeclared(t *testing.T) {
	ff := extractTSFiles(t, map[string]string{
		"src/starter.ts": `export interface IStarter {
	readonly brand: undefined;
	start(id: string): Promise<void>;
	start(id: string, opts: object): Promise<void>;
	onExit: (code: number) => void;
	kill?(id: string): void;
	['computed']: number;
}
`,
		"src/host.ts": `import { IStarter } from './starter';

export class Host {
	constructor(private readonly starter: IStarter) {}

	launch(): void {
		this.starter.start('a');
	}
}
`,
	})
	for name, want := range map[string]string{
		"src.IStarter.start":  facts.SymbolMethod,
		"src.IStarter.onExit": facts.SymbolMethod,
		"src.IStarter.kill":   facts.SymbolMethod,
		"src.IStarter.brand":  facts.SymbolVariable,
	} {
		f := symbolNamed(t, ff, name)
		if got := f.PropString("symbol_kind"); got != want {
			t.Errorf("%s symbol_kind = %q, want %q", name, got, want)
		}
		if b, _ := f.PropAny("abstract").(bool); !b {
			t.Errorf("%s: not abstract", name)
		}
	}
	n := 0
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Name == "src.IStarter.start" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("src.IStarter.start declared %d times, want once", n)
	}
	if launch := symbolNamed(t, ff, "src.Host.launch"); !launch.HasRelation(facts.RelCalls, "src.IStarter.start") {
		t.Errorf("launch relations = %v, want a call to IStarter.start", launch.Relations)
	}
}

// A name destructured from a typed parameter, and a member read through a typed
// local, have the type their declaration states, in another file too: the first
// parameter of a function-type alias, an interface parameter (with a rename), and
// `app.scene`. A member nothing declares adds no edge, and no deferred name leaks.
func TestTSDestructuredAndMemberTypedCalls(t *testing.T) {
	ff := extractTSFiles(t, map[string]string{
		"src/scene.ts": "export class Scene {\n\ttriggerUpdate(): void {}\n\tmutate(): void {}\n}\n",
		"src/types.ts": `import { Scene } from './scene';

export type DragCallback = (props: { scene: Scene; count: number }) => void;

export interface AppProps {
	scene: Scene;
}
`,
		"src/stats.ts": `import type { DragCallback, AppProps } from './types';

export const onDrag: DragCallback = ({ scene, count }) => {
	scene.triggerUpdate();
};

export function open(app: AppProps): void {
	app.scene.mutate();
	app.missing.mutate();
}

export function refresh({ scene: s }: AppProps): void {
	s.triggerUpdate();
}
`,
	})
	for from, want := range map[string]string{
		"src.onDrag":  "src.Scene.triggerUpdate",
		"src.open":    "src.Scene.mutate",
		"src.refresh": "src.Scene.triggerUpdate",
	} {
		if f := symbolNamed(t, ff, from); !f.HasRelation(facts.RelCalls, want) {
			t.Errorf("%s relations = %v, want a call to %s", from, f.Relations, want)
		}
	}
	for _, f := range ff {
		for _, r := range f.Relations {
			if strings.Contains(r.Target, memberSep) {
				t.Errorf("%s: deferred target %q leaked", f.Name, r.Target)
			}
		}
		for _, p := range []string{propMemberTypes, propParam0Members, propParam0Type} {
			if _, kept := f.Prop(p); kept {
				t.Errorf("%s: note %s leaked", f.Name, p)
			}
		}
	}
}
