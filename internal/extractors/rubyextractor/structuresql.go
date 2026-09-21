package rubyextractor

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// structureSQLPath is where Rails keeps the SQL-format schema dump. Its
// presence — not project detection — is what switches this pass on: the file
// is the database's own account of the schema, which the model-derived storage
// facts can only infer.
const structureSQLPath = "db/structure.sql"

// The recognized pg_dump shapes, and nothing else. This is deliberately not a
// SQL grammar: structure.sql is machine-written by pg_dump in a handful of
// stable line shapes, and a line outside them is skipped rather than guessed
// at — a wrong column census would be worse than none, because a declared rule
// verdicts against it.
var (
	reCreateTable = regexp.MustCompile(`^CREATE TABLE (?:IF NOT EXISTS )?(\S+) \($`)
	reAlterTable  = regexp.MustCompile(`^ALTER TABLE (?:ONLY )?(\S+)$`)
	// Single-column foreign keys only: a composite key has no honest
	// column->table pair form, so it is skipped, never split into claims the
	// dump does not make.
	reForeignKey = regexp.MustCompile(`^ADD CONSTRAINT \S+ FOREIGN KEY \(([a-z_][a-z0-9_]*)\) REFERENCES (\S+)\(`)
	reColumnName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
)

// parsedTable is one CREATE TABLE's census: the sorted column names and the
// sorted single-column foreign keys as "column->reftable" pairs.
type parsedTable struct {
	line    int
	columns []string
	fks     []string
}

// applyStructureSQL parses db/structure.sql when present and returns storage
// facts for the tables it declares.
func applyStructureSQL(repoPath string, allFacts []facts.Fact) []facts.Fact {
	data, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(structureSQLPath)))
	if err != nil {
		return nil
	}
	return foldTables(parseStructureSQL(string(data)), allFacts, structureSQLPath)
}

// foldTables turns a parsed dump into storage facts, and is what both dump
// formats reach: the census a rule reads must not be able to tell which format
// declared it. A table an ActiveRecord/Sequel model already claims (a storage
// fact whose table prop names it) gets its census ADDED to the model's fact
// instead of a second fact — one table, one storage identity, whichever pass
// saw it first. Model facts in allFacts are mutated in place for exactly that
// case.
func foldTables(tables map[string]*parsedTable, allFacts []facts.Fact, dumpPath string) []facts.Fact {
	if len(tables) == 0 {
		return nil
	}

	claimed := map[string][]int{}
	for i, f := range allFacts {
		if f.Kind != facts.KindStorage || f.Props == nil {
			continue
		}
		if table, _ := f.PropAny("table").(string); table != "" {
			claimed[table] = append(claimed[table], i)
		}
	}

	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []facts.Fact
	for _, name := range names {
		t := tables[name]
		columns := strings.Join(t.columns, " ")
		fks := strings.Join(t.fks, " ")
		if owners := claimed[name]; len(owners) > 0 {
			for _, i := range owners {
				setCensus(allFacts[i].Props, columns, fks)
			}
			continue
		}
		// A table is a database object whichever dump declared it, so the
		// language stays sql even when the declaring file is Ruby: the two
		// formats must not sort into different buckets for the same table.
		props := map[string]any{
			"storage_kind": "table",
			"table":        name,
			"language":     "sql",
		}
		setCensus(props, columns, fks)
		out = append(out, facts.Fact{
			Kind:  facts.KindStorage,
			Name:  name,
			File:  dumpPath,
			Line:  t.line,
			Props: props,
		})
	}
	return out
}

func setCensus(props map[string]any, columns, fks string) {
	if columns != "" {
		props["columns"] = columns
	}
	if fks != "" {
		props["fk_constraints"] = fks
	}
}

// parseStructureSQL walks the dump line by line: a CREATE TABLE opens a column
// scan that runs to its closing paren, an ALTER TABLE arms the next line to be
// its ADD CONSTRAINT (pg_dump splits the statement across exactly those two
// lines), and everything unrecognized falls through untouched.
func parseStructureSQL(src string) map[string]*parsedTable {
	tables := map[string]*parsedTable{}
	table := func(name string) *parsedTable {
		if tables[name] == nil {
			tables[name] = &parsedTable{}
		}
		return tables[name]
	}

	var inTable *parsedTable
	pendingAlter := ""
	for i, raw := range strings.Split(src, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)

		if inTable != nil {
			if trimmed == ");" {
				sort.Strings(inTable.columns)
				inTable = nil
				continue
			}
			if col, ok := columnName(trimmed); ok {
				inTable.columns = append(inTable.columns, col)
			}
			continue
		}

		if m := reCreateTable.FindStringSubmatch(line); m != nil {
			if name, ok := sqlTableName(m[1]); ok {
				inTable = table(name)
				inTable.line = i + 1
			}
			continue
		}

		if pendingAlter != "" {
			if m := reForeignKey.FindStringSubmatch(trimmed); m != nil {
				if ref, ok := sqlTableName(m[2]); ok {
					t := table(pendingAlter)
					t.fks = append(t.fks, m[1]+"->"+ref)
					sort.Strings(t.fks)
				}
			}
			pendingAlter = ""
			continue
		}
		if m := reAlterTable.FindStringSubmatch(line); m != nil {
			if name, ok := sqlTableName(m[1]); ok {
				pendingAlter = name
			}
			continue
		}
	}
	return tables
}

// columnName reads the first token of a CREATE TABLE body line as a column
// name. pg_dump writes reserved-word columns quoted and everything else as a
// bare lowercase identifier, so an uppercase first token (CONSTRAINT, PRIMARY,
// CHECK…) is a table-level clause and is skipped by failing the identifier
// shape — no keyword list to fall out of date.
func columnName(trimmed string) (string, bool) {
	if trimmed == "" {
		return "", false
	}
	tok := strings.Fields(trimmed)[0]
	tok = strings.TrimSuffix(tok, ",")
	if quoted, ok := strings.CutPrefix(tok, `"`); ok {
		name, closed := strings.CutSuffix(quoted, `"`)
		if !closed || name == "" {
			return "", false
		}
		return name, true
	}
	if !reColumnName.MatchString(tok) {
		return "", false
	}
	return tok, true
}

// sqlTableName normalizes a dump's table reference — schema-qualified,
// possibly quoted — to the bare table name. An empty or still-odd result
// reports failure so the caller skips rather than records a mangled name.
func sqlTableName(ref string) (string, bool) {
	ref = strings.TrimSuffix(ref, ";")
	parts := strings.Split(ref, ".")
	name := strings.Trim(parts[len(parts)-1], `"`)
	if name == "" || strings.ContainsAny(name, `"()`) {
		return "", false
	}
	return name, true
}
