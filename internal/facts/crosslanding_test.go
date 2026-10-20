package facts

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// writtenTargetIDs writes the store and returns, for each relation, the kind
// of the fact its target_id names, or "" when it carries none. A relation is
// keyed "source name|relation kind|target".
func writtenTargetIDs(t *testing.T, s *Store) map[string]string {
	t.Helper()
	var buf bytes.Buffer
	if err := s.WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	type line struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Relations []struct {
			Kind     string `json:"kind"`
			Target   string `json:"target"`
			TargetID string `json:"target_id"`
		} `json:"relations"`
	}
	var lines []line
	kindOf := map[string]string{}
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var l line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, l)
		kindOf[l.ID] = l.Kind
	}
	got := map[string]string{}
	for _, l := range lines {
		for _, r := range l.Relations {
			key := l.Name + "|" + r.Kind + "|" + r.Target
			if r.TargetID == "" {
				got[key] = ""
				continue
			}
			kind, ok := kindOf[r.TargetID]
			if !ok {
				t.Fatalf("%s: target_id %s names no fact in the snapshot", key, r.TargetID)
			}
			got[key] = kind
		}
	}
	return got
}

// In a union the name lookup reaches into every repository when the caller's
// own holds nothing of the name, and a call's bare name meets facts that share
// it by coincidence: a Rust `Walk::default()` derived from `Default` meets a
// Ruby job queue named default, and a method named like a repository meets the
// service node standing for that repository. Neither is what the call reaches.
func TestCallTargetID_ACallOntoAnotherRepositorysNonSymbolGetsNoID(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindStorage, Name: "default", File: "jobs/app/jobs/report_job.rb", Repo: "jobs",
			Props: map[string]any{"language": "ruby", "storage_kind": "topic"}},
		Fact{Kind: KindModule, Name: "config", File: "jobs/config", Repo: "jobs"},
		Fact{Kind: KindRoute, Name: "/reports", File: "jobs/config/routes.rb", Repo: "jobs"},
		Fact{Kind: KindSymbol, Name: "crates/pic/src.Walk", File: "atlas/crates/pic/src/lib.rs", Repo: "atlas",
			Props: map[string]any{"language": "rust"}},
		Fact{Kind: KindSymbol, Name: "crates/pic/src.write", File: "atlas/crates/pic/src/lib.rs", Repo: "atlas",
			Props: map[string]any{"language": "rust", "unresolved_calls": "no_method_on_type=1"},
			Relations: []Relation{
				{Kind: RelCalls, Target: "default"},
				{Kind: RelCalls, Target: "config"},
				{Kind: RelCalls, Target: "/reports"},
				{Kind: RelInstantiates, Target: "config"},
				{Kind: RelInstantiates, Target: "crates/pic/src.Walk"},
			}},
	)
	got := writtenTargetIDs(t, s)
	for key, want := range map[string]string{
		"crates/pic/src.write|calls|default":                    "",
		"crates/pic/src.write|calls|config":                     "",
		"crates/pic/src.write|calls|/reports":                   "",
		"crates/pic/src.write|instantiates|config":              "",
		"crates/pic/src.write|instantiates|crates/pic/src.Walk": KindSymbol,
	} {
		if got[key] != want {
			t.Errorf("%s: target_id names a %q, want %q", key, got[key], want)
		}
	}
}

func TestCallTargetID_ACallOntoAServiceNodeGetsNoID(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindService, Name: "atlas", Repo: "atlas", Props: map[string]any{"synthetic": "crossrepo"}},
		Fact{Kind: KindService, Name: "jobs", Repo: "jobs", Props: map[string]any{"synthetic": "crossrepo"}},
		Fact{Kind: KindSymbol, Name: "crates/pic/src.Kind.atlas", File: "atlas/crates/pic/src/lib.rs", Repo: "atlas",
			Props: map[string]any{"language": "rust", "receiver": "Kind"}},
		Fact{Kind: KindSymbol, Name: "crates/pic/src.placed", File: "atlas/crates/pic/src/lib.rs", Repo: "atlas",
			Props: map[string]any{"language": "rust", "unresolved_calls": "receiver_expression=1"},
			Relations: []Relation{
				{Kind: RelCalls, Target: "atlas"},
				{Kind: RelCalls, Target: "jobs"},
				{Kind: RelInstantiates, Target: "atlas"},
			}},
		Fact{Kind: KindFileRef, Name: "atlas/app/templates/index.hbs", File: "atlas/app/templates/index.hbs", Repo: "atlas",
			Props:     map[string]any{"language": "handlebars"},
			Relations: []Relation{{Kind: RelCalls, Target: "atlas"}}},
	)
	got := writtenTargetIDs(t, s)
	for _, key := range []string{
		"crates/pic/src.placed|calls|atlas",
		"crates/pic/src.placed|calls|jobs",
		"crates/pic/src.placed|instantiates|atlas",
		"atlas/app/templates/index.hbs|calls|atlas",
	} {
		if kind := got[key]; kind != "" {
			t.Errorf("%s carries an id onto a %s; a service node stands for a repository, and no call reaches one", key, kind)
		}
	}
}

