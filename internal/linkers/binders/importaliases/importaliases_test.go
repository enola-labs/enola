package importaliases

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func goRoot(repo, modulePath string) facts.Fact {
	return facts.Fact{Kind: facts.KindModule, Name: ".", File: ".", Repo: repo,
		Props: map[string]any{"language": "go", "modulePath": modulePath}}
}

func sym(repo, name, file string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Repo: repo}
}

// targetIDs writes the store and returns, per relation target of the consumer's
// facts, the target_id the writer gave it ("" when none).
func targetIDs(t *testing.T, store *facts.Store, repo string) map[string]string {
	t.Helper()
	var buf bytes.Buffer
	if err := store.WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var f struct {
			Repo      string `json:"repo"`
			Relations []struct {
				Target   string `json:"target"`
				TargetID string `json:"target_id"`
			} `json:"relations"`
		}
		if err := json.Unmarshal([]byte(l), &f); err != nil {
			t.Fatal(err)
		}
		if f.Repo != repo {
			continue
		}
		for _, r := range f.Relations {
			out[r.Target] = r.TargetID
		}
	}
	return out
}

// The consumer's import-path spelling resolves to the provider's own facts: a root
// package symbol, a subpackage method, and a package import landing on the module.
// A module nested under another's path claims its own targets.
func TestImportAliases_GoTargetsResolveToTheProvider(t *testing.T) {
	consumer := sym("golf", "internal/app.Service.Login", "golf/internal/app/service.go")
	consumer.Relations = []facts.Relation{
		{Kind: facts.RelCalls, Target: "github.com/acme/auth.AuthService.Verify"},
		{Kind: facts.RelCalls, Target: "github.com/acme/auth/adapters.Handler.Login"},
		{Kind: facts.RelCalls, Target: "github.com/acme/auth/tools.Run"},
		{Kind: facts.RelCalls, Target: "github.com/acme/authz.Check"},
	}
	imp := facts.Fact{Kind: facts.KindDependency, Name: "internal/app -> github.com/acme/auth/adapters",
		File: "golf/internal/app/service.go", Repo: "golf",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "github.com/acme/auth/adapters"}}}

	store := facts.NewStore()
	store.Add(
		goRoot("golf", "golf"), consumer, imp,
		goRoot("auth", "github.com/acme/auth"),
		sym("auth", "..AuthService.Verify", "auth/service.go"),
		sym("auth", "adapters.Handler.Login", "auth/adapters/handler.go"),
		facts.Fact{Kind: facts.KindModule, Name: "adapters", File: "auth/adapters", Repo: "auth"},
		// A decoy: the auth repo also has a tools package, but tools is its own module.
		sym("auth", "tools.Run", "auth/tools/run.go"),
		goRoot("authtools", "github.com/acme/auth/tools"),
		sym("authtools", "..Run", "authtools/run.go"),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	ids := targetIDs(t, store, "golf")
	for target, want := range map[string]string{
		"github.com/acme/auth.AuthService.Verify":     facts.FactID("auth", facts.KindSymbol, "..AuthService.Verify", "auth/service.go"),
		"github.com/acme/auth/adapters.Handler.Login": facts.FactID("auth", facts.KindSymbol, "adapters.Handler.Login", "auth/adapters/handler.go"),
		"github.com/acme/auth/adapters":               facts.FactID("auth", facts.KindModule, "adapters", "auth/adapters"),
		"github.com/acme/auth/tools.Run":              facts.FactID("authtools", facts.KindSymbol, "..Run", "authtools/run.go"),
		"github.com/acme/authz.Check":                 "",
	} {
		if got := ids[target]; got != want {
			t.Errorf("%s target_id = %q, want %q", target, got, want)
		}
	}
}

