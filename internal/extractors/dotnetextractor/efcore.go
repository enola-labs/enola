package dotnetextractor

// Persistence — EF Core, Dapper and MongoDB.
//
// Before this, `storage` was ZERO in all fourteen .NET repositories of the
// benchmark corpus, bitwarden-server and eShop included, both of which are EF Core
// products. There was no persistence in any .NET graph enola produced.
//
// The hard part is that EF Core entities carry NO ANNOTATION. Java can look for
// `@Entity`; a C# entity is a plain class, and what makes it an entity is that
// some DbContext elsewhere declares a `DbSet<T>` of it, or an
// `IEntityTypeConfiguration<T>` configures it. Detection is therefore evidence
// collected per file and resolved once the whole fact set exists — the same shape
// composeControllerRoutes uses for `[Route]` inherited from a base class.

import (
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// storageDecl is one type that IS a storage construct.
type storageDecl struct {
	name string // canonical fact name
	dir  string
	file string
	line int
	kind string // context | migration
	// entities named by this declaration, as written (bare type names).
	entities []string
}

// entityEvidence is what one file learned about an entity type.
type entityEvidence struct {
	framework string // efcore | dapper | mongo
	table     string // from ToTable("…"), when present
	// Directory of the declaration that named this entity. Used to disambiguate a
	// short name declared several times: eShop declares CatalogItem three times —
	// the API model, the mobile client's model and the Blazor components' model —
	// and only the one in the context's own project is the table.
	fromDir string
}

type storageScaffold struct {
	decls    []storageDecl
	entities map[string]entityEvidence
}

func (s *storageScaffold) noteEntity(name, framework, table, fromDir string) {
	if name == "" || !isPlausibleEntityName(name) {
		return
	}
	if s.entities == nil {
		s.entities = map[string]entityEvidence{}
	}
	e := s.entities[name]
	if e.fromDir == "" {
		e.fromDir = fromDir
	}
	// A declaration that names a table outranks one that does not: ToTable is the
	// only place the physical name appears.
	if e.framework == "" || (e.table == "" && table != "") {
		if table != "" {
			e.table = table
		}
		if e.framework == "" {
			e.framework = framework
		}
	}
	s.entities[name] = e
}

func (s *storageScaffold) merge(o storageScaffold) {
	s.decls = append(s.decls, o.decls...)
	for k, v := range o.entities {
		s.noteEntity(k, v.framework, v.table, v.fromDir)
	}
}

// isPlausibleEntityName rejects the generic parameters that appear in the same
// syntactic position as an entity type. `DbSet<T>` inside a generic base class,
// or `IEntityTypeConfiguration<TEntity>`, names a type PARAMETER, not a table —
// and emitting one would create a storage fact called `T`.
func isPlausibleEntityName(name string) bool {
	if len(name) < 2 || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	// The .NET convention for a type parameter is a leading T followed by another
	// capital, or a bare single letter (already excluded by the length check).
	if name[0] == 'T' && name[1] >= 'A' && name[1] <= 'Z' {
		return false
	}
	return !csharpNoise[name]
}

var (
	dbSetDecl    = regexp.MustCompile(`\bDbSet<\s*([A-Za-z_][\w.]*)\s*>`)
	entityConfig = regexp.MustCompile(`\bIEntityTypeConfiguration<\s*([A-Za-z_][\w.]*)\s*>`)
	toTableCall  = regexp.MustCompile(`\.ToTable\(\s*"([^"]+)"`)
	mongoColl    = regexp.MustCompile(`\bIMongoCollection<\s*([A-Za-z_][\w.]*)\s*>`)
	// Dapper's generic query methods name the row type they materialise.
	dapperQuery = regexp.MustCompile(`\b(?:Query|QueryAsync|QueryFirst|QueryFirstAsync|` +
		`QueryFirstOrDefault|QueryFirstOrDefaultAsync|QuerySingle|QuerySingleAsync|` +
		`QuerySingleOrDefault|QuerySingleOrDefaultAsync)<\s*([A-Za-z_][\w.]*)\s*>`)
)

// noteStorage records what one type declaration says about persistence.
//
// Called for every class, because whether a type is a DbContext depends on a base
// list that may name a class declared in another file — the same reason controller
// routing is decided after the walk rather than during it.
func (w *astWalker) noteStorage(declText, canonical string, bases []string, line int) {
	// The BASE LIST, never the declaration text. Matching "DbContext" as a
	// substring caught three things that are not one: a class merely NAMED
	// MigrateDbContextExtensions, a generic CONSTRAINT (`where TContext :
	// DbContext`), and a service whose type parameter is a context. eShop reported
	// 8 contexts where it has 4.
	isContext, isMigration, entityCfg := false, false, ""
	for _, b := range bases {
		switch {
		case strings.HasSuffix(b, "DbContext"):
			isContext = true // DbContext, IdentityDbContext<T>, ApiAuthorizationDbContext<T>
		case b == "Migration":
			isMigration = true
		case strings.HasPrefix(b, "IEntityTypeConfiguration"):
			// baseTypes strips generic arguments, so the entity is read back out of
			// the declaration text. The base list still decides WHETHER this is a
			// configuration; only the type argument comes from the text.
			head := declText
			if i := strings.IndexByte(declText, '{'); i > 0 {
				head = declText[:i]
			}
			if m := entityConfig.FindStringSubmatch(head); m != nil {
				entityCfg = m[1]
			}
		}
	}

	switch {
	case isContext:
		d := storageDecl{name: canonical, dir: w.dir, file: w.relFile, line: line, kind: "context"}
		for _, m := range dbSetDecl.FindAllStringSubmatch(declText, -1) {
			name := shortType(m[1])
			if isPlausibleEntityName(name) {
				d.entities = append(d.entities, name)
				w.scaffold.storage.noteEntity(name, "efcore", "", w.dir)
			}
		}
		d.entities = dedupeSorted(d.entities)
		w.scaffold.storage.decls = append(w.scaffold.storage.decls, d)

	case isMigration:
		// An EF Core migration is a schema change, not a table. Recording it is what
		// lets a reviewer see that a directory holds migrations rather than logic.
		w.scaffold.storage.decls = append(w.scaffold.storage.decls, storageDecl{
			name: canonical, dir: w.dir, file: w.relFile, line: line, kind: "migration",
		})
	}

	if entityCfg != "" {
		table := ""
		if t := toTableCall.FindStringSubmatch(declText); t != nil {
			table = t[1]
		}
		w.scaffold.storage.noteEntity(shortType(entityCfg), "efcore", table, w.dir)
	}
	for _, m := range mongoColl.FindAllStringSubmatch(declText, -1) {
		w.scaffold.storage.noteEntity(shortType(m[1]), "mongo", "", w.dir)
	}
	for _, m := range dapperQuery.FindAllStringSubmatch(declText, -1) {
		w.scaffold.storage.noteEntity(shortType(m[1]), "dapper", "", w.dir)
	}
}

// composeStorageFacts materialises the evidence once every file is in.
//
// An entity fact is named after the SYMBOL that declares the type, not after the
// context that mentioned it — a `DbSet<CatalogItem>` in Infrastructure/ refers to
// a class declared in Model/, and naming the fact for the mentioning directory
// would mint a node that matches nothing. Types the repository does not declare
// (a BCL or framework row type) are dropped for the same reason.
func composeStorageFacts(allFacts []facts.Fact, sc storageScaffold) []facts.Fact {
	// Canonical symbol name by simple name, and a guard against ambiguity: two
	// classes sharing a short name cannot be told apart here, and picking one would
	// attribute a table to the wrong subsystem.
	candidates := map[string][]string{}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol || !isTypeKind(f.PropAny("symbol_kind")) {
			continue
		}
		short := f.Name
		if dot := strings.LastIndex(short, "."); dot >= 0 {
			short = short[dot+1:]
		}
		candidates[short] = append(candidates[short], f.Name)
	}
	resolve := func(name, fromDir string) (string, bool) {
		return nearestDeclaration(candidates[name], fromDir)
	}

	var out []facts.Fact
	for _, d := range sc.decls {
		props := map[string]any{
			"language":     "csharp",
			"storage_kind": d.kind,
			"framework":    "efcore",
		}
		rels := []facts.Relation{{Kind: facts.RelDeclares, Target: d.dir}}
		for _, e := range d.entities {
			if target, ok := resolve(e, d.dir); ok {
				rels = append(rels, facts.Relation{Kind: facts.RelDependsOn, Target: target})
			}
		}
		if len(d.entities) > 0 {
			props["entities"] = strings.Join(d.entities, ",")
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindStorage,
			Name:      d.name,
			File:      d.file,
			Line:      d.line,
			Props:     props,
			Relations: rels,
		})
	}

	names := make([]string, 0, len(sc.entities))
	for name := range sc.entities {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ev := sc.entities[name]
		canonical, ok := resolve(name, ev.fromDir)
		if !ok {
			continue
		}
		props := map[string]any{
			"language":     "csharp",
			"storage_kind": "entity",
			"framework":    ev.framework,
		}
		if ev.table != "" {
			props["table"] = ev.table
		}
		file, line := "", 0
		for i := range allFacts {
			if allFacts[i].Name == canonical && allFacts[i].Kind == facts.KindSymbol {
				file, line = allFacts[i].File, allFacts[i].Line
				break
			}
		}
		out = append(out, facts.Fact{
			Kind:  facts.KindStorage,
			Name:  canonical,
			File:  file,
			Line:  line,
			Props: props,
		})
	}
	return out
}

