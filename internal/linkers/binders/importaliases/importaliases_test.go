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