// Two distinct provider facts under the local name leave the target unresolved.
func TestImportAliases_AmbiguousProviderNameIsSkipped(t *testing.T) {
	consumer := sym("golf", "internal/app.Run", "golf/internal/app/run.go")
	consumer.Relations = []facts.Relation{{Kind: facts.RelCalls, Target: "github.com/acme/auth.New"}}
	store := facts.NewStore()
	store.Add(
		consumer,
		goRoot("auth", "github.com/acme/auth"),
		sym("auth", "..New", "auth/a.go"),
		sym("auth", "..New", "auth/b.go"),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if got := targetIDs(t, store, "golf")["github.com/acme/auth.New"]; got != "" {
		t.Fatalf("an ambiguous name was resolved: %q", got)
	}
}

func tsModule(repo, dir, pkg string) facts.Fact {
	return facts.Fact{Kind: facts.KindModule, Name: dir, File: dir, Repo: repo,
		Props: map[string]any{"language": "typescript", "package_name": pkg}}
}

// A package-qualified target resolves to the one symbol the package declares under
// that name, in whichever of its directories; a package import resolves to the
// package's root module; a longer package name sharing the prefix is not confused
// with it; two same-named symbols in the package resolve to nothing. It applies in a
// single repository too, where a monorepo imports its own workspace package.
func TestImportAliases_TypeScriptPackageTargets(t *testing.T) {
	consumer := sym("web", "src/app.run", "web/src/app/run.ts")
	consumer.Relations = []facts.Relation{
		{Kind: facts.RelCalls, Target: "@acme/sdk.createClient"},
		{Kind: facts.RelCalls, Target: "@acme/sdk/http.Transport.send"},
		{Kind: facts.RelCalls, Target: "@acme/sdk.duplicated"},
		{Kind: facts.RelCalls, Target: "@acme/sdk-extra.createClient"},
	}
	imp := facts.Fact{Kind: facts.KindDependency, Name: "src/app -> @acme/sdk", File: "web/src/app/run.ts", Repo: "web",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: "@acme/sdk"}}}

	store := facts.NewStore()
	store.Add(
		consumer, imp,
		tsModule("sdk", "src", "@acme/sdk"),
		tsModule("sdk", "src/client", "@acme/sdk"),
		tsModule("sdk", "src/http", "@acme/sdk"),
		sym("sdk", "src/client.createClient", "sdk/src/client/index.ts"),
		sym("sdk", "src/http.Transport.send", "sdk/src/http/transport.ts"),
		sym("sdk", "src/client.duplicated", "sdk/src/client/a.ts"),
		sym("sdk", "src/http.duplicated", "sdk/src/http/b.ts"),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	ids := targetIDs(t, store, "web")
	for target, want := range map[string]string{
		"@acme/sdk.createClient":        facts.FactID("sdk", facts.KindSymbol, "src/client.createClient", "sdk/src/client/index.ts"),
		"@acme/sdk/http.Transport.send": facts.FactID("sdk", facts.KindSymbol, "src/http.Transport.send", "sdk/src/http/transport.ts"),
		"@acme/sdk":                     facts.FactID("sdk", facts.KindModule, "src", "src"),
		"@acme/sdk.duplicated":          "",
		"@acme/sdk-extra.createClient":  "",
	} {
		if got := ids[target]; got != want {
			t.Errorf("%s target_id = %q, want %q", target, got, want)
		}
	}

	// The same package imported inside its own repository resolves the same way.
	mono := facts.NewStore()
	self := sym("sdk", "apps/web.run", "apps/web/run.ts")
	self.Relations = []facts.Relation{{Kind: facts.RelCalls, Target: "@acme/sdk.createClient"}}
	mono.Add(self, tsModule("sdk", "src/client", "@acme/sdk"), sym("sdk", "src/client.createClient", "src/client/index.ts"))
	if err := New().Bind(context.Background(), mono); err != nil {
		t.Fatal(err)
	}
	if got := targetIDs(t, mono, "sdk")["@acme/sdk.createClient"]; got != facts.FactID("sdk", facts.KindSymbol, "src/client.createClient", "src/client/index.ts") {
		t.Errorf("workspace package import not resolved in a single repository: %q", got)
	}
}

