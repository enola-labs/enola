package dotnetextractor

// MSBuild project system — the .NET assembly graph.
//
// Everything else in this package reads `.cs` and names its facts after the
// DIRECTORY they live in. That is the right unit for a symbol, and the wrong one
// for a dependency: .NET's dependency unit is the assembly, declared by a project
// file, and a `ProjectReference` between two of them is the only edge in a .NET
// solution that the build system itself enforces. Until this file existed the
// project was read from its path alone (see projectInfo) and those edges were not
// in the graph at all, so `cycles` and `package_metrics` judged .NET at directory
// granularity while MSBuild judged it at assembly granularity.
//
// Parsed for every MSBuild language, not just C#. A `.fsproj` declares the same
// references as a `.csproj`, and reading it is what lets an F#-only repository
// produce its project graph before an F# source extractor exists.

import (
	"encoding/xml"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// projectLanguages maps an MSBuild project extension to the language prop its
// module carries. The extension is the only reliable signal: an SDK-style project
// names no language, and the sources it globs may not have been walked yet.
var projectLanguages = map[string]string{
	".csproj": "csharp",
	".fsproj": "fsharp",
	".vbproj": "vbnet",
}

// isProjectFile reports whether rel is an MSBuild project of any .NET language.
func isProjectFile(relFile string) bool {
	_, ok := projectLanguages[strings.ToLower(filepath.Ext(relFile))]
	return ok
}

// isSolutionFile reports whether rel is an MSBuild solution, in either the legacy
// `.sln` text format or the current `.slnx` XML one.
func isSolutionFile(relFile string) bool {
	switch strings.ToLower(filepath.Ext(relFile)) {
	case ".sln", ".slnx":
		return true
	}
	return false
}

// nugetRef is one PackageReference: an external dependency the project DECLARES,
// as opposed to the ones inferred from `using` directives. The two disagree
// usefully — a package referenced but never used, or used only through a
// transitive dependency, is visible only by comparing them.
type nugetRef struct {
	id      string
	version string
}

// msbuildProject is one parsed project file.
type msbuildProject struct {
	file       string   // repo-relative path to the project file
	dir        string   // its directory, which is the module name
	assembly   string   // AssemblyName, defaulting to the file's base name
	language   string   // from the extension, via projectLanguages
	refs       []string // directories of referenced projects, repo-relative
	packages   []nugetRef
	tfm        string
	outputType string
	isTest     bool
	solution   string // the solution that lists it, if any
}

// parseProjectFile reads one MSBuild project.
//
// Token-walked rather than unmarshalled into a struct, because the two project
// formats disagree about namespaces: an SDK-style project has none, while a
// legacy one puts every element in the 2003 MSBuild namespace. Matching on
// Name.Local reads both without carrying two sets of structs, and ignores the
// large majority of elements neither format guarantees.
//
// CONDITIONS ARE IGNORED. A `<ProjectReference Condition="'$(TargetFramework)'
// == 'net48'">` is a real reference under some build configuration, and enola has
// no configuration to evaluate against. Taking every branch over-approximates the
// edge set, which is the safe direction here: a missing edge hides a real
// dependency, while an extra one describes a build that genuinely exists.
func parseProjectFile(repoPath, relFile string) *msbuildProject {
	f, err := os.Open(filepath.Join(repoPath, relFile))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	rel := filepath.ToSlash(relFile)
	p := &msbuildProject{
		file:     rel,
		dir:      path.Dir(rel),
		assembly: strings.TrimSuffix(path.Base(rel), path.Ext(rel)),
		language: projectLanguages[strings.ToLower(path.Ext(rel))],
	}

	dec := xml.NewDecoder(f)
	// Legacy project files are occasionally written in a non-UTF-8 encoding and
	// declare it; without a charset reader the decoder stops at the declaration.
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	cur := ""   // local name of the element whose chardata is being read
	inPkg := -1 // index into p.packages while inside a PackageReference element
	for {
		tok, err := dec.Token()
		if err != nil {
			break // includes io.EOF; a truncated project yields what was read so far
		}
		switch t := tok.(type) {
		case xml.StartElement:
			cur = t.Name.Local
			switch cur {
			case "ProjectReference":
				if inc := attr(t, "Include"); inc != "" {
					if d := resolveProjectRef(p.dir, inc); d != "" {
						p.refs = append(p.refs, d)
					}
				}
			case "PackageReference":
				id := attr(t, "Include")
				if id == "" {
					id = attr(t, "Update") // the form used to pin a transitively-supplied version
				}
				if id != "" {
					p.packages = append(p.packages, nugetRef{id: id, version: attr(t, "Version")})
					inPkg = len(p.packages) - 1
				}
			}
		case xml.CharData:
			v := strings.TrimSpace(string(t))
			if v == "" {
				break
			}
			switch cur {
			case "AssemblyName":
				p.assembly = v
			case "TargetFramework", "TargetFrameworks":
				p.tfm = v
			case "OutputType":
				p.outputType = v
			case "IsTestProject":
				p.isTest = p.isTest || strings.EqualFold(v, "true")
			case "Version":
				// <PackageReference Include="X"><Version>1.2.3</Version></PackageReference>
				if inPkg >= 0 && p.packages[inPkg].version == "" {
					p.packages[inPkg].version = v
				}
			}
		case xml.EndElement:
			if t.Name.Local == "PackageReference" {
				inPkg = -1
			}
			cur = ""
		}
	}

	// The SDK marks a test project by referencing the test SDK, which is a far more
	// common signal than the explicit IsTestProject property.
	for _, pkg := range p.packages {
		if pkg.id == "Microsoft.NET.Test.Sdk" {
			p.isTest = true
			break
		}
	}
	return p
}

// attr returns the named attribute, case-insensitively — MSBuild accepts either
// spelling and hand-edited legacy projects use both.
func attr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if strings.EqualFold(a.Name.Local, name) {
			return a.Value
		}
	}
	return ""
}

