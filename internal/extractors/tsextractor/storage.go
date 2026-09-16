package tsextractor

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// TypeScript storage modelling — TypeORM, Drizzle and Prisma.
//
// tsextractor emitted ZERO storage facts before this, making TypeScript the only backend
// language in enola that models no tables: Go, Java, Kotlin, Python and Ruby all do. A
// database-backed Node service therefore reported no storage at all in explore,
// impact_analysis, llm_context and --explain's data surface.
//
// Facts are named after the DECLARATION (dir + "." + name) with the physical table in a
// `table` prop — the dominant convention (Kotlin's Room detectRoomStorage, Java's JPA
// detectJpaStorage, Ruby's ActiveRecord models). The class/const still emits its own
// symbol fact; the storage fact is a companion, not a replacement.

// ormDeps are the package.json dependencies that switch each ORM on. Detection is gated
// on the dependency, so a class coincidentally decorated `@Entity`, or a helper named
// `pgTable`, models nothing in a repo that does not use the ORM.
const (
	depTypeORM = "typeorm"
	depDrizzle = "drizzle-orm"
	depPrisma  = "@prisma/client"
)

// drizzleTableFns are Drizzle's per-dialect table constructors.
var drizzleTableFns = map[string]bool{
	"pgTable": true, "sqliteTable": true, "mysqlTable": true,
}

// detectORMs reports which ORMs the repo declares, reusing the same package.json
// primitive (and the same tsRoot + repo-root fallback) that Vue/Nuxt detection uses.
func detectORMs(repoPath string) (typeorm, drizzle, prisma bool) {
	tsRoot, _ := findTSRoot(repoPath)
	has := func(pkg string) bool {
		return hasPkgDependency(tsRoot, pkg) || (tsRoot != repoPath && hasPkgDependency(repoPath, pkg))
	}
	return has(depTypeORM), has(depDrizzle), has(depPrisma)
}