// A subpath import names a file under the package root, so it settles which of two
// same-named exports the target means.
func TestImportAliases_SubpathNarrowsTheExport(t *testing.T) {
	consumer := sym("web", "src/app.run", "web/src/app/run.ts")
	consumer.Relations = []facts.Relation{{Kind: facts.RelCalls, Target: "@acme/features/auth/lib/session.getSession"}}
	store := facts.NewStore()
	store.Add(
		consumer,
		tsModule("mono", "packages/features", "@acme/features"),
		tsModule("mono", "packages/features/auth/lib", "@acme/features"),
		tsModule("mono", "packages/features/billing", "@acme/features"),
		sym("mono", "packages/features/auth/lib.getSession", "mono/packages/features/auth/lib/session.ts"),
		sym("mono", "packages/features/billing.getSession", "mono/packages/features/billing/session.ts"),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	want := facts.FactID("mono", facts.KindSymbol, "packages/features/auth/lib.getSession", "mono/packages/features/auth/lib/session.ts")
	if got := targetIDs(t, store, "web")["@acme/features/auth/lib/session.getSession"]; got != want {
		t.Fatalf("target_id = %q, want the auth/lib export %q", got, want)
	}
}

// A fully qualified type name resolves to the type whose fqn prop it is, and a
// member after it to that type's member, across repositories; a qualified name two
// types claim resolves to nothing.
func TestImportAliases_QualifiedTypeTargets(t *testing.T) {
	typ := func(repo, name, file, fqn string) facts.Fact {
		f := sym(repo, name, file)
		f.Props = map[string]any{"symbol_kind": "class", "fqn": fqn}
		return f
	}
	consumer := sym("storefront", "src/main/java/com/shop.Checkout.run", "storefront/src/main/java/com/shop/Checkout.java")
	consumer.Relations = []facts.Relation{
		{Kind: facts.RelInstantiates, Target: "com.acme.inventory.Client"},
		{Kind: facts.RelCalls, Target: "com.acme.inventory.Client.reserve"},
		{Kind: facts.RelInstantiates, Target: "com.acme.dup.Thing"},
	}
	store := facts.NewStore()
	store.Add(
		consumer,
		typ("inventory", "src/main/java/com/acme/inventory.Client", "inventory/src/main/java/com/acme/inventory/Client.java", "com.acme.inventory.Client"),
		sym("inventory", "src/main/java/com/acme/inventory.Client.reserve", "inventory/src/main/java/com/acme/inventory/Client.java"),
		typ("a", "x.Thing", "a/x/Thing.java", "com.acme.dup.Thing"),
		typ("b", "y.Thing", "b/y/Thing.java", "com.acme.dup.Thing"),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	ids := targetIDs(t, store, "storefront")
	for target, want := range map[string]string{
		"com.acme.inventory.Client":         facts.FactID("inventory", facts.KindSymbol, "src/main/java/com/acme/inventory.Client", "inventory/src/main/java/com/acme/inventory/Client.java"),
		"com.acme.inventory.Client.reserve": facts.FactID("inventory", facts.KindSymbol, "src/main/java/com/acme/inventory.Client.reserve", "inventory/src/main/java/com/acme/inventory/Client.java"),
		"com.acme.dup.Thing":                "",
	} {
		if got := ids[target]; got != want {
			t.Errorf("%s target_id = %q, want %q", target, got, want)
		}
	}
}

// PHP needs no alias: its facts are named by namespace, the same spelling a consumer
// writes, so a reference into another loaded repository resolves by name.
func TestPHPReferencesResolveAcrossReposByName(t *testing.T) {
	consumer := sym("shop", `App\Checkout::run`, "shop/src/Checkout.php")
	consumer.Relations = []facts.Relation{{Kind: facts.RelCalls, Target: `Acme\Inventory\Client::reserve`}}
	store := facts.NewStore()
	store.Add(consumer, sym("inventory", `Acme\Inventory\Client::reserve`, "inventory/src/Client.php"))
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	want := facts.FactID("inventory", facts.KindSymbol, `Acme\Inventory\Client::reserve`, "inventory/src/Client.php")
	if got := targetIDs(t, store, "shop")[`Acme\Inventory\Client::reserve`]; got != want {
		t.Fatalf("target_id = %q, want %q", got, want)
	}
}
