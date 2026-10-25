package tsextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func drizzleRepoFiles(withDep bool) map[string]string {
	pkg := `{"name": "@acme/orders", "dependencies": {}}`
	if withDep {
		pkg = `{"name": "@acme/orders", "dependencies": {"drizzle-orm": "^0.36.0"}}`
	}
	return map[string]string{
		"package.json":             `{"name": "root", "private": true}`,
		"libs/orders/package.json": pkg,
		"libs/orders/src/ports.ts": `export interface OrderRepository {
  save(id: string): Promise<void>;
  find(id: string): Promise<string | null>;
  label(id: string): string;
}
`,
		"libs/orders/src/repo.ts": `import { sql } from 'drizzle-orm';
import { OrderRepository } from './ports';

export class DrizzleOrderRepository implements OrderRepository {
  constructor(private readonly db: any) {}
  async save(id: string) {
    await this.db.insert(orders).values({ id });
  }
  async find(id: string) {
    const [row] = await this.db.select().from(orders).where(eq(orders.id, id)).limit(1);
    return row ?? null;
  }
  async touch(id: string) {
    await this.db.update(orders).set({ id }).where(eq(orders.id, id));
  }
  async purge() {
    await this.db.execute(sql` + "`DELETE FROM orders`" + `);
  }
  label(id: string) {
    const parts = Array.from(id);
    const m = new Map<string, string>();
    m.set(id, id);
    return Object.values(parts).join('-');
  }
}
`,
		"libs/orders/src/usecase.ts": `import { OrderRepository } from './ports';

export class SaveAll {
  constructor(private readonly repo: OrderRepository) {}
  async run(ids: string[]) {
    for (const id of ids) {
      await this.repo.save(id);
    }
  }
  names(ids: string[]) {
    return ids.map((id) => this.repo.label(id));
  }
}
`,
	}
}

func symbolProp(t *testing.T, ff []facts.Fact, name, prop string) any {
	t.Helper()
	f, ok := findFact(ff, name)
	if !ok {
		t.Fatalf("no fact %q", name)
	}
	return f.PropAny(prop)
}

func TestDrizzle_BuilderChainsAreQueries(t *testing.T) {
	ff := extractRepoAllFiles(t, drizzleRepoFiles(true))
	for _, m := range []string{"save", "find", "touch", "purge"} {
		name := "libs/orders/src.DrizzleOrderRepository." + m
		if b, _ := symbolProp(t, ff, name, "io_direct").(bool); !b {
			t.Errorf("%s: not io_direct", name)
		}
	}
	if b, _ := symbolProp(t, ff, "libs/orders/src.DrizzleOrderRepository.label", "io_direct").(bool); b {
		t.Errorf("label: Array.from / Map.set / Object.values read as a query")
	}
}

func TestDrizzle_NotWithoutTheDependency(t *testing.T) {
	ff := extractRepoAllFiles(t, drizzleRepoFiles(false))
	if b, _ := symbolProp(t, ff, "libs/orders/src.DrizzleOrderRepository.find", "io_direct").(bool); b {
		t.Errorf("find: io_direct in a repository that does not depend on drizzle-orm")
	}
}

func TestDrizzle_UseCaseReachesQueryThroughPort(t *testing.T) {
	ff := extractRepoAllFiles(t, drizzleRepoFiles(true))
	if b, _ := symbolProp(t, ff, "libs/orders/src.SaveAll.run", "performs_io").(bool); !b {
		t.Errorf("SaveAll.run: no performs_io through OrderRepository.save")
	}
	if b, _ := symbolProp(t, ff, "libs/orders/src.SaveAll.names", "performs_io").(bool); b {
		t.Errorf("SaveAll.names: performs_io through a member no implementer queries in")
	}
	// The interface's members are facts: the I/O is on the member an implementer
	// queries in, and not on the one none does.
	if b, _ := symbolProp(t, ff, "libs/orders/src.OrderRepository.save", "performs_io").(bool); !b {
		t.Errorf("OrderRepository.save: no performs_io through its implementer")
	}
	if b, _ := symbolProp(t, ff, "libs/orders/src.OrderRepository.label", "performs_io").(bool); b {
		t.Errorf("OrderRepository.label: performs_io, though no implementer queries in it")
	}
}

func TestDeclaredImplementers_AmbiguousNameResolvesToNothing(t *testing.T) {
	all := []facts.Fact{
		{Kind: facts.KindSymbol, Name: "a.Repo", Props: map[string]any{"symbol_kind": facts.SymbolInterface}},
		{Kind: facts.KindSymbol, Name: "b.Repo", Props: map[string]any{"symbol_kind": facts.SymbolInterface}},
		{Kind: facts.KindSymbol, Name: "c.Store", Props: map[string]any{"symbol_kind": facts.SymbolInterface}},
		{Kind: facts.KindSymbol, Name: "d.Impl", Props: map[string]any{"symbol_kind": facts.SymbolClass},
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: "Repo"}, {Kind: facts.RelImplements, Target: "Store"}}},
	}
	got := declaredImplementers(all)
	if len(got["a.Repo"]) != 0 || len(got["b.Repo"]) != 0 {
		t.Errorf("ambiguous Repo resolved: %v", got)
	}
	if !slices.Equal(got["c.Store"], []string{"d.Impl"}) {
		t.Errorf("c.Store implementers = %v, want [d.Impl]", got["c.Store"])
	}
}

func TestImportPhaseSyntaxIsADependency(t *testing.T) {
	ff := extractRepoAllFiles(t, map[string]string{
		"package.json":  `{"name": "app"}`,
		"src/dyn.js":    "const m = await import.defer('./x.js');\n",
		"src/static.js": "import defer * as ns from './y.js';\nconsole.log(ns);\n",
		"src/x.js":      "export const x = 1;\n",
		"src/y.js":      "export const y = 1;\n",
	})
	for _, file := range []string{"src/dyn.js", "src/static.js"} {
		found := false
		for _, f := range findFactsByKind(ff, facts.KindDependency) {
			if f.File == file {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no dependency fact", file)
		}
	}
}