// resolveProjectRef turns a project-relative Include into a repo-relative
// directory. MSBuild writes these with BACKSLASHES regardless of host platform,
// so they are not path-separator-portable and must be normalised before any
// path operation.
func resolveProjectRef(fromDir, include string) string {
	inc := strings.ReplaceAll(include, `\`, "/")
	if !isProjectFile(inc) {
		return ""
	}
	joined := path.Join(fromDir, path.Dir(inc))
	// A reference reaching above the repository root cannot be part of this graph.
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return ""
	}
	return joined
}

// ── Solutions ───────────────────────────────────────────────────────────────

// slnProjectLine matches the legacy `.sln` project entry, whose second quoted
// field is the project path:
//
//	Project("{FAE04EC0-...}") = "Acme.Api", "src\Acme.Api\Acme.Api.csproj", "{...}"
var slnProjectLine = regexp.MustCompile(`(?m)^Project\("\{[^}]*\}"\)\s*=\s*"[^"]*",\s*"([^"]+)"`)

// parseSolution returns the repo-relative directories of the projects a solution
// lists. Both formats are read: `.slnx` is XML with a Path attribute per project,
// while `.sln` is a bespoke text format that predates it.
//
// A solution contributes GROUPING only — which assemblies ship together — never
// an edge. Two projects in one solution have no dependency by virtue of that
// alone, and drawing one would connect every project in a 148-project monorepo to
// every other.
func parseSolution(repoPath, relFile string) []string {
	data, err := os.ReadFile(filepath.Join(repoPath, relFile))
	if err != nil {
		return nil
	}
	base := path.Dir(filepath.ToSlash(relFile))

	var out []string
	add := func(include string) {
		inc := strings.ReplaceAll(include, `\`, "/")
		if !isProjectFile(inc) {
			return // solution folders and non-MSBuild entries
		}
		d := path.Join(base, path.Dir(inc))
		if d != ".." && !strings.HasPrefix(d, "../") {
			out = append(out, d)
		}
	}

	if strings.EqualFold(filepath.Ext(relFile), ".slnx") {
		dec := xml.NewDecoder(strings.NewReader(string(data)))
		dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "Project" {
				add(attr(se, "Path"))
			}
		}
		return out
	}

	for _, m := range slnProjectLine.FindAllStringSubmatch(string(data), -1) {
		add(m[1])
	}
	return out
}

// ── Fact emission ───────────────────────────────────────────────────────────

// parseProjects reads every project file and attributes each to the solution
// that lists it, if any. A project listed by several solutions keeps the first by
// path order, so the prop is a function of the fact set rather than of walk order.
// Read SERIALLY, deliberately. Parsing these through parallel.MapFiles was tried
// and is measurably slower — on roslyn's 351 project files the extract phase went
// from 8.05s to 9.21s across repeated cold runs. Concurrent reads of many small
// files defeat sequential readahead, and encoding/xml's allocation rate turns the
// fan-out into GC pressure. The source walk parallelises because a tree-sitter
// parse is CPU-bound; this is neither.
func parseProjects(repoPath string, projFiles, slnFiles []string) []*msbuildProject {
	out := make([]*msbuildProject, 0, len(projFiles))
	byDir := make(map[string]*msbuildProject, len(projFiles))
	for _, rel := range projFiles {
		p := parseProjectFile(repoPath, rel)
		if p == nil {
			continue
		}
		out = append(out, p)
		if _, dup := byDir[p.dir]; !dup {
			byDir[p.dir] = p
		}
	}

	sorted := append([]string(nil), slnFiles...)
	sort.Strings(sorted)
	for _, rel := range sorted {
		name := strings.TrimSuffix(path.Base(filepath.ToSlash(rel)), path.Ext(rel))
		for _, dir := range parseSolution(repoPath, rel) {
			if p, ok := byDir[dir]; ok && p.solution == "" {
				p.solution = name
			}
		}
	}
	return out
}

// applyMSBuild overlays the assembly graph onto the per-directory module facts.
//
// A project root that also holds sources must produce ONE module fact, not two.
// Facts are name-keyed, so two would be merged by the graph anyway — but they
// would both land in facts.jsonl and be counted twice in fact_count, which is a
// published benchmark number. Merging here keeps the count honest.
func applyMSBuild(mods map[string]*facts.Fact, projects []*msbuildProject) []facts.Fact {
	var deps []facts.Fact
	for _, p := range projects {
		f, ok := mods[p.dir]
		if !ok {
			// A project whose sources this extractor cannot read yet (.fsproj,
			// .vbproj) has no source-directory module. Emitting it anyway is the
			// point: the assembly graph does not depend on reading the sources.
			f = &facts.Fact{
				Kind:  facts.KindModule,
				Name:  p.dir,
				File:  p.file,
				Props: map[string]any{facts.PropModuleRole: facts.ModuleRoleForPath(p.dir)},
			}
			mods[p.dir] = f
		}
		if f.Props == nil {
			f.Props = map[string]any{}
		}
		// The project file names the language authoritatively; a directory holding
		// .cs beside an .fsproj is the rare mixed case, and the sources win there
		// because they are what produced the symbols.
		if _, has := f.Prop("language"); !has {
			f.SetProp("language", p.language)
		}
		f.SetProp("project", p.assembly)
		f.SetProp("msbuild", true)
		if p.tfm != "" {
			f.SetProp("target_framework", p.tfm)
		}
		if p.outputType != "" {
			f.SetProp("output_type", p.outputType)
		}
		if p.solution != "" {
			f.SetProp("solution", p.solution)
		}
		// MSBuild's own answer outranks the path heuristic: a test project named
		// Foo.UnitTests/ under src/ is a test project whatever its path suggests.
		if p.isTest {
			f.SetProp(facts.PropModuleRole, facts.ModuleRoleTest)
		}

		seen := make(map[string]bool, len(p.refs))
		for _, ref := range p.refs {
			if ref == p.dir || seen[ref] {
				continue // a self-reference is not an edge; duplicates come from Conditions
			}
			seen[ref] = true
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: ref})
		}

		pkgSeen := make(map[string]bool, len(p.packages))
		for _, pkg := range p.packages {
			if pkgSeen[pkg.id] {
				continue
			}
			pkgSeen[pkg.id] = true
			props := map[string]any{
				"language":        p.language,
				"import":          pkg.id,
				"source":          "external",
				"package_manager": "nuget",
			}
			if pkg.version != "" {
				props["version"] = pkg.version
			}
			deps = append(deps, facts.Fact{
				Kind:      facts.KindDependency,
				Name:      p.dir + " -> " + pkg.id,
				File:      p.file,
				Props:     props,
				Relations: []facts.Relation{{Kind: facts.RelImports, Target: pkg.id}},
			})
		}
	}
	return deps
}
