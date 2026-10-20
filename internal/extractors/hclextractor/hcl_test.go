package hclextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func extractHCL(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	var rel []string
	for f, c := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
		rel = append(rel, f)
	}
	ok, err := New().Detect(dir)
	if err != nil || !ok {
		t.Fatalf("Detect = %v, %v", ok, err)
	}
	ff, err := New().Extract(context.Background(), dir, rel)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return ff
}

func find(ff []facts.Fact, kind, name string) *facts.Fact {
	for i := range ff {
		if ff[i].Kind == kind && ff[i].Name == name {
			return &ff[i]
		}
	}
	return nil
}

func TestHCL_BlocksAndReferences(t *testing.T) {
	ff := extractHCL(t, map[string]string{
		"stacks/prod/main.tf": `variable "region" {
  type = string
}

resource "aws_vpc" "core" {
  cidr_block = "10.0.0.0/16"
  tags       = local.tags
}

resource "aws_instance" "web" {
  ami        = data.aws_ami.ubuntu.id
  subnet_id  = aws_vpc.core.id
  depends_on = [aws_vpc.core]
  region     = var.region
}

data "aws_ami" "ubuntu" {
  most_recent = true
}

locals {
  tags = { env = "prod" }
}

output "web_ip" {
  value = aws_instance.web.public_ip
}

module "dns" {
  source = "./modules/dns"
  zone   = var.region
}

module "registry_thing" {
  source = "registry.example.com/org/mod/aws"
}
`,
		"stacks/prod/modules/dns/main.tf": `variable "zone" {}
`,
	})

	web := find(ff, facts.KindSymbol, "stacks/prod.aws_instance.web")
	if web == nil {
		t.Fatal("aws_instance.web missing")
	}
	wantRefs := map[string]bool{
		"stacks/prod.data.aws_ami.ubuntu": false,
		"stacks/prod.aws_vpc.core":        false,
		"stacks/prod.var.region":          false,
	}
	for _, r := range web.Relations {
		if r.Kind == facts.RelDependsOn {
			if _, ok := wantRefs[r.Target]; ok {
				wantRefs[r.Target] = true
			} else {
				t.Errorf("unexpected reference %q", r.Target)
			}
		}
	}
	for ref, found := range wantRefs {
		if !found {
			t.Errorf("reference %q missing", ref)
		}
	}
	if out := find(ff, facts.KindSymbol, "stacks/prod.output.web_ip"); out == nil || !out.HasRelation(facts.RelDependsOn, "stacks/prod.aws_instance.web") {
		t.Errorf("output missing or unbound: %+v", out)
	}
	if loc := find(ff, facts.KindSymbol, "stacks/prod.local.tags"); loc == nil {
		t.Error("locals entry missing")
	}
	dep := find(ff, facts.KindDependency, "stacks/prod -> stacks/prod/modules/dns")
	if dep == nil {
		t.Error("local module source did not draw the directory dependency")
	}
	reg := find(ff, facts.KindSymbol, "stacks/prod.module.registry_thing")
	if reg == nil || reg.Props["external"] != true {
		t.Errorf("registry module = %+v, want marked external with no edge", reg)
	}
	if vpc := find(ff, facts.KindSymbol, "stacks/prod.aws_vpc.core"); vpc == nil || !vpc.HasRelation(facts.RelDependsOn, "stacks/prod.local.tags") {
		t.Errorf("vpc = %+v, want local.tags reference", vpc)
	}
}

func TestHCL_BareTokensNeedDeclaredAddress(t *testing.T) {
	ff := extractHCL(t, map[string]string{
		"main.tf": `resource "aws_s3_bucket" "logs" {
  bucket = "example.logs"
  note   = "uses format.v2 and max.size conventions"
}
`,
	})
	logs := find(ff, facts.KindSymbol, "aws_s3_bucket.logs")
	if logs == nil {
		t.Fatal("bucket missing")
	}
	for _, r := range logs.Relations {
		if r.Kind == facts.RelDependsOn {
			t.Errorf("prose token drew edge %v — bare addresses must match a declared address exactly", r)
		}
	}
}

func TestHCL_MultipleLocalsBlocks(t *testing.T) {
	ff := extractHCL(t, map[string]string{
		"main.tf": `locals {
  tags = { env = "prod" }
}

resource "aws_vpc" "core" {
  cidr_block = "10.0.0.0/16"
}

locals {
  region = "eu-north-1"
}
`,
	})
	if find(ff, facts.KindSymbol, "local.tags") == nil || find(ff, facts.KindSymbol, "local.region") == nil {
		t.Error("a second locals block was not scanned — every locals block counts")
	}
}

// TestHCL_BlockLineIsItsKeyword: a block's line is the line its keyword is on,
// whatever blank lines or indentation come before it.
func TestHCL_BlockLineIsItsKeyword(t *testing.T) {
	src := "provider \"aws\" {\n  region = \"eu\"\n}\n\n\nresource \"aws_instance\" \"web\" {\n  ami = var.ami\n}\n\n  variable \"ami\" {\n}\n\nmodule \"vpc\" {\n  source = \"./vpc\"\n}\n"
	data := "# zones\n\ndata \"aws_availability_zones\" \"all\" {\n}\n"
	ff := extractHCL(t, map[string]string{"main.tf": src, "data.tf": data, "vpc/main.tf": "output \"id\" {\n  value = 1\n}\n"})
	for _, c := range []struct {
		name string
		line int
	}{{"provider.aws", 1}, {"aws_instance.web", 6}, {"var.ami", 10}, {"module.vpc", 13}, {"data.aws_availability_zones.all", 3}} {
		if f := find(ff, facts.KindSymbol, c.name); f == nil || f.Line != c.line {
			t.Errorf("%s line = %v, want %d", c.name, f, c.line)
		}
	}
	if dep := find(ff, facts.KindDependency, ". -> vpc"); dep == nil || dep.Line != 13 {
		t.Errorf("module dependency = %+v, want line 13", dep)
	}
}
