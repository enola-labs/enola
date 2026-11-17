# Terraform / HCL — the module directory as the unit of scope

HCL is a data language whose block grammar is line-regular enough for an exact
scanner — the gRPC/OpenAPI hand-rolled tradition, no grammar dependency. A
Terraform directory is a module; blocks declare addresses and reference each
other by exactly those addresses, so every edge below is a literal the source
states.

Fixture: [`hcl_sample`](../../internal/engine/testdata/repos/hcl_sample/)

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| a directory holding `.tf` files | one module, `.` at the repository root | `module` |
| `resource`, `data`, `module`, `variable`, `output`, `provider` | a symbol named by its Terraform address, carrying `hcl_block` | `symbol` |
| each assignment inside `locals { }` | one symbol apiece, not just the first | `symbol` |
| a block in a non-root module | the same, prefixed by its directory (`modules/dns.var.zone`) | `symbol` |
| `var.region`, `aws_vpc.core` in an argument | `depends_on` to the block that declares that address | relation |
| `module "dns" { source = "./modules/dns" }` | a dependency tagged `internal`, drawing the directory edge | `dependency` |
| a reference to an address nothing declares | nothing — bare names resolve only against the declared set | — |

## Blocks become symbols, addressed as Terraform addresses them

Detected by any `.tf`/`.hcl` file within three directory levels. Each block —
`resource`, `data`, `module`, `variable`, `output`, `provider`, and every
assignment inside a `locals` block (all of them, not just the first block) —
becomes a symbol named by its Terraform address, prefixed with its directory
when the module is not the repo root:

```hcl
# main.tf
resource "aws_vpc" "core" {
  cidr_block = "10.0.0.0/16"
}

resource "aws_instance" "web" {
  subnet_id  = aws_vpc.core.id
  region     = var.region
  depends_on = [aws_vpc.core]
}

output "web_ip" {
  value = aws_instance.web.public_ip
}
```

```
symbol  aws_vpc.core       main.tf   props: hcl_block=resource
symbol  aws_instance.web   main.tf   props: hcl_block=resource
        relations: depends_on -> aws_vpc.core, depends_on -> var.region
symbol  output.web_ip      main.tf   props: hcl_block=output, exported=true
        relations: depends_on -> aws_instance.web
symbol  var.region         main.tf   props: hcl_block=variable, exported=true
```

`variable` and `output` blocks are `exported` — they are the module's public
surface, exactly as Terraform treats them.

## References are literals, scoped the way Terraform scopes them

Three reference forms draw `depends_on` edges, and only three:

- **Prefixed references** — `var.region`, `local.tags`, `module.dns`,
  `data.aws_ami.base` — are unambiguous by construction and always count.
- **Bare resource addresses** — `aws_vpc.core` in an expression — count *only*
  when the same directory declares that exact address. Terraform's own scoping
  rule, applied verbatim: prose in a description or a function name can never
  fabricate an edge, because the declared set is the filter.
- **`depends_on` lists** — explicit, and matched against the same declared set.

## A module block's literal source draws the directory dependency

```hcl
module "dns" {
  source = "./modules/dns"
  zone   = var.region
}
```

```
symbol      module.dns          main.tf    props: hcl_block=module
            relations: depends_on -> var.region
dependency  . -> modules/dns    main.tf    props: source=internal
            relations: imports -> modules/dns
module      modules/dns
symbol      modules/dns.var.zone      modules/dns/main.tf   props: hcl_block=variable
symbol      modules/dns.output.fqdn   modules/dns/main.tf
            relations: depends_on -> modules/dns.var.zone
```

A local source (`./` or `../`) resolves to the directory and draws the
module-to-module edge. A remote source (registry, git) is recorded on the
module symbol as `module_source` with `external=true` and draws nothing — a
missing edge beats a wrong one.

## Terragrunt units

A directory holding a `terragrunt.hcl` is a unit, deployed on its own. Its file
usually declares no Terraform block, so each unit is made a module of its own
(`terragrunt_unit: true`), with a dependency (`terragrunt` naming why) on:

| The unit writes | Dependency on | `terragrunt` |
|---|---|---|
| `include { path = find_in_parent_folders("module.hcl") }` | the nearest ancestor directory holding `module.hcl` (no argument: `terragrunt.hcl` or `root.hcl`); a literal `path` relative to the unit | `include` |
| `terraform { source = "../../modules/vpc//net?ref=v2" }`, its own or the included file's | the module directory, `modules/vpc/net` | `source` |
| `dependency "network" { config_path = "../network" }`, `dependencies { paths = [...] }` | the other unit's directory | `dependency` |

A remote source (registry, git, S3, or one built with `${…}`) draws no edge and is
recorded on the unit as `module_source`. Commented-out blocks are ignored.

## What is deliberately not extracted

- **Remote modules** are named, never followed — no registry fetch, no git
  clone, no fabricated address space for code that is not in the repository.
- **Expression evaluation** — `count`, `for_each`, conditionals, function
  calls and string interpolation are never evaluated; a reference inside them
  still counts as a reference, but no expansion is modeled.
- **Cross-directory bare addresses** — a bare address only resolves inside its
  own module directory, because that is the only scope Terraform gives it.
- **State and providers** — what a provider does with a resource at apply time
  is runtime, not architecture.
