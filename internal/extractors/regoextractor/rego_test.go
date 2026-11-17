package regoextractor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// Rules reference a rule of their own package by name, another package's through
// an imported alias and through data., and the package-level path of a data
// document that is no rule names nothing. Test files are not read; a rule with
// several definitions is one symbol carrying every body's references.
func TestRegoRulesAndReferences(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"policies/shared/token.rego": "package shared.token\n\nimport rego.v1\n\nverified := io.jwt.decode(input.token) if {\n  true\n}\n\nvalid_issuer(iss) if {\n  iss == \"idp\"\n}\n",
		"policies/orders/permissions.rego": `package orders.sandbox

import data.shared
import data.shared.token as tok
import rego.v1

default allow := false

permissions := data.permissions if {
	data.permissions
} else := data.policies.orders.sandbox.permissions

allow if {
	verified := shared.token.verified
	has_permission(permissions.groups, verified.groups)
}

allow if {
	tok.valid_issuer(input.iss)
}

# deny if nothing else
has_permission(perms, principals) if {
	some p in principals
	perms[p]
}
`,
		"policies/orders/permissions_test.rego": "package orders.sandbox\n\ntest_allow if {\n  allow with input as {}\n}\n",
	}
	var rels []string
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	ff, err := New().Extract(context.Background(), dir, rels)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]facts.Fact{}
	for _, f := range ff {
		got[f.Name] = f
	}
	deps := func(name string) []string {
		var out []string
		for _, r := range got[name].Relations {
			if r.Kind == facts.RelDependsOn {
				out = append(out, r.Target)
			}
		}
		sort.Strings(out)
		return out
	}
	if d := deps("orders.sandbox.allow"); !slices.Equal(d, []string{"orders.sandbox.has_permission", "orders.sandbox.permissions", "shared.token.valid_issuer", "shared.token.verified"}) {
		t.Errorf("allow depends on %v", d)
	}
	if d := deps("orders.sandbox.has_permission"); len(d) != 0 {
		t.Errorf("has_permission depends on %v, want nothing", d)
	}
	if _, ok := got["orders.sandbox.test_allow"]; ok {
		t.Error("a test file was read")
	}
	if f := got["orders.sandbox.allow"]; f.Line != 7 || f.PropString(facts.PropFrameworkRoot) != "opa" {
		t.Errorf("allow at line %d, props %v", f.Line, f.Props)
	}
	want := []string{"orders.sandbox.allow", "orders.sandbox.has_permission", "orders.sandbox.permissions", "shared.token.valid_issuer", "shared.token.verified"}
	var names []string
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if !slices.Equal(names, want) {
		t.Errorf("rules = %v, want %v", names, want)
	}
}
