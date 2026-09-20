# .NET — what enola extracts

Covers **C#, VB.NET, F#, Razor and XAML**, plus the MSBuild project system that
binds them into assemblies. One extractor reads all of them into one fact set, which is
what lets a VB class reference a C# type, and a `.razor` or `.xaml` view merge with
its code-behind.

C# is parsed with tree-sitter; VB.NET and F# with line scanners; Razor is scanned
for its C# regions; XAML and the MSBuild project and solution files are parsed as
XML.

Detected by a solution or project file of any .NET language (`.sln`, `.slnx`,
`.csproj`, `.fsproj`, `.vbproj`), or by any `.cs`, `.vb`, `.razor`, `.cshtml`,
`.xaml` or `.axaml` file, within four directory levels.

> Every .NET source language is read: C#, VB.NET, F#, Razor and XAML. A mixed
> solution has all of its projects, all of its `ProjectReference` edges, and
> symbols for every half — see [Razor](#razor--blazor-components-and-mvc-views),
> [XAML](#xaml--wpf-winui-maui-and-avalonia), [VB.NET](#vbnet) and [F#](#f).
>
> Reading `.fsproj` and `.vbproj` is also what keeps a claimed repository from
> being an empty one. `giraffe-fsharp/Giraffe` ships `Giraffe.slnx`, seven
> `.fsproj` and no `.cs` at all: detection matched the solution, this extractor
> claimed the repository, emitted **zero facts** and reported a successful
> snapshot — indistinguishable from an empty repo. It now emits the project graph,
> and logs that the sources went unread.

Fixture: [`csharp_sample`](../../internal/engine/testdata/repos/csharp_sample/) ·
Unit coverage in
[`dotnetextractor/`](../../internal/extractors/dotnetextractor/) —
`csharp_test.go`, `msbuild_test.go`, `razor_test.go`, `xaml_test.go`,
`vbnet_test.go`

> **Check your `mcp-arch.yaml` before you conclude .NET is unsupported.** A config file's
> `extractors:` list *replaces* the built-in default rather than merging with it, so a
> config written before an extractor existed silently disables it. A repository indexed
> with a stale list reports zero .NET facts and says nothing about why. Either add
> `csharp` to the list or delete the key to inherit the default. (The extractor is
> still named `csharp`; the rename is pending.)

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| a directory of `.cs` files | one module carrying its `project` (the owning `.csproj`) | `module` |
| a `.csproj` / `.fsproj` / `.vbproj` | that directory as a module carrying `project`, `target_framework`, `output_type`, `solution` | `module` |
| `<ProjectReference Include="..\B\B.csproj" />` | a `depends_on` relation to B's directory | relation |
| `<PackageReference Include="Serilog" />` | an external dependency tagged `package_manager=nuget` | `dependency` |
| `class` / `interface` / `struct` / `record` / `enum` / `delegate` | a symbol with `symbol_kind`, `namespace` and `fqn` | `symbol` |
| a method, constructor, operator | a symbol with `receiver` | `symbol` |
| a **public or protected** field or property | a symbol | `symbol` |
| a private field or property | *nothing* — see [What is deliberately not extracted](#what-is-deliberately-not-extracted) | — |
| `partial class Foo` in several files | **one** symbol carrying the union of every half's edges | `symbol` |
| `using Acme.Data;` | a dependency tagged `internal` / `external` / `stdlib` | `dependency` |
| `: BaseClass, IThing` | one `implements` relation per entry | relation |
| a sole constructor's parameters | `injects` relations | relation |
| `new Order()` | an `instantiates` relation | relation |
| `Lookup(id)` inside the declaring type | a `calls` relation | relation |
| `[Route("x")]` + `[HttpGet("y")]` | a server route at `/x/y`, `handled_by` its action | `route` |
| `app.MapGroup("api/x")` + `.MapGet("/y", H)` | a server route at `/api/x/y`, `handled_by` `H` | `route` |
| `class X : DbContext` with `DbSet<T>` | a context, plus each `T` as an entity carrying its table | `storage` |
| `httpClient.GetAsync("/api/x")`, `[Get("/api/x")]` | a `role=client` route — the calling side of a cross-repo edge | `route` |
| `services.AddScoped<IFoo, Foo>()` | an `instantiates` relation to both — the only place `Foo` is named | relation |

## The project system — where .NET's real dependency edges are

Everywhere else in this extractor a fact is named after the **directory** it lives
in. That is the right unit for a symbol and the wrong one for a dependency: .NET's
dependency unit is the **assembly**, it is declared in a project file, and a
`ProjectReference` between two of them is the one edge in a .NET solution that the
build system itself enforces.

```xml
<!-- src/Acme.Api/Acme.Api.csproj -->
<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <TargetFramework>net9.0</TargetFramework>
    <OutputType>Exe</OutputType>
  </PropertyGroup>
  <ItemGroup>
    <ProjectReference Include="..\Acme.Domain\Acme.Domain.csproj" />
    <PackageReference Include="Serilog" Version="4.2.0" />
  </ItemGroup>
</Project>
```

```
module  src/Acme.Api   props: project=Acme.Api, msbuild=true, target_framework=net9.0,
                              output_type=Exe, language=csharp
                       --depends_on--> src/Acme.Domain

dependency  src/Acme.Api -> Serilog   props: package_manager=nuget, version=4.2.0,
                                             source=external
```

A project root that also holds sources produces **one** module fact carrying both
halves, not two. Facts are name-keyed so the graph would merge them anyway, but
both would reach `facts.jsonl` and be counted twice in `fact_count` — a published
benchmark number.

Project files are read with a **token walk over `Name.Local`** rather than
unmarshalled into a struct, because the two formats disagree about namespaces: an
SDK-style project has none, a legacy one puts every element in the 2003 MSBuild
namespace. `Include` paths use **backslashes on every host platform**, so they are
normalised before any path operation — a plain `filepath.Dir` on macOS leaves them
embedded and the edge points nowhere.

**Conditions are ignored.** A `ProjectReference` guarded by
`Condition="'$(TargetFramework)' == 'net48'"` is a real reference under some build
configuration, and enola has no configuration to evaluate against. Taking every
branch over-approximates the edge set, which is the safe direction: a missing edge
hides a real dependency, an extra one describes a build that genuinely exists. The
same target reached twice still yields one edge.

A **solution** contributes grouping only — a `solution` prop naming which
assemblies ship together — and never an edge. Two projects in one solution have no
dependency by virtue of that alone, and drawing one would connect every project in
a 148-project monorepo to every other. Both formats are read: `.slnx` as XML,
`.sln` through its bespoke text format.

`IsTestProject`, or a `Microsoft.NET.Test.Sdk` package reference, sets
`module_role=test` and **outranks the path heuristic** — a test project named
`Acme.Verification/` under `src/` is a test project whatever its path suggests.

A reference escaping the repository root is dropped: it names a project that
cannot be in this graph.

## Razor — Blazor components and MVC views

**This is not a parse.** A Razor file interleaves HTML, C# and a transition syntax
that has no standalone grammar; the real one lives in the Razor compiler, which
generates a C# class. What runs here instead finds the regions where C# appears
and harvests the **names referenced** there. That is deliberately less than a
parse, and it is enough for the job it exists to do.

The job, measured before it existed: MudBlazor reported **5,749 orphans out of
9,287 symbols — 62% of a maintained component library** — because
`MudAlert.razor.cs` declares `OnClickHandler` and only `MudAlert.razor` calls it.

### A component and its code-behind are one type

```razor
@* src/MudBlazor/Components/Alert/MudAlert.razor *@
@namespace MudBlazor
@inherits MudComponentBase
@inject InternalMudLocalizer Localizer

<div class="@Classname" @onclick="OnClickHandler">
    @if (ShowCloseIcon) { <MudIconButton @onclick="OnCloseIconClickAsync" /> }
</div>
```

```
symbol  src/MudBlazor/Components/Alert.MudAlert
          props: symbol_kind=class, razor_component=true, framework=blazor,
                 partial=true, namespace=MudBlazor
          --implements--> MudComponentBase      (@inherits)
          --injects-----> InternalMudLocalizer  (@inject)
          --instantiates-> MudIconButton        (a PascalCase tag)
          --calls-------> Classname, OnClickHandler, ShowCloseIcon,
                          OnCloseIconClickAsync
```

No special handling makes this converge with `MudAlert.razor.cs`: both name the
same directory-anchored symbol and both carry `partial`, so the ordinary
[partial-type merge](#partial-types-are-one-type) unifies them. The component is
marked partial **even when no code-behind exists**, so the merge never depends on
whether one happens to be present.

`symbol_kind` is `class`, not `component`, and that is load-bearing rather than
pedantic. `symbol_kind` is what puts a name into the type index that resolves bare
type references, and `component` is not a type kind. Emitting it cost a real edge:
the `.razor` half merged over the `.razor.cs` half, carried the unrecognised kind
with it, and `DialogService.ShowCoreAsync --calls--> MudDialogContainer`
disappeared — turning live code into apparent dead code. The component-ness
travels as `razor_component` instead.

### Tag helpers, which carry no `@` at all

```html
<input asp-for="DisplayMenuFilter" class="form-check-input" type="checkbox" />
```

Every one of OrchardCore's view-model false positives was reached this way. An
MVC or Razor Pages view binds to its model through the `asp-for` family —
`asp-for`, `asp-validation-for`, `asp-validation-class-for`, `asp-items` — which
is plain HTML attribute syntax with **no Razor transition**. A scanner that
follows only `@` finds none of them.

A `.cshtml` view emits a `file_ref`, not a symbol: MVC and Razor Pages generate a
class nothing references by name, so a view can never itself become a dead-code
candidate, while still vouching for the members it binds.

```
file_ref  …/Views/AdminSettings.Edit.cshtml
            --calls--> AdminSettingsViewModel   (@model, reduced to its last segment)
            --calls--> DisplayMenuFilter, DisplayNewMenu, DisplayThemeToggler
```

### `@code` blocks are real C#

An `@code` / `@functions` body is handed to the ordinary C# walker inside a
synthetic compilation unit. The wrapper occupies exactly **one line** and is
followed by enough blank lines to put the body back on the line it occupies in
the `.razor` file, so every symbol's reported line is the real one rather than an
offset into a synthetic buffer.

### String literals, and the holes in them

Literal contents are blanked before names are harvested — `@T["Enable Admin Menu
filter"]` otherwise contributed `Enable`, `Admin`, `Menu` and `filter`. A
fabricated reference is worse than a missing one: it vouches for a symbol nothing
uses and *suppresses* a genuine dead-code finding.

**Interpolation holes survive.** `$"{Section.ToStringFast(true)}/{Previous.Link}"`
is a string whose braces hold real code, and it is how Blazor builds most of its
hrefs; blanking it wholesale would lose exactly the references worth having.

### `@page` is a UI route, never a server route

```
route  /counter/{id:int}   props: method=GET, type=page, framework=blazor
                           --handled_by--> src/App/Pages.Counter
```

A Blazor or Razor Pages URL is one the **browser** navigates to, not an HTTP
contract served to other services. Typed as a server route it would become a
cross-repo match candidate and an "unused route no client calls" finding; `type=page`
is the existing marker the linker already excludes for that reason.

### Razor results

| Repo | orphans before | after | rescued | regressed |
|---|---:|---:|---:|---:|
| OrchardCore | 8,382 | **6,497** | 1,885 | **0** |
| MudBlazor | 19,442 | 21,437 | 721 | 2 |

Counted over the symbols present in *both* snapshots, because the change also
**adds** symbols (`@code` members, component types) and every added symbol is a
new orphan candidate — which is why MudBlazor's total rises while the fix works.
Its rescue rate is the lower one because its orphan set is dominated by 10,629
constants and 4,010 variables that no markup references.

MudBlazor's two "regressions" are not regressions: both were a *test* file's
namespace import (`using MudBlazor.UnitTests.TestComponents.DropZone`) vouching
for an unrelated production nested class by short-name collision. Losing that
un-suppresses a genuine finding.

*Cost:* +100ms on MudBlazor's 1,987 `.razor`, +21ms on OrchardCore's 1,610
`.cshtml`.

*Limits:* `_Imports.razor`, `_ViewImports.cshtml` and `_ViewStart.cshtml` declare
no component and emit nothing. A Razor Pages route that comes from the `Pages/`
directory convention rather than an explicit `@page` template is not derived. A
reference is harvested by NAME, so an unrelated symbol sharing it is credited —
the same bias, in the same safe direction, as `test_ref`.

## XAML — WPF, WinUI, MAUI and Avalonia

Unlike Razor, XAML *is* XML, so this is a real token walk rather than a scan. It
looks for the three ways a view reaches into code:

```xml
<Page x:Class="Files.App.Views.SplashScreenPage"
      xmlns:conv="using:Files.App.Converters">
    <Image ImageFailed="Image_ImageFailed" />
    <TextBlock Text="{x:Bind BranchLabel, Mode=OneTime}" />
    <Button Click="OnGoClicked"
            IsEnabled="{Binding Path=CanNavigate,
                        Converter={StaticResource BoolNegationConverter}}" />
    <conv:StatusCenterItem />
</Page>
```

```
symbol  src/Files.App/Views.SplashScreenPage
          props: symbol_kind=class, xaml_view=true, framework=xaml, partial=true,
                 fqn=Files.App.Views.SplashScreenPage, namespace=Files.App.Views
          --instantiates-> StatusCenterItem
          --calls-------> Image_ImageFailed, OnGoClicked, BranchLabel,
                          CanNavigate, BoolNegationConverter
```

`x:Class` makes the document one half of a partial class, so it merges with
`SplashScreenPage.xaml.cs` through the same partial-type merge a `.razor`
component uses — and for the same reason `symbol_kind` is `class`, not a
XAML-specific kind: that prop is what puts a name into the type index.

A document with **no `x:Class`** — a ResourceDictionary, a style file — has no
class to attach to and emits a `file_ref` rather than inventing one.

### Namespaces are matched by URI, not by prefix

`encoding/xml` resolves a prefix to its URI before reporting a name, so
`<conv:StatusCenterItem>` arrives with `Space="using:Files.App.Converters"`. A
tag is an instantiation when its namespace URI is a `clr-namespace:` or `using:`
one — a type declared in this solution. Framework controls (`Grid`, `TextBlock`,
`Button`) live in the schema-URL namespace and are not repository symbols.

### Why handlers are gated rather than harvested

`Click="OnSave"` names a method; `Stretch="None"` and `FontWeight="SemiBold"` do
not, and they are the same syntax. A bare-identifier attribute value is far more
often an enum member than a handler, so a value is taken as a method only when
the attribute is a known event **or** the value follows one of the two dominant
handler conventions (`OnSave`, `Image_ImageFailed`). Harvesting every bare value
would vouch for symbols nothing uses, which *suppresses* genuine dead-code
findings — the direction worth guarding, and the same rule the Razor scanner
applies to string literals.

Inside a markup extension only the ARGUMENTS are read. `Binding`,
`StaticResource` and `x:Bind` are syntax, and so are the named arguments that
configure a binding rather than name code — `Mode`, `RelativeSource`,
`FallbackValue`, `ElementName`, `StringFormat`. Nested extensions are followed,
which is how `Converter={StaticResource BoolNegationConverter}` reaches the
converter class.

`x:Name` and `x:Key` **declare**; they are not references.

### XAML results

| Repo | orphans before | after | rescued | regressed |
|---|---:|---:|---:|---:|
| Files (WinUI) | 4,818 | **4,399** | 419 | **0** |
| Avalonia | 14,827 | **14,213** | 615 | **0** |

No symbols are added — a view merges into the code-behind symbol that already
existed — so unlike Razor there is no orphan-candidate inflation and the totals
fall directly. What comes back is what the corpus predicted: `AdaptiveGridView`,
`BladeItem.CloseButtonForeground`, `BreadcrumbBar.EllipsisButtonToolTip`,
Avalonia's `GenericValueConverter` and `TestItemView`.

*Cost:* immeasurable at this scale — +3ms on Files' 145 documents, within noise
on Avalonia's 481.

*Limits:* a handler whose name follows neither convention and whose event this
file does not know is missed. `{Binding}` with no path names nothing. A reference
is harvested by NAME, so an unrelated symbol sharing it is credited — the same
bias, in the same safe direction, as `test_ref`.

## VB.NET

Read **into the same fact set as C#**, which is the whole point: a .NET solution
mixes the two languages inside one assembly, so a VB class referencing a C# type
must resolve through the one shared type index.

Line-oriented rather than AST-driven, and that is a property of the language
rather than a shortcut — VB terminates every construct with an explicit `End
Class` / `End Sub`, has no braces, and no expression-level ambiguity about where a
declaration begins. (There is also no maintained tree-sitter grammar for it.)

```vb
Namespace Microsoft.CodeAnalysis.VisualBasic.Symbols
    Partial Friend Class SourceNamedTypeSymbol
        Inherits SourceMemberContainerTypeSymbol
        Implements IAttributeTargetSymbol

        Friend Sub New(declaration As MergedTypeDeclaration,
                       containingSymbol As NamespaceOrTypeSymbol)
            MyBase.New(declaration)
        End Sub
    End Class
End Namespace
```

```
symbol  …/Symbols.SourceNamedTypeSymbol  props: symbol_kind=class, partial=true,
                                                exported=false, language=vbnet,
                                                namespace=Microsoft.CodeAnalysis.…
          --implements--> SourceMemberContainerTypeSymbol, IAttributeTargetSymbol
symbol  …/Symbols.SourceNamedTypeSymbol.New   props: symbol_kind=constructor
```

Details that are VB rather than C#:

- **Case-insensitive.** `imports` and `End Sub` are legal VB, so every keyword
  match folds case.
- **`Friend` is `internal`**, so it is not part of the exported surface — the same
  treatment the C# walker gives `internal`.
- **A `Module` is a static class**, not a namespace. It carries `vb_module` but its
  `symbol_kind` is `class`, because that prop is what puts a name into the type
  index that resolves bare references.
- **Line continuations are folded** — a trailing `_` or comma. roslyn's VB writes
  multi-line parameter lists constantly, and without folding the second line parses
  as a stray statement and the declaration is lost.
- **`Handles ctl.Click`** is a reference. It is how a WinForms or WPF handler is
  wired, and without it a handler nothing calls by name reads as dead — the VB
  counterpart of XAML's `Click=`.
- **`Implements IEquatable(Of T).Equals`** on a member names the interface member
  it satisfies; generic arguments are dropped.
- **Private fields are not symbols**, matching the C# rule.
- `.Designer.vb`, `.Generated.vb`, `My Project/` and an `<auto-generated>` header
  produce nothing.

### VB.NET results

On dotnet/roslyn, whose VB compiler is the largest VB codebase in the open:

| | orphans | rescued | regressed |
|---|---:|---:|---:|
| before | 60,726 | | |
| after | **59,209** | 6,644 | 33 |

Both sides measured with the test-glob fix below already applied, so the numbers
are VB's contribution alone rather than the two changes mixed.

The 33 are a documented rule firing correctly rather than a defect. roslyn ships
two parallel compilers, and the VB one declares its own
`CodeGen.CodeGenerator.CallKind` beside the C# one. A simple name that was unique
no longer is, so [resolution](#resolution--why-it-is-a-whole-repository-pass) step
3 declines to guess and the reference is left as written. Binding it would have
pointed a C# reference at the VB compiler's type.

*Cost:* +4.3s on roslyn's 2,752 VB files.

### The test-directory globs were case-sensitive

Found while measuring this, and it is not VB-specific. Glob matching is
case-sensitive, and .NET names its test directories in **PascalCase** — roslyn puts
784 files under `Test/` and 73 under `Tests/`, none of which `**/test/**` or
`**/tests/**` ever matched. They were being indexed as production code, in C# as
well as VB, since the C# extractor shipped.

Both casings are now listed for every .NET source extension. On roslyn alone this
moves 4,734 files out of the graph and drops the orphan count from 129,003 to
60,726 — a larger effect than any extractor in this series.

## F#

Read into the same fact set as the rest, and **indentation-scoped** rather than
delimiter-scoped — F# closes a module or type by dedenting, so the walker carries a
scope stack keyed on column instead of matching an `End` keyword.

```fsharp
namespace Giraffe

[<RequireQualifiedAccess>]
module SubRouting =
    open Microsoft.AspNetCore.Http

    let private RouteKey = "giraffe_route"

    let routeWithPartialPath (path: string) (handler: HttpHandler) : HttpHandler =
        let saved = getSavedPartialPath ctx
        handler next ctx
```

```
symbol  src/Giraffe.SubRouting          props: symbol_kind=class, fsharp_module=true,
                                               namespace=Giraffe
symbol  src/Giraffe.SubRouting.RouteKey props: symbol_kind=variable, exported=false
symbol  src/Giraffe.SubRouting.routeWithPartialPath
                                        props: symbol_kind=function
                                        --calls--> getSavedPartialPath, handler, ctx
```

- **A module is a static class**, which is what it compiles to, so `symbol_kind` is
  `class` and the module-ness travels as `fsharp_module`.
- **Function or value** is decided by what stands between the name and the `=` —
  parameters make it a function, nothing (or a lone type annotation) makes it a
  value. Testing for a `(` instead calls `let private RouteKey = "…"` a function,
  because the modifiers sit before the name and look like parameters.
- **A `let` inside a type is private state**, not a member, matching the C# and VB
  rules for private fields. A `let` indented past its enclosing member is a local
  binding and declares nothing.
- `.fs`, `.fsi` and `.fsx` are all read. `//` inside a string is not a comment, and
  `(* … *)` blocks nest.

### Free functions, and why they needed a fix elsewhere

F# is the only .NET language with genuine free functions — a module-level `let` is
not a method on anything. Two things followed from that:

The bare-call index used to hold **methods only**, so every F#-to-F# call was
dropped on the floor. It now holds functions too.

And references are harvested by NAME from the whole body, as the Razor scanner
does, rather than only in a few anchored positions. F# applies functions without
parentheses and in almost any position, and anchoring narrowly left **2,579 of
dotnet/fsharp's 12,430 functions with no inbound edge**. Widening it brought that
to 977.

> **Treat F# orphan findings as leads, not a cleanup list** — the same warning
> this page gives for C#, and for a sharper reason. `find_orphans` rates a
> `function` finding **high confidence** because plain calls are reliably tracked
> in the languages that model was built for. They are not reliably tracked here:
> this is a name-based scan of a language with no parenthesised call syntax. The
> confidence rating is inherited, not earned.

### F# results

| Repo | facts before | after | symbols before | after | regressed |
|---|---:|---:|---:|---:|---:|
| dotnet/fsharp | 8,009 | **68,379** | 5,578 | **45,571** | **0** |
| Giraffe | 25 | **641** | 0 | **439** | **0** |

dotnet/fsharp parsed 111 files of 10,519 before this — the 5,473 `.fs` sources
contributed nothing. Giraffe had only the project graph Phase 1 gave it.

*Not extracted:* Giraffe's routing DSL (`route`, `routef`, `subRoute`, `choose`).
It is the obvious next thing to read and the corpus cannot validate it —
`giraffe-fsharp/Giraffe` *defines* the DSL and every use of it lives in `tests/`,
which the ignore globs exclude for the same reason csharp-sdk's 31 test-only
endpoints were. Shipping an unexercised route parser would add a claim no
benchmark run can check.

## Persistence — EF Core, Dapper, MongoDB

`storage` was **zero in all fourteen .NET repositories** of the benchmark corpus
before this, bitwarden-server and eShop included, both EF Core products.

The hard part is that **EF Core entities carry no annotation**. Java looks for
`@Entity`; a C# entity is a plain class, and what makes it one is that some
DbContext elsewhere declares a `DbSet<T>` of it. Detection is therefore evidence
collected per file and resolved once the whole fact set exists — the same shape
`composeControllerRoutes` uses for an inherited `[Route]`.

```csharp
public class CatalogContext : DbContext                    // src/Catalog.API/Infrastructure
{
    public required DbSet<CatalogItem> CatalogItems { get; set; }
}

class CatalogItemEntityTypeConfiguration                   // …/EntityConfigurations
    : IEntityTypeConfiguration<CatalogItem>
{
    public void Configure(EntityTypeBuilder<CatalogItem> b) => b.ToTable("Catalog");
}
```

```
storage  src/Catalog.API/Infrastructure.CatalogContext  props: storage_kind=context,
                                                              framework=efcore
                                                       --depends_on--> …/Model.CatalogItem
storage  src/Catalog.API/Model.CatalogItem             props: storage_kind=entity,
                                                              framework=efcore, table=Catalog
```

An entity fact is named for the **symbol that declares the type**, not for the
directory that mentioned it: the `DbSet` is in `Infrastructure/` and the class is
in `Model/`, and naming the fact for the mentioning directory would mint a node
matching nothing. `ToTable` is the only place the physical table name appears.

Also read: Dapper's generic query methods (`QueryAsync<Cipher>`) and
`IMongoCollection<T>` name their row types, and a `Migration` subclass is recorded
as one.

### Two rules that stop it inventing things

**Match the base list, never the declaration text.** Testing for the substring
`DbContext` caught three things that are not one — a class merely *named*
`MigrateDbContextExtensions`, a generic *constraint* (`where TContext :
DbContext`), and a service whose type parameter is a context. eShop reported 8
contexts where it has 4.

**A duplicated short name resolves to the nearest declaration.** eShop declares
`CatalogItem` three times: the API's model, the mobile client's, and the Blazor
components'. The one in the context's own project is the table, so the candidate
sharing the most leading path segments with the declaring directory wins — the
storage counterpart of [resolution](#resolution--why-it-is-a-whole-repository-pass)
step 1. A genuine tie is still dropped: attributing a table to the wrong subsystem
is worse than attributing none. A type parameter (`DbSet<TEntity>` in a generic
base) and a row type the repository does not declare are dropped outright.

### Persistence results

| Repo | contexts | entities | migrations |
|---|---:|---:|---:|
| bitwarden-server | 1 | 60 | 452 |
| jellyfin | 1 | 65 | 51 |
| OrchardCore | 0 | 14 | 0 |
| eShop | 4 | 7 | 9 |

All were zero. OrchardCore has no EF Core context — it uses YesSql, whose
migrations derive from `DataMigration` rather than `Migration`, so they are
correctly absent rather than mislabelled `framework=efcore`.

*Not extracted:* a repository class is not itself a storage fact. C# has no
`@Repository` annotation and the `*Repository` name is a convention, not a
declaration — the entity and the context are what the code actually states.

## Outbound HTTP — what a service calls

Until this existed a C# service linked to its neighbours only as a route
**provider**: no `role=client` routes were emitted, so the cross-repo linker had
one side of every edge and the benchmark's cross-repo axis had no .NET answer.

The path is rarely a literal at the call site. eShop's CatalogService is
representative — the verb is on the call, the path is in a local, and the local is
an interpolation over a field:

```csharp
private readonly string remoteServiceBaseUrl = "api/catalog/";

var uri = $"{remoteServiceBaseUrl}items/{id}?api-version=2.0";
return httpClient.GetFromJsonAsync(uri, CatalogJsonContext.Default.CatalogItem);
```

```
route  /api/catalog/items/{id}   props: method=GET, role=client, framework=httpclient
```

So a literal environment is resolved **per member body**, with the type's fields
as the fallback. File-wide would be wrong for a reason the corpus shows plainly: a
class reuses the name `uri` in every method, and one map made every CatalogService
call resolve to whichever `uri` happened to be written last.

Three rules decide what a resolved argument is worth:

- **An interpolation hole becomes a path parameter**, keeping its name. That is
  what lets a client's `items/{id}` match a server template of the same shape;
  dropping the hole would produce `items/` and match nothing.
- **An absolute URL keeps only its path.** The host is deployment configuration,
  and under .NET Aspire it is a service *name* (`https+http://catalog-api`) rather
  than a hostname at all. Query strings go too — they are not part of a template.
- **At least one literal segment is required.** A path of only parameters (`/{x}`)
  would match every server template at that depth, so it names no endpoint. A bare
  host, and a path computed at run time, are dropped for the same reason.

`SendAsync` is deliberately absent from the verb table: it takes an
`HttpRequestMessage` whose verb is set elsewhere, so the call site cannot classify
it and guessing GET would invent an edge.

Refit interfaces are read too — `[Get("/api/orders/{id}")]` on an interface method
declares an outbound request the same way `[HttpGet]` declares an inbound one.

### Outbound HTTP results

Client routes emitted: eShop 8, bitwarden-server 13, jellyfin 0 (a media server
that serves rather than calls).

On the `bitwarden-server` + `bitwarden-clients` cluster:

| | detected | resolved | classification |
|---|---:|---:|---|
| before | 5 | 5 | `isolated` |
| after | 13 | 7 | `coverage_gap` |

The classification change is the more important half. `isolated` asserted that the
server calls nothing; `coverage_gap` says enola found calls it cannot place, which
is true and actionable.

*Not extracted yet:* SignalR hubs, gRPC clients and service implementations, Azure
Functions triggers, MassTransit consumers, .NET Aspire's AppHost topology, and
conventionally-routed MVC actions — OrchardCore registers those through
`MapAreaControllerRoute` in a `Startup`, and 94% of its controllers carry no
`[Route]`.

## Dependency injection is a reference

A .NET application calls almost everything through an interface, and the only
place the implementation is named is its registration:

```csharp
services.AddScoped<IOrderRepository, OrderRepository>();
services.AddHostedService<OrderProcessor>();
```

Without reading those, `OrderRepository` has no inbound edge at all and reads as
dead. Measured before this existed: **441 of bitwarden-server's 1,661 orphan
classes (27%) and 59 of eShop's 202 (29%)** were named in a registration and
nowhere else.

This is a fix to the **graph**, not to a confidence heuristic — the registration is
a real reference that was simply not being read, rather than a false positive to be
explained away afterwards.

The registrar list is explicit rather than an `Add*` match. A generic
`AddRange<T>` or a builder's `AddPolicy<T>` names no service, and treating every
`Add` method as a registration would draw an edge from a startup file to whatever
type happened to be passed anywhere in the application.

| Repo | orphans before | after | rescued | regressed |
|---|---:|---:|---:|---:|
| OrchardCore | 6,494 | **5,309** | 1,185 | **0** |
| bitwarden-server | 5,149 | **4,292** | 857 | **0** |
| jellyfin | 4,245 | **4,041** | 204 | **0** |
| eShop | 819 | **753** | 66 | **0** |

No symbols added, so the totals fall directly.

## Layers — the `dotnet-clean` pattern

.NET names a project `<Product>.<Layer>` — `Ordering.Domain`, `Catalog.API` — so
the layer is the last dot-separated component of a path segment, never the whole
segment. Matching is therefore opt-in per pattern: a dotted directory in another
ecosystem (`cal.com`, `foo.web`) would otherwise start matching layers it has
nothing to do with.

Two things keep it from firing on repositories that are not clean architecture:

**Only the four layers whose dependency direction the pattern actually fixes.**
`.Abstractions`, `.Shared`, `.Common` and `.Core` name a shared kernel that sits at
no particular level. Giving them one produced **~230 "violations" on OrchardCore**,
a modular CMS that never claimed to be clean architecture — a worse failure than
the weak `hexagonal` match it replaced.

**A violation needs to cross an assembly boundary.** Two directories inside one
project compile into the same DLL, which is why `cycles` already says an
intra-assembly cycle is a coupling signal rather than a build problem. Without the
gate, eShop reported `Basket.API/Repositories -> Basket.API/Model` as
`infrastructure -> api`, because a sub-directory takes its layer from its own name
while its sibling inherits one from the project name they share.

| Repo | pattern | violations |
|---|---|---:|
| eShop | dotnet-clean (0.71) | 0 |
| bitwarden-server | dotnet-clean (0.47) | 0 |
| OrchardCore | dotnet-clean (0.42) | 0 |
| jellyfin | dotnet-clean (0.39) | 0 |
| roslyn | *(none)* | 0 |

eShop previously matched `hexagonal` at 0.48 by reading directory names *inside*
projects while the assembly graph went unused. roslyn matching nothing is correct:
it is a compiler, not a layered application. Other ecosystems are unaffected —
thingsboard still reports 75 violations and nowinandroid 4, unchanged.

## Names, namespaces and the two spellings

enola names a fact by its **directory**, as it does in every language — `<dir>.<Type>`,
`<dir>.<Outer>.<Inner>`, `<dir>.<Type>.<member>`. A C# namespace routinely disagrees with
the file system, so it travels as a prop rather than as part of the name:

```csharp
// src/Acme.Api/OrderService.cs
namespace Acme.Api;                 // file-scoped

public sealed class OrderService : IDisposable { … }
```

```
symbol  src/Acme.Api.OrderService   props: symbol_kind=class, exported=true, sealed=true,
                                          namespace=Acme.Api, fqn=Acme.Api.OrderService
                                    --implements--> IDisposable
```

Both spellings — the file-scoped `namespace A.B;` and the block `namespace A.B { … }` —
produce the same `namespace` prop. That matters because resolution keys on it.

## Partial types are one type

C# lets a type be split across any number of files, and real codebases lean on it hard:
7,740 files in the benchmark corpus declare one. Each half is its own parse tree, so
without a merge each becomes its own fact under the same name — inflating the symbol
count, scattering the type's edges across facts that each look thinly connected, and
handing every consumer an arbitrary one of the halves as the type's `file:line`.

```csharp
// src/Acme.Domain/Widget.Core.cs        // src/Acme.Domain/Widget.Extra.cs
public partial class Widget              public partial class Widget : IOrderRepository
{                                        {
    public void Reset() => Describe();       public void Describe() { }
}                                        }
```

```
symbol  src/Acme.Domain.Widget          props: partial=true, partial_declarations=2
                                        --implements--> src/Acme.Domain.IOrderRepository
symbol  src/Acme.Domain.Widget.Reset    --calls--> src/Acme.Domain.Widget.Describe
symbol  src/Acme.Domain.Widget.Describe
```

The surviving fact is the earliest by `(file, line)`, so the merge is a function of the
fact set rather than of the order the files happened to be walked.

`Reset --calls--> Describe` is the part a per-file index cannot produce: the two members
are in different trees. Inside a partial type a bare call is therefore offered
speculatively against **that type's own member namespace**, and dropped if no half
declares it. The candidate is scoped to one type rather than to the repository, so the
worst case is a base-class call that draws no edge — never an edge to an unrelated type
that happens to share a method name.

## Resolution — why it is a whole-repository pass

Java can resolve a bare type reference from the file's own imports, because a Java
`import` names a type. A C# `using` opens a **namespace**, so the file's directives say
which namespaces are in scope and nothing about which one declares `StringBuilder`.

Bare names are therefore bound against a project-wide index, in this order:

1. a declaration in the reference's **own namespace** — C#'s own rule, not a tie-break;
2. a **unique** simple name anywhere in the repository;
3. otherwise **nothing**, and the target is left as written.

Step 3 is the load-bearing one. On a corpus this size `Options`, `Message` and `Result`
each name dozens of unrelated types, and picking one would manufacture an edge between
subsystems that have never heard of each other.

Measured on [dotnet/eShop](https://github.com/dotnet/eShop), the unresolved remainder is
almost entirely BCL and framework surface — `Task.FromResult`, `Guid.NewGuid`,
`JsonSerializer.Deserialize`, `IRequestHandler`, `Migration` — which is what "left as
written" is supposed to look like.

An **alias** (`using Repo = Acme.Data.OrderRepository;`) and a `using static` are the two
forms that *do* name a type; both resolve through the type index, and the alias is
substituted at every reference in that file.

## Dependency injection

```csharp
public OrderService(IOrderRepository repo, HttpClient http) { … }
```

```
symbol  src/Acme.Api.OrderService  --injects--> src/Acme.Domain.IOrderRepository
                                   --injects--> HttpClient
```

Injection is read from a **sole** constructor, or from a C# 12 primary constructor
(`class Foo(IBar bar)`). A type with several constructors has convenience overloads and
no single injection point, so none of them produce edges — the same restriction the Java
extractor applies, for the same reason.

For a `record`, the primary constructor's parameters are additionally emitted as the
public properties the compiler generates from them.

## Routes — ASP.NET Core attribute routing

An action's URL is assembled from two attributes in two places, and frequently from
a third the file cannot see:

```csharp
// src/Acme.Api/BaseApiController.cs          // src/Acme.Api/Controllers/AudioController.cs
[ApiController]                                public class AudioController : BaseApiController
[Route("[controller]")]                        {
public class BaseApiController : ControllerBase    [HttpGet("{itemId}/stream", Name = "GetAudioStream")]
{ }                                                public IActionResult GetAudioStream(string itemId) => Ok();
                                               }
```

```
route  /Audio/{itemId}/stream   props: method=GET, framework=aspnetcore,
                                       handler=src/Acme.Api/Controllers.AudioController.GetAudioStream
                                --handled_by--> src/Acme.Api/Controllers.AudioController.GetAudioStream
```

Most controllers declare no `[Route]` of their own — 40 of jellyfin's 64 inherit
one from a shared base — so composition runs over the whole fact set rather than
per file, walking the resolved inheritance edges to find the nearest template.
`[controller]` then resolves to each subclass's own name minus the `Controller`
suffix, which is how one shared base attribute gives 40 controllers 40 distinct
paths. `[action]` resolves to the method name.

A method template beginning with `/` or `~/` is **absolute**: it replaces the
controller's template rather than extending it. `Name = "…"` is a route's display
name, not its path — only an attribute's first argument is a template, and only
when it is a string literal.

Measured on [jellyfin](https://github.com/jellyfin/jellyfin): 422 routes, exactly
one per `[HttpGet]`/`[HttpPost]`/`[HttpDelete]`/`[HttpHead]` attribute in the
source, with all 388 distinct handlers resolving to real symbol facts.

### Conventional routing, when the registration can be read

A controller with verb attributes but **no `[Route]` anywhere in its hierarchy** is
using *conventional* routing, where the URL comes from a template registered in a
startup file:

```csharp
public class AccountController : Controller     // really served at /Account/Login
{
    [HttpGet]  public IActionResult Login()  => View();
    [HttpPost] public IActionResult Logout() => View();
}
```

Composing from the action alone is what this used to do, and it was wrong twice
over: a bare `[HttpGet]` carries no template, so every action came out as `/` — the
wrong path, and, because facts are name-keyed, several actions collapsing onto a
single root node. On eShop's Identity.API that turned 14 real endpoints into a
handful of phantom roots.

The registration itself **is** read now, which is what makes those URLs safe to
emit:

```csharp
routes.MapAreaControllerRoute(
    name: "ListFeed",
    areaName: "OrchardCore.Feeds",
    pattern: "Contents/Lists/{contentItemId}/rss",
    defaults: new { controller = "Feed", action = "Index" });
```

```
route  /Contents/Lists/{contentItemId}/rss   props: routing=conventional, method=GET,
                                                    controller=Feed, action=Index
                                             --handled_by--> …Controllers.FeedController.Index
```

The template is routinely a `const` field rather than an inline string —
OrchardCore's default route is exactly that — so it resolves through the same
literal environment the [client scan](#outbound-http--what-a-service-calls) builds.
`areaName` and the `defaults` controller/action are substituted into their tokens.

**A template still generic after substitution is not emitted.** OrchardCore's
default is `{area}/{controller}/{action}/{id?}`, and expanding it needs each
controller's area — from an `[Area]` attribute or a module convention this
extractor does not read. A route at a literal `/{area}/…` would be a URL the
application never serves. The count left generic is logged, so the gap stays
visible rather than silent.

The method is `GET`: a registration declares a URL, not a verb, and GET is what
ASP.NET serves for an action with no verb attribute. A controller name that two
declarations share binds no handler — the route still stands, only the binding is
withheld.

Measured on OrchardCore: **45 routes before, 82 after** — 30 registrations
resolved, 20 left generic. jellyfin is unchanged at 420, which is the control:
attribute routing must not move.

Note that `[Route("")]` is **not** the same as declaring no `[Route]`: an empty
template is a real one, and its actions are served at the method template alone.

## Routes — minimal APIs

A minimal-API endpoint is registered by a call rather than declared by an
attribute, and its path is split between a group builder held in a local variable
and the registrations that use it:

```csharp
public static RouteGroupBuilder MapOrdersApiV1(this IEndpointRouteBuilder app)
{
    var api = app.MapGroup("api/orders").HasApiVersion(1.0);

    api.MapPut("/cancel", CancelOrderAsync);
    api.MapGet("{orderId:int}", GetOrderAsync);

    var admin = api.MapGroup("admin");
    admin.MapDelete("/{orderId:int}", DeleteOrderAsync);
}
```

```
route  /api/orders/cancel                 props: method=PUT,    handler=…OrdersApi.CancelOrderAsync
route  /api/orders/{orderId:int}          props: method=GET,    handler=…OrdersApi.GetOrderAsync
route  /api/orders/admin/{orderId:int}    props: method=DELETE, handler=…OrdersApi.DeleteOrderAsync
```

Groups nest, and a fluent call after `MapGroup` does not hide the prefix. The
analysis is scoped to **one method body** — which is what makes it resolvable
without a whole-program pass, and equally what bounds it: a group passed across a
function boundary is not followed. A group variable in one method never resolves a
registration in another.

A **method-group** handler (`GetOrderAsync`) binds to the sibling method it names;
a **lambda** has no symbol to point at, so the route carries no `handler` at all
rather than a fabricated one.

A **string-literal first argument** is the discriminator. That is what keeps
`MapControllers()`, `MapRazorPages()` and `MapHub<T>()` — none of which take a
path — out of the route set, without a list of method names to exclude, and it
also excludes a computed path (`app.MapGet(BuildPath(), H)`) that cannot be read.

### An unresolvable group prefix drops its routes

A library may mount its whole surface at a prefix its caller supplies:

```csharp
public static IEndpointConventionBuilder MapMcp(this IEndpointRouteBuilder endpoints,
                                                string pattern = "")
{
    var mcpGroup = endpoints.MapGroup(pattern);   // ← not a literal
    var streamable = mcpGroup.MapGroup("");

    streamable.MapPost("", HandlePostAsync);
    streamable.MapGet("/sse", HandleSseAsync);
}
```

The real paths are whatever the host application passes, so the group is marked
**unknown** and its registrations emit nothing. Publishing the registration path
alone would claim endpoints the library does not serve — and since the first
registers at `""`, it would land on `/`, recreating the phantom-root collapse
[conventional routing](#conventional-routing-when-the-registration-can-be-read) once produced.

This is the one place the C# extractor is stricter than the Go extractor, which
keeps a bare path when a mount is unresolved. A bare Go path is still a
recognisable suffix; a bare path here is frequently the root itself.

Measured on [eShop](https://github.com/dotnet/eShop): 30 routes, all with composed
prefixes; the MCP SDK's caller-mounted file contributes 0.

## Dead code, and why the bare edge matters

A DI-wired .NET application calls almost everything through an interface. With a
same-type-only call graph, every method reached that way has no inbound edge at
all — so it reads as dead, and so does the interface member it implements.

Measured on jellyfin, before and after the bare member-call edge:

| | methods | enums | constants | all symbols |
|---|---:|---:|---:|---:|
| same-type edges only | 2,478 | 105 | 802 | 7,485 / 15,665 |
| + bare member-call edges | 988 | 105 | 802 | 5,982 |
| + qualified references | **905** | **8** | **179** | **4,576** |

A second gap sat beside the first: only *invocations* emitted edges, so a plain
`VideoRange.HDR` produced nothing at all and 84 of 137 enums read as isolated
while that exact expression appeared at 25 call sites. A qualified `Type.Member`
reference now emits an edge to the member **and** to the type — the member edge
alone vouches for `HDR` and says nothing about `VideoRange`, which left every
class reached only through static calls unreferenced too.

The type edge is added once the receiver has *provably* resolved to a declared
type, not at the call site: a bare type name emitted there would be
indistinguishable from the bare method name a member call produces, and
`foo.Order()` would bind to a class named `Order`. Receivers are gated on
PascalCase, C#'s type-naming convention, so `order.Id` and `list.Count` emit
nothing.

2,909 symbols rescued in total, at +49% `calls` edges.

What remains flagged is largely **not** a false positive, and splits in two.
jellyfin discovers its providers by reflection (`GetExports<IImageProvider>()`),
so a class like `ComicImageProvider` genuinely has no static reference anywhere in
the tree. And a type used only in *type position* — `ViewType Kind { get; set; }`,
`List<ViewType>` — is not edge-tracked in any language enola supports, which is
what the last 8 enum findings are. Both are limitations the orphan detector's own
caveat already describes.

Note also that `find_orphans` reports **no** high-confidence findings for C#. Its
confidence model rates plain `function` calls as reliably tracked, and C# has
almost no free functions — everything is a method, which it rates `low`. Treat C#
orphan output as leads to verify, not a cleanup list.

## Complexity and I/O

Like the other AST extractors, each member body is walked once for `cyclomatic`,
`loop_depth`, `loop_count`, `calls_in_loop`, `recursive_self`, plus `scaling_loop_depth`
and `calls_in_scaling_loop`. `for`/`foreach`/`while`/`do` and LINQ iterator lambdas
(`Select`, `Where`, `ForEach`, `Aggregate`, …) count as loops; a literal-bounded `for
(var i = 0; i < 3; i++)` or a `foreach` over a collection literal raises `loop_depth` but
adds **no** scaling depth, so a fixed-size loop never inflates a genuine O(n) into a
false O(n²).

A member that calls a network, file or database primitive — `HttpClient`'s verbs,
`File.*`, ADO.NET `Execute*`, EF Core `SaveChangesAsync`/`ToListAsync` — is tagged
`io_direct`, and a post-pass propagates that transitively over the call graph into
`performs_io`. That is what lets a per-iteration call to a wrapper read as an N+1 rather
than as ordinary work.

`&&`, `||` and `??` each add a decision point. `?.` does not, matching the other
extractors' treatment of optional chaining.

A loop's **iterable is evaluated in the enclosing scope**, so it is not counted as
nested work. `foreach (var x in items.Where(p))` enumerates once — the lambda runs
per element of `items`, which is the same *n* the loop itself runs, not *n* per
iteration — so it is O(n), not O(n²). LINQ puts a `Where`/`Select`/`OrderBy` in
the iterable position of a great many `foreach` statements, and counting it inside
inflated the estimate for all of them. A `for` initializer runs once for the same
reason; its condition and update genuinely repeat and stay inside.

## Test projects

Three directory patterns keep test code out of the production graph:

```
**/tests/**/*.cs      **/test/**/*.cs      **/*.Tests/**/*.cs
```

`*.Tests/` is not redundant with `tests/`: the dominant .NET solution layout puts a
test project in `MyApp.Tests/` *beside* `MyApp/` rather than under a `tests/`
directory, and 303 files in the benchmark corpus are reachable only that way.

**There is deliberately no filename pattern**, and that is measured rather than
cautious. Across 40,014 `.cs` files in the corpus, `**/*Tests.cs` would have added
exactly **one** file the directory rules miss — `emitUnitTests.cs`, a generator
that *emits* unit tests — while `**/*Test.cs` would have deleted
`XmlQualifiedNameTest` (a real XPath node-test type in `System.Private.Xml`) and
the Azure Load Testing tool's `LoadTest/Test.cs` model. That is the same hazard
Ruby's `_test.rb` suffix presents, and it resolves the same way: the directory is
the signal, the filename is not.

What this changes, measured:

| Repo | facts before | after | routes before | after |
|---|---:|---:|---:|---:|
| dotnet/runtime | 881,581 | **372,992** | 23 | 17 |
| csharp-sdk | 9,156 | 4,122 | **31** | **0** |
| mcp | 32,844 | 21,228 | 0 | 0 |
| jellyfin | 30,012 | 26,882 | 422 | 420 |
| eShop | 3,080 | 3,156 | 30 | 30 |

csharp-sdk is the case that motivated it: every one of the 31 endpoints enola
reported for it came from `tests/`, and the library serves no HTTP surface of its
own. jellyfin keeps its real 420-endpoint API and loses two test-only routes.

*Limits:* a test tree under a name these patterns do not know — dotnet/runtime's
`src/mono/wasm/testassets/` — is still indexed, and a test-support library that
happens to ship (`Microsoft.Extensions.DependencyInjection.Specification.Tests`) is
excluded along with real tests.

### …but what a test *references* is kept

Excluding a test project has a cost: a production symbol whose only caller is a
test then has no inbound edge at all, and reads as dead. So test files are parsed
once more, for the sole purpose of capturing their outbound references:

```csharp
// tests/Acme.Api.Tests/OrderServiceTests.cs
var svc = new OrderService(repo);
var money = svc.Summarise(orders);
Assert.Equal("EUR", money.Currency);
```

```
test_ref  tests/Acme.Api.Tests/OrderServiceTests.cs
            --calls--> OrderService     --calls--> Summarise
            --calls--> svc.Summarise    --calls--> money.Currency
```

One `test_ref` fact per file, carrying **only** `calls` relations — no symbols, no
modules, no routes. So a test class still never becomes a dead-code candidate, and
no symbol/module/route explainer sees test code at all.

Targets are emitted **as written** rather than resolved to canonical fact names.
The production symbol index is built inside `Extract` and is not available to this
pass, and the consumer does not need it: the orphan detector matches a target both
exactly and by its last dot-separated segment, which is why the Ruby extractor
emits the same `Const.method` shape.

Assertion and mocking receivers (`Assert`, `Mock`, `It`, `Times`, …) are dropped,
**including the bare method name**. Filtering only `Assert.Equal` let `Equal`
through — and production code really does declare `Equal`, so the harness would
have vouched for a symbol no test exercises, suppressing a genuine dead-code
finding.

| Repo | `test_ref` facts | distinct targets | matching a production symbol |
|---|---:|---:|---:|
| jellyfin | 210 | 2,796 | 1,974 (71%) |
| csharp-sdk | 261 | 4,032 | 2,191 (54%) |
| eShop | 34 | 330 | 170 (52%) |

The remainder are BCL and framework calls (`Activator.CreateInstance`,
`AddLogging`, `AddDays`) that match nothing and cost nothing.

*Cost:* the pass parses every test file. On dotnet/runtime — 16,974 of them — it
adds roughly 23s to a 20s snapshot.

## Generated code

`.g.cs`, `.generated.cs`, `.Designer.cs`, `AssemblyInfo.cs` and anything carrying an
`<auto-generated>` header produce **nothing** — not an empty symbol set, nothing at all,
so a directory holding only generated files emits no module either. The count of skipped
files is logged rather than silently absorbed.

Generated code is a projection of something else — a `.resx`, a `.xaml`, an interop
definition — and indexing it attributes thousands of symbols and a large complexity
surface to whichever directory the generator wrote into. Those findings are also
unactionable: the fix lives in the generator, and the generator is not in the graph.

`obj/` and `bin/Debug|Release/` are excluded by the default ignore globs for the same
reason.

## What is deliberately not extracted

- **Private fields and properties are not symbols.** They are a type's internal state,
  and on a BCL-scale repository emitting them would multiply the symbol count without
  adding a node anyone traverses. Their initializers and accessor bodies are still walked,
  so the call edges inside them survive, attributed to the enclosing type.
- **A call on an arbitrary receiver draws a BARE edge, never a bound one.** `foo.Handle()`
  emits `calls -> Handle` — the method name alone — because the receiver's static type is
  not tracked. It is kept bare even when exactly one type declares that name: jellyfin has
  a single `Match` method (`FileStackRule.Match`) while four of its five variable-receiver
  `.Match(` call sites are `Regex`, so binding on uniqueness would point them into the
  video-stack parser, and a wrong edge feeds `impact_analysis` and `find_path`. Nothing is
  lost — the dead-code detector matches by short name, and the rescue measures identical
  either way. A name no type in the repository declares is dropped, so `.ToString()` and
  `.Add()` do not become edges to symbols that do not exist. Same-type calls (`Foo()`,
  `this.Foo()`) and static calls (`Type.Method()`) still resolve to canonical targets.
- **Extension methods record their receiver but bind no call site.** A
  `static bool IsOpen(this Order o)` carries `extension_method` and `extends_type=Order`;
  an `order.IsOpen()` call site needs the receiver's static type to bind, so it does not.
- **Same-named types in different directories are never merged**, including partial ones.
  The name is directory-anchored, so they are different nodes by construction — the same
  limitation the C/C++ header/source merge has.
- **EF Core storage and outbound HTTP clients are not extracted yet.** There are no
  `storage` facts and no client-role routes for the cross-repo linker to match, so a C#
  service links to its neighbours only as a route *provider*.
- **A minimal-API group is not followed across a function boundary**, and a group whose
  prefix is not a string literal drops its routes entirely — see above.
- **Conventionally-routed controller actions mint no route** — see above.
- **A test's references are captured by name, not resolved.** A `test_ref` target
  matches a production symbol by its last segment, so an unrelated symbol sharing a
  method name is credited with test coverage it does not have. That biases toward a
  *missed* dead-code lead rather than a false accusation, which is the safe direction.