// typeORMEntityStorage emits a storage fact for a class decorated `@Entity()` or
// `@Entity("users")`.
//
// tsextractor read NO decorators before this — there was not one occurrence in the whole
// package — so this is the extractor's first decorator support. Decorator nodes hang off
// the class_declaration itself, which is why the class branch is the hook.
func typeORMEntityStorage(kinds *tsutil.KindTable, node *sitter.Node, src []byte, className, relFile, dir string, line int) *facts.Fact {
	name, arg, ok := classDecorator(kinds, node, src, "Entity")
	if !ok || name == "" {
		return nil
	}
	table := arg
	if table == "" {
		// `@Entity()` with no argument: TypeORM defaults the table to the class name.
		table = className
	}
	return &facts.Fact{
		Kind: facts.KindStorage,
		Name: dir + "." + className,
		File: relFile,
		Line: line,
		Props: map[string]any{
			"storage_kind": "entity",
			"language":     "typescript",
			"framework":    "typeorm",
			"table":        table,
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
	}
}

// classDecorator finds a decorator by name on a class node and returns its first string
// argument, if any. Handles both `@Entity` and `@Entity("users")` shapes.
//
// The decorator hangs off DIFFERENT nodes depending on export:
//
//	class_declaration        →  decorator, class, type_identifier, class_body   (bare class)
//	export_statement         →  decorator, export, class_declaration            (exported class)
//
// so a search of the class node alone finds nothing for `@Entity() export class X` — which
// is the shape essentially every real entity uses. Look at the class node, then at its
// parent.
func classDecorator(kinds *tsutil.KindTable, node *sitter.Node, src []byte, want string) (name, arg string, ok bool) {
	if n, a, found := decoratorIn(kinds, node, src, want); found {
		return n, a, true
	}
	if parent := node.Parent(); parent != nil && kindOf(kinds, parent) == "export_statement" {
		return decoratorIn(kinds, parent, src, want)
	}
	return "", "", false
}

// decoratorIn scans one node's immediate children for a named decorator and returns
// its first string argument. A thin wrapper over decoratorArgsIn, which owns the
// scan — @Controller({path: "…"}) needs the arguments NODE rather than its first
// string, and the two must not drift into separate traversals.
func decoratorIn(kinds *tsutil.KindTable, node *sitter.Node, src []byte, want string) (name, arg string, ok bool) {
	args, found := decoratorArgsIn(kinds, node, src, want)
	if !found {
		return "", "", false
	}
	return want, firstStringArg(kinds, args, src), true
}

// decoratorArgsIn scans one node's immediate children for a named decorator and
// returns its call arguments. A bare decorator (@Entity, @Get) yields a nil args
// node with ok=true — present, but carrying nothing.
func decoratorArgsIn(kinds *tsutil.KindTable, node *sitter.Node, src []byte, want string) (args *sitter.Node, ok bool) {
	if node == nil {
		return nil, false
	}
	for i := range node.ChildCount() {
		child := node.Child(i)
		if kindOf(kinds, child) != "decorator" {
			continue
		}
		// The decorator wraps either a bare identifier (@Entity) or a call (@Entity(...)).
		for j := range child.ChildCount() {
			inner := child.Child(j)
			switch kindOf(kinds, inner) {
			case "identifier":
				if nodeText(inner, src) == want {
					return nil, true
				}
			case "call_expression":
				fn := inner.ChildByFieldName("function")
				if fn == nil || nodeText(fn, src) != want {
					continue
				}
				return inner.ChildByFieldName("arguments"), true
			}
		}
	}
	return nil, false
}

// drizzleTableStorage emits a storage fact for `export const orders = pgTable("orders", …)`.
// The call_expression is already in hand at the lexical_declaration branch — it simply
// fails the isComponentWrapper check today.
func drizzleTableStorage(kinds *tsutil.KindTable, call *sitter.Node, src []byte, constName, relFile, dir string, line int) *facts.Fact {
	fn := call.ChildByFieldName("function")
	if fn == nil || !drizzleTableFns[nodeText(fn, src)] {
		return nil
	}
	table := firstStringArg(kinds, call.ChildByFieldName("arguments"), src)
	if table == "" {
		table = constName
	}
	return &facts.Fact{
		Kind: facts.KindStorage,
		Name: dir + "." + constName,
		File: relFile,
		Line: line,
		Props: map[string]any{
			"storage_kind": "entity",
			"language":     "typescript",
			"framework":    "drizzle",
			"table":        table,
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
	}
}

// firstStringArg returns the text of the first string literal in an argument list.
func firstStringArg(kinds *tsutil.KindTable, args *sitter.Node, src []byte) string {
	if args == nil {
		return ""
	}
	for i := range args.ChildCount() {
		a := args.Child(i)
		if kindOf(kinds, a) != "string" {
			continue
		}
		return strings.Trim(nodeText(a, src), `"'`+"`")
	}
	return ""
}

// prismaModel matches a `model User {` block header in a Prisma schema.
var prismaModel = regexp.MustCompile(`(?m)^\s*model\s+(\w+)\s*\{`)

// extractPrismaStorage reads Prisma schemas OFF-GLOB and emits one storage fact per
// model. Schemas live below package roots in monorepos, so checking only
// <repo>/prisma/schema.prisma silently misses the common packages/shared/prisma shape.
//
// schema.prisma is a separate DSL, not TypeScript, so tree-sitter never sees it. That is
// not an obstacle: the extractor ALREADY reads non-TS files from disk this way —
// package.json for framework detection and tsconfig.json for path aliases. This is the
// same mechanism, plus a block-header match. (`datasource`/`generator` blocks are not
// models and are ignored by construction.)
func extractPrismaStorage(repoPath string) []facts.Fact {
	var out []facts.Fact
	seen := map[string]bool{}
	_ = filepath.WalkDir(repoPath, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "dist", "build", ".next", ".enola":
				if abs != repoPath {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.ToLower(filepath.Ext(abs)) != ".prisma" {
			return nil
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil
		}
		rawRel, err := filepath.Rel(repoPath, abs)
		if err != nil {
			return nil
		}
		relFile := factpath.Slash(rawRel)
		dir := factpath.Dir(relFile)
		src := string(data)
		for _, m := range prismaModel.FindAllStringSubmatchIndex(src, -1) {
			model := src[m[2]:m[3]]
			name := dir + "." + model
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, facts.Fact{
				Kind: facts.KindStorage,
				Name: name,
				File: relFile,
				Line: strings.Count(src[:m[0]], "\n") + 1,
				Props: map[string]any{
					"storage_kind": "entity",
					"language":     "typescript",
					"framework":    "prisma",
					"table":        model,
				},
				Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
			})
		}
		return nil
	})
	return out
}

// ormFlags carries the per-repo ORM detection results into per-file extraction. The
// extractor runs files in parallel, so these are read-only once computed — as the
// existing isNextJS/isVue/isNuxt flags are.
type ormFlags struct {
	typeORM bool
	drizzle bool
}
