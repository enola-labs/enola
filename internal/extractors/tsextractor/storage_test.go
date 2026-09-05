package tsextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// extractWithPkg writes package.json (the ORM DETECTION input, mirroring how
// python_flask_sample's requirements.txt drives Flask detection) but keeps it out of
// the file list handed to Extract, since it is not a TS source file.
func extractWithPkg(t *testing.T, pkgJSON string, files map[string]string) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	var relFiles []string
	for rel, content := range files {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if filepath.Ext(rel) == ".ts" || filepath.Ext(rel) == ".tsx" {
			relFiles = append(relFiles, rel)
		}
	}
	ext := New()
	got, err := ext.Extract(context.Background(), dir, relFiles)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return got
}

func storageFacts(ff []facts.Fact) []facts.Fact {
	var out []facts.Fact
	for _, f := range ff {
		if f.Kind == facts.KindStorage {
			out = append(out, f)
		}
	}
	return out
}

func findStorage(ff []facts.Fact, name string) (facts.Fact, bool) {
	for _, f := range storageFacts(ff) {
		if f.Name == name {
			return f, true
		}
	}
	return facts.Fact{}, false
}

const ormPkgJSON = `{"dependencies":{"typeorm":"^0.3.20","drizzle-orm":"^0.30.0","@prisma/client":"^5.12.0"}}`

// TestStorage_TypeORMEntity — a @Entity() decorated class is a table. tsextractor read
// NO decorators at all before this (zero occurrences in the package), so this is new
// AST capability, not a tweak.
func TestStorage_TypeORMEntity(t *testing.T) {
	ff := extractWithPkg(t, ormPkgJSON, map[string]string{
		"src/entity.ts": `import { Entity, Column } from "typeorm";

@Entity("users")
export class User {
  @Column()
  email: string;
}

@Entity()
export class Session {
  id: number;
}

export class UserPresenter {
  present(u: User): string { return u.email; }
}
`,
	})

	// The decorator's argument names the physical table.
	f, ok := findStorage(ff, "src.User")
	if !ok {
		t.Fatal("@Entity class User emitted no storage fact")
	}
	if f.Props["storage_kind"] != "entity" || f.Props["framework"] != "typeorm" || f.Props["language"] != "typescript" {
		t.Errorf("props = %v", f.Props)
	}
	if f.Props["table"] != "users" {
		t.Errorf(`table = %v, want "users" (from @Entity("users"))`, f.Props["table"])
	}

	// No argument: the table defaults to the class name.
	s, ok := findStorage(ff, "src.Session")
	if !ok {
		t.Fatal("@Entity() with no argument emitted no storage fact")
	}
	if s.Props["table"] != "Session" {
		t.Errorf(`table = %v, want "Session" (defaulted from the class name)`, s.Props["table"])
	}

	// An ordinary class is not storage.
	if _, ok := findStorage(ff, "src.UserPresenter"); ok {
		t.Error("an undecorated class emitted a storage fact")
	}
}

// TestStorage_DrizzleTable — `export const orders = pgTable("orders", {...})`.
func TestStorage_DrizzleTable(t *testing.T) {
	ff := extractWithPkg(t, ormPkgJSON, map[string]string{
		"src/schema.ts": `import { pgTable, serial, text } from "drizzle-orm/pg-core";

export const orders = pgTable("orders", {
  id: serial("id").primaryKey(),
});

export const MAX_ORDERS = 100;
`,
	})

	f, ok := findStorage(ff, "src.orders")
	if !ok {
		t.Fatal("pgTable const emitted no storage fact")
	}
	if f.Props["framework"] != "drizzle" || f.Props["table"] != "orders" {
		t.Errorf("props = %v", f.Props)
	}
	if _, ok := findStorage(ff, "src.MAX_ORDERS"); ok {
		t.Error("an ordinary exported const emitted a storage fact")
	}
}