// nearestDeclaration picks the candidate declaration closest to the directory that
// named it, measured in shared leading path segments.
//
// A short name declared once resolves outright. Declared several times, the one in
// the same project wins — a DbContext in src/Catalog.API/Infrastructure naming
// CatalogItem means the class in src/Catalog.API/Model, not the identically-named
// DTOs in src/ClientApp and src/WebAppComponents. This is the storage counterpart
// of resolution step 1, which prefers a declaration in the reference's own
// namespace. A tie is still dropped: attributing a table to the wrong subsystem is
// worse than attributing none.
func nearestDeclaration(cands []string, fromDir string) (string, bool) {
	switch len(cands) {
	case 0:
		return "", false
	case 1:
		return cands[0], true
	}
	want := strings.Split(fromDir, "/")
	best, bestScore, tied := "", -1, false
	for _, c := range cands {
		dir := c
		if dot := strings.LastIndex(c, "."); dot >= 0 {
			dir = c[:dot]
		}
		got := strings.Split(dir, "/")
		n := 0
		for n < len(want) && n < len(got) && want[n] == got[n] {
			n++
		}
		switch {
		case n > bestScore:
			best, bestScore, tied = c, n, false
		case n == bestScore:
			tied = true
		}
	}
	if tied || bestScore == 0 {
		return "", false
	}
	return best, true
}