// What a union is for must survive: a call onto a symbol another repository
// declares, a service's dependency on another service, an import or a call
// through the alias a consumer's own import wrote, and every non-call relation
// onto another repository's module or table.
func TestCallTargetID_CrossRepositoryEdgesThatLandKeepTheirID(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "billing.internal.Charge", File: "billing/internal/charge.go", Repo: "billing",
			Props: map[string]any{"language": "go"}},
		Fact{Kind: KindAssociation, Name: "Invoice#lines", File: "billing/app/models/invoice.rb", Repo: "billing"},
		Fact{Kind: KindModule, Name: "adapters", File: "billing/adapters", Repo: "billing"},
		Fact{Kind: KindSymbol, Name: "adapters.Client.Charge", File: "billing/adapters/client.go", Repo: "billing",
			Props: map[string]any{"language": "go"}},
		Fact{Kind: KindStorage, Name: "invoices", File: "billing/db/schema.rb", Repo: "billing"},
		Fact{Kind: KindService, Name: "billing", Repo: "billing", Props: map[string]any{"synthetic": "crossrepo"}},
		Fact{Kind: KindService, Name: "frontend", Repo: "frontend", Props: map[string]any{"synthetic": "crossrepo"},
			Relations: []Relation{{Kind: RelDependsOn, Target: "billing"}}},
		Fact{Kind: KindSymbol, Name: "web.Checkout", File: "frontend/web/checkout.go", Repo: "frontend",
			Props: map[string]any{"language": "go"},
			Relations: []Relation{
				{Kind: RelCalls, Target: "billing.internal.Charge"},
				{Kind: RelCalls, Target: "Invoice#lines"},
				{Kind: RelCalls, Target: "github.com/acme/billing/adapters.Client.Charge"},
				{Kind: RelImports, Target: "github.com/acme/billing/adapters"},
				{Kind: RelImports, Target: "adapters"},
				{Kind: RelNames, Target: "invoices"},
			}},
	)
	s.SetTargetAliases(map[string]FactKey{
		"github.com/acme/billing/adapters":               {Repo: "billing", Kind: KindModule, Name: "adapters", File: "billing/adapters"},
		"github.com/acme/billing/adapters.Client.Charge": {Repo: "billing", Kind: KindSymbol, Name: "adapters.Client.Charge", File: "billing/adapters/client.go"},
	})
	got := writtenTargetIDs(t, s)
	for key, want := range map[string]string{
		"web.Checkout|calls|billing.internal.Charge":                        KindSymbol,
		"web.Checkout|calls|Invoice#lines":                                  KindAssociation,
		"web.Checkout|calls|github.com/acme/billing/adapters.Client.Charge": KindSymbol,
		"web.Checkout|imports|github.com/acme/billing/adapters":             KindModule,
		"web.Checkout|imports|adapters":                                     KindModule,
		"web.Checkout|names|invoices":                                       KindStorage,
		"frontend|depends_on|billing":                                       KindService,
	} {
		if got[key] != want {
			t.Errorf("%s: target_id names a %q, want the %s it named before", key, got[key], want)
		}
	}
}

// A call's own repository is where a same-named module or route is its
// language's to decide, as before: an Ember link onto its own route keeps the
// id, and a route of the same name in another repository is never preferred
// over it nor reached when the caller's repository holds none.
func TestCallTargetID_ACallStillLandsInItsOwnRepository(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindRoute, Name: "catalog", File: "shop/app/router.ts", Repo: "shop", Props: map[string]any{"language": "typescript"}},
		Fact{Kind: KindRoute, Name: "catalog", File: "admin/app/router.ts", Repo: "admin", Props: map[string]any{"language": "typescript"}},
		Fact{Kind: KindRoute, Name: "account", File: "admin/app/router.ts", Repo: "admin", Props: map[string]any{"language": "typescript"}},
		Fact{Kind: KindFileRef, Name: "shop/app/templates/index.hbs", File: "shop/app/templates/index.hbs", Repo: "shop",
			Props: map[string]any{"language": "handlebars"},
			Relations: []Relation{
				{Kind: RelCalls, Target: "catalog"},
				{Kind: RelCalls, Target: "account"},
			}},
	)
	got := writtenTargetIDs(t, s)
	for key, want := range map[string]string{
		"shop/app/templates/index.hbs|calls|catalog": KindRoute,
		"shop/app/templates/index.hbs|calls|account": "",
	} {
		if got[key] != want {
			t.Errorf("%s: target_id names a %q, want %q", key, got[key], want)
		}
	}
}