// TestStorage_PrismaModels — schema.prisma is a separate DSL, not TypeScript, so it is
// read off-glob exactly as package.json/tsconfig.json already are.
func TestStorage_PrismaModels(t *testing.T) {
	ff := extractWithPkg(t, ormPkgJSON, map[string]string{
		"prisma/schema.prisma": `datasource db {
  provider = "postgresql"
}

model Post {
  id    Int    @id
  title String
}

model Comment {
  id   Int    @id
  body String
}
`,
		"src/noop.ts": `export const x = 1;
`,
	})

	for _, name := range []string{"prisma.Post", "prisma.Comment"} {
		f, ok := findStorage(ff, name)
		if !ok {
			t.Errorf("prisma model %q emitted no storage fact", name)
			continue
		}
		if f.Props["framework"] != "prisma" || f.Props["storage_kind"] != "entity" {
			t.Errorf("%s props = %v", name, f.Props)
		}
	}
	// `datasource db { ... }` is not a model.
	if _, ok := findStorage(ff, "prisma.db"); ok {
		t.Error("a datasource block was emitted as storage")
	}
}

func TestStorage_PrismaModelsInMonorepoPackage(t *testing.T) {
	ff := extractWithPkg(t, `{"workspaces":["packages/*"]}`, map[string]string{
		"packages/shared/prisma/schema.prisma": "model Trace {\n id String @id\n}\n",
	})
	f, ok := findStorage(ff, "packages/shared/prisma.Trace")
	if !ok {
		t.Fatal("nested monorepo Prisma schema emitted no storage fact")
	}
	if f.File != "packages/shared/prisma/schema.prisma" || f.Props["table"] != "Trace" {
		t.Fatalf("fact = %#v", f)
	}
}

// TestStorage_NoORMDependencyEmitsNothing is the guard against over-firing. Detection is
// gated on the package.json dependency, so a class decorated @Entity in a repo that does
// not use TypeORM — or a helper coincidentally named pgTable — models no storage.
func TestStorage_NoORMDependencyEmitsNothing(t *testing.T) {
	ff := extractWithPkg(t, `{"dependencies":{"react":"^18.0.0"}}`, map[string]string{
		"src/entity.ts": `@Entity("users")
export class User { id: number; }

export const orders = pgTable("orders", {});
`,
	})

	if got := storageFacts(ff); len(got) != 0 {
		t.Errorf("a repo with no ORM dependency emitted %d storage facts: %+v", len(got), got)
	}
}

// TestIODirect_ORMCallSeedsPerformsIO is the half that moves perf findings.
//
// TS already propagates io_direct into performs_io transitively (computeTSPerformsIO),
// but ORM calls never seeded it — so a repository wrapper around prisma.post.findMany()
// was NOT performs_io, and a per-iteration call to that wrapper was invisible to the
// performance analyzer. This is new/26's own second bullet, and it is the reachable
// evidence source (perf's byName[target].PerformsIO branch), unlike the storage-fact
// branch, which keys on call targets and is dead in every language.
func TestIODirect_ORMCallSeedsPerformsIO(t *testing.T) {
	ff := extractWithPkg(t, ormPkgJSON, map[string]string{
		"src/repo.ts": `import { PrismaClient } from "@prisma/client";

const prisma = new PrismaClient();

export async function loadPostsFor(authorId: number) {
  return prisma.post.findMany({ where: { authorId } });
}

export async function loadFeed(ids: number[]) {
  const out = [];
  for (const id of ids) {
    out.push(await loadPostsFor(id));
  }
  return out;
}

export function findIndexOfPost(posts: { id: number }[], id: number) {
  const seen = [];
  for (const p of posts) { seen.push(p.id === id); }
  return seen;
}
`,
	})

	wrapper, ok := findFact(ff, "src.loadPostsFor")
	if !ok {
		t.Fatal("wrapper symbol not extracted")
	}
	if io, _ := wrapper.Props["io_direct"].(bool); !io {
		t.Error("a function whose body calls prisma.post.findMany() must be io_direct — " +
			"without it, a per-iteration call to this wrapper is invisible to perf")
	}
	if pio, _ := wrapper.Props["performs_io"].(bool); !pio {
		t.Error("the wrapper must be performs_io")
	}

	// The pure in-memory helper must stay clean. Frontend TS reuses the CamelCase I/O
	// verbs for ordinary helpers, and everything is exported, so a false positive here
	// floods the high-severity bucket — the exact reason the TS detector was narrowed.
	helper, _ := findFact(ff, "src.findIndexOfPost")
	if io, _ := helper.Props["io_direct"].(bool); io {
		t.Error("a pure in-memory helper was tagged io_direct — the seeds are over-firing")
	}
	if pio, _ := helper.Props["performs_io"].(bool); pio {
		t.Error("a pure in-memory helper was tagged performs_io")
	}
}
