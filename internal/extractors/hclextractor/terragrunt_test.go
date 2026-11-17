package hclextractor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// Units include a parent configuration that names a remote module, deploy a
// local module of their own, and read other units; each is a module with those
// dependencies. A commented-out dependency does not count.
func TestTerragruntUnits(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"live/root.hcl":                 "locals { env = \"prod\" }\n",
		"live/apps/module.hcl":          "locals {\n  url     = \"git::https://example.test/modules//app\"\n  version = \"v1\"\n}\nterraform {\n  source = \"${local.url}?ref=${local.version}\"\n}\n",
		"live/apps/shop/terragrunt.hcl": "include \"root\" {\n  path = find_in_parent_folders(\"root.hcl\")\n}\ninclude \"module\" {\n  path = find_in_parent_folders(\"module.hcl\")\n}\ndependency \"network\" {\n  config_path = \"../../network\"\n}\n# dependency \"old\" {\n#   config_path = \"../../old\"\n# }\ninputs = { name = \"shop\" }\n",
		"live/network/terragrunt.hcl":   "include {\n  path = find_in_parent_folders(\"root.hcl\")\n}\nterraform {\n  source = \"../../modules/vpc//net?ref=v2\"\n}\ndependencies {\n  paths = [\"../dns\"]\n}\n",
		"live/dns/terragrunt.hcl":       "terraform {\n  source = \"../../modules/dns\"\n}\n",
		"modules/vpc/net/main.tf":       "resource \"aws_vpc\" \"main\" {\n  cidr_block = \"10.0.0.0/16\"\n}\n",
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
	edges := map[string][]string{}
	modules := map[string]facts.Fact{}
	for _, f := range ff {
		switch f.Kind {
		case facts.KindDependency:
			if f.PropString("terragrunt") != "" {
				from := f.Name[:len(f.Name)-len(" -> "+f.Relations[0].Target)]
				edges[from] = append(edges[from], f.PropString("terragrunt")+":"+f.Relations[0].Target)
			}
		case facts.KindModule:
			modules[f.Name] = f
		}
	}
	for _, v := range edges {
		sort.Strings(v)
	}
	want := map[string][]string{
		"live/apps/shop": {"dependency:live/network", "include:live", "include:live/apps"},
		"live/network":   {"dependency:live/dns", "include:live", "source:modules/vpc/net"},
		"live/dns":       {"source:modules/dns"},
	}
	for unit, w := range want {
		if got := edges[unit]; !slices.Equal(got, w) {
			t.Errorf("%s edges = %v, want %v", unit, got, w)
		}
	}
	if m := modules["live/apps/shop"]; !m.PropBool("terragrunt_unit") || m.PropString("module_source") != "git::https://example.test/modules//app?ref=v1" {
		t.Errorf("shop module = %v", m.Props)
	}
}
